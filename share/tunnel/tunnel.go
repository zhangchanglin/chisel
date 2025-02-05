package tunnel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"sync"
	"time"

	"github.com/armon/go-socks5"
	"github.com/jpillora/chisel/share/cio"
	"github.com/jpillora/chisel/share/cnet"
	"github.com/jpillora/chisel/share/settings"
	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"
)

//Config a Tunnel
type Config struct {
	*cio.Logger
	Inbound   bool
	Outbound  bool
	Socks     bool
	KeepAlive time.Duration
}

//Tunnel represents an SSH tunnel with proxy capabilities.
//Both chisel client and server are Tunnels.
//chisel client has a single set of remotes, whereas
//chisel server has multiple sets of remotes (one set per client).
//Each remote has a 1:1 mapping to a proxy.
//Proxies listen, send data over ssh, and the other end of the ssh connection
//communicates with the endpoint and returns the response.
type Tunnel struct {
	Config
	//ssh connection
	activeConnMut  sync.RWMutex
	activatingConn waitGroup
	activeConn     ssh.Conn
	//proxies
	proxyCount int
	//internals
	connStats   cnet.ConnCount
	socksServer *socks5.Server
	RemoteIP  string
}

//New Tunnel from the given Config
func New(c Config) *Tunnel {
	c.Logger = c.Logger.Fork("tun")
	t := &Tunnel{
		Config: c,
	}
	t.activatingConn.Add(1)
	//setup socks server (not listening on any port!)
	extra := ""
	if c.Socks {
		sl := log.New(ioutil.Discard, "", 0)
		if t.Logger.Debug {
			sl = log.New(os.Stdout, "[socks]", log.Ldate|log.Ltime)
		}
		t.socksServer, _ = socks5.New(&socks5.Config{Logger: sl})
		extra += " (SOCKS enabled)"
	}
	t.Debugf("Created%s", extra)
	return t
}

//BindSSH provides an active SSH for use for tunnelling
func (t *Tunnel) BindSSH(ctx context.Context, c ssh.Conn, reqs <-chan *ssh.Request, chans <-chan ssh.NewChannel) error {
	//link ctx to ssh-conn
	go func() {
		<-ctx.Done()
		if c.Close() == nil {
			t.Debugf("SSH cancelled")
		}
		t.activatingConn.DoneAll()
	}()
	//mark active and unblock
	t.activeConnMut.Lock()
	if t.activeConn != nil {
		panic("double bind ssh")
	}
	t.activeConn = c
	t.activeConnMut.Unlock()
	t.activatingConn.Done()
	//optional keepalive loop against this connection
	if t.Config.KeepAlive > 0 {
		go t.keepAliveLoop(c)
	}
	//block until closed
	go t.handleSSHRequests(reqs)
	go t.handleSSHChannels(chans)
	msg := fmt.Sprintf("[LocalAddr:%s]=>[RemoteAddr:%s]", c.LocalAddr(), c.RemoteAddr())
	t.Debugf("%s, SSH connected", msg)
	err := c.Wait()
	t.Debugf("%s,SSH disconnected", msg)
	//mark inactive and block
	t.activatingConn.Add(1)
	t.activeConnMut.Lock()
	t.activeConn = nil
	t.activeConnMut.Unlock()
	return err
}

//getSSH blocks while connecting
func (t *Tunnel) getSSH(ctx context.Context) ssh.Conn {
	//cancelled already?
	if isDone(ctx) {
		return nil
	}
	t.activeConnMut.RLock()
	c := t.activeConn
	t.activeConnMut.RUnlock()
	//connected already?
	if c != nil {
		return c
	}
	//connecting...
	select {
	case <-ctx.Done(): //cancelled
		return nil
	case <-time.After(settings.EnvDuration("SSH_WAIT", 35*time.Second)):
		return nil //a bit longer than ssh timeout
	case <-t.activatingConnWait():
		t.activeConnMut.RLock()
		c := t.activeConn
		t.activeConnMut.RUnlock()
		return c
	}
}

func (t *Tunnel) activatingConnWait() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		t.activatingConn.Wait()
		close(ch)
	}()
	return ch
}

//BindRemotes converts the given remotes into proxies, and blocks
//until the caller cancels the context or there is a proxy error.
func (t *Tunnel) BindRemotes(ctx context.Context, remotes []*settings.Remote) error {
	if len(remotes) == 0 {
		return errors.New("no remotes")
	}
	if !t.Inbound {
		return errors.New("inbound connections blocked")
	}
	proxies := make([]*Proxy, len(remotes))
	for i, remote := range remotes {
		p, err := NewProxy(t.Logger, t, t.proxyCount, remote)
		if err != nil {
			return err
		}
		proxies[i] = p
		t.proxyCount++
	}
	//TODO: handle tunnel close
	eg, ctx := errgroup.WithContext(ctx)
	var msg string
	for _, proxy := range proxies {
		msg = fmt.Sprintf("[LocalAddr:%s]=>[RemoteAddr:%s]", proxy.tcp.Addr(), proxy.remote.Remote())
		p := proxy
		eg.Go(func() error {
			return p.Run(ctx)
		})
	}
	t.Debugf("%s,Bound proxies", msg)
	err := eg.Wait()
	t.Debugf("%s,Unbound proxies", msg)
	return err
}

func (t *Tunnel) keepAliveLoop(sshConn ssh.Conn) {
	msg := fmt.Sprintf("[LocalAddr:%s]=>[RemoteAddr:%s]", sshConn.LocalAddr(), sshConn.RemoteAddr())
	defer func() {
		//close ssh connection on abnormal ping
		t.Debugf("%s,close ssh connection on abnormal ping", msg)
		sshConn.Close()
	}()
	//ping forever
	for {
		time.Sleep(t.Config.KeepAlive)
		select {
		case <-time.After(t.Config.KeepAlive):
			return
		case err := <-t.KeepAliveChan(sshConn):
			if err != nil {
				return
			}
		}
	}
}

func (t *Tunnel) KeepAliveChan(sshConn ssh.Conn) <-chan error {
	msg := fmt.Sprintf("[LocalAddr:%s]=>[RemoteAddr:%s]", sshConn.LocalAddr(), sshConn.RemoteAddr())
	ch := make(chan error)
	go func() {
		defer close(ch)
		_, b, err := sshConn.SendRequest("ping", true, nil)
		if err != nil {
			t.Debugf("%s ping error,err=%s", msg, err)
			ch <- err
		}
		if len(b) > 0 && !bytes.Equal(b, []byte("pong")) {
			// t.Debugf("strange ping response")
			t.Debugf("%s strange ping response", msg)
			ch <- fmt.Errorf("strange ping response")
		}
	}()
	return ch
}

// Close ssh connection
func (t *Tunnel) Close(ctx context.Context) error {
	sshConn := t.getSSH(ctx)
	if sshConn == nil {
		t.Debugf("No ssh-conn to close")
		return nil
	}
	return sshConn.Close()
}

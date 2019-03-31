package localtunnel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// Listener implements a net.Listener using localtunnel.me
type Listener struct {
	log      Logger
	remote   string
	url      string
	context  context.Context
	mErr     sync.Mutex
	err      error
	cancel   func()
	nConns   counter
	incoming chan net.Conn
	done     sync.WaitGroup
}

// Listen creates a *Listener that gets incoming connections from localtunnel.me
func Listen(options Options) (*Listener, error) {
	options.setDefaults()
	ctx, cancel := context.WithCancel(context.Background())
	l := &Listener{
		log:     options.Log,
		context: ctx,
		cancel:  cancel,
	}

	// Create a setup URL
	setupURL := options.BaseURL + "/"
	if options.Subdomain != "" {
		setupURL += options.Subdomain
	} else {
		setupURL += "?new"
	}

	// Call the setupURL
	l.log.Println("registering tunnel:", setupURL)
	client := http.Client{Timeout: 30 * time.Second}
	res, err := client.Get(setupURL)
	if err != nil {
		return nil, fmt.Errorf("failed to setup tunnel, error: %s", err)
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("internal server error, statusCode: %d", res.StatusCode)
	}
	body, err := readAtmost(res.Body, 4*1024)
	if err != nil {
		return nil, fmt.Errorf("failed to read server response, error: %s", err)
	}
	var reply struct {
		ID           string `json:"id"`
		Port         int    `json:"port"`
		MaxConnCount int    `json:"max_conn_count"`
		URL          string `json:"url"`
	}
	err = json.Unmarshal(body, &reply)
	if err != nil {
		return nil, fmt.Errorf("failed to parse server response, error: %s", err)
	}
	l.log.Println("registered tunnel: ", reply)

	// Set some sanity values
	if reply.MaxConnCount == 0 {
		reply.MaxConnCount = 1
	}
	if reply.MaxConnCount > options.MaxConnections {
		reply.MaxConnCount = options.MaxConnections
	}

	// Extract remote host
	u, _ := url.Parse(options.BaseURL)
	l.remote = fmt.Sprintf("%s:%d", u.Hostname(), reply.Port)

	// Set remote URL
	l.url = reply.URL

	// Start listening for new connections
	errChan := make(chan error, reply.MaxConnCount)
	l.incoming = make(chan net.Conn, reply.MaxConnCount)
	l.done.Add(reply.MaxConnCount)
	for i := 0; i < reply.MaxConnCount; i++ {
		go func() {
			c, err := l.connect()
			errChan <- err
			if err == nil {
				if err := l.handle(c); err != nil {
					l.abort(err)
				}
				l.done.Done()
			}
		}()
	}

	ret := make(chan error)
	go func() {
		notify := false
		for i := 0; i < reply.MaxConnCount; i++ {
			if err := <-errChan; err != nil {
				l.log.Println("while waiting for proxy, receive error: ", err)
				continue
			}
			if !notify {
				ret <- nil
				notify = true
			}
		}
		if !notify {
			ret <- fmt.Errorf("fail for %d connections", reply.MaxConnCount)
		}
	}()

	return l, <-ret
}

// Accept returns the next incoming connection
func (l *Listener) Accept() (net.Conn, error) {
	select {
	case <-l.context.Done():
		return nil, l.err
	case c := <-l.incoming:
		if c == nil {
			return nil, l.err
		}
		return c, nil
	}
}

func (l *Listener) connect() (c net.Conn , err error) {
	// Dial with Context
	var d net.Dialer
	for i := 0; i < 3; i++ {
		time.Sleep(time.Duration(i*i) * 3 * time.Second)
		c, err = d.DialContext(l.context, "tcp", l.remote)
		if err == nil {
			l.log.Println("success open connection to ", l.remote)
			break
		}
		if l.context.Err() != nil {
			return nil, l.context.Err()
		}
		l.log.Println(i+1, " attempt connection got: ", err)
	}
	if err != nil {
		return nil, err
	}
	l.nConns.Add(1)

	return c, nil
}

func (l *Listener) handle(c net.Conn) error {
	var n int
	var err error
	var b [1]byte

	// Ensure that we close the connection if we not done reading before
	// context.Done()
	doneReading := make(chan struct{})
	go func() {
		select {
		case <-doneReading:
			return
		case <-l.context.Done():
			c.Close()
		}
	}()

	start := time.Now()
	for n == 0 && err == nil {
		n, err = c.Read(b[:])
	}
	close(doneReading)
	if err != nil {
		// Ignore if it took more than 30s
		if start.Before(time.Now().Add(-30 * time.Second)) {
			l.nConns.Add(-1)
			c.Close()
			return nil
		}
		return err
	}
	l.nConns.Add(-1)

	done := make(chan struct{})
	l.incoming <- &conn{Conn: c, Buffer: b, Done: done}

	fmt.Println("waitint for proxy connection is done")
	// Wait for conn to be closed
	select {
	case <-done:
	case <-l.context.Done():
	}

	// Always close the remote connection
	c.Close()
	return nil
}

// Addr implements net.Addr
type Addr struct {
	URL string
}

// Addr returns an address representation in compliance with net.Listener
func (l *Listener) Addr() net.Addr {
	return Addr{URL: l.url}
}

func (l *Listener) abort(err error) {
	l.mErr.Lock()
	defer l.mErr.Unlock()

	// Only abort once
	if l.err != nil {
		return
	}
	l.err = err

	// Close all tunnels and stop creating new ones
	go func() {
		l.cancel()
		go func() {
			for c := range l.incoming {
				c.Close()
			}
		}()
		l.done.Wait()
		close(l.incoming)
	}()
}

// Close the listener, breaking all connections proxied by this listener
func (l *Listener) Close() error {
	l.abort(ErrListenerClosed)
	l.done.Wait()
	if l.err != ErrListenerClosed {
		return l.err
	}
	return nil
}

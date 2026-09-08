package server

import (
	"net"
	"sync"
)

// limitListener accepts at most a fixed number of connections at a time. Once
// the limit is reached, further connections wait in the kernel's accept queue
// until a live one closes, which keeps memory and file descriptor use bounded
// under a flood of connections.
type limitListener struct {
	net.Listener
	// slots holds one token per connection the listener may keep open.
	slots chan struct{}
}

// newLimitListener wraps inner so that no more than limit connections are open
// at once.
func newLimitListener(inner net.Listener, limit int) net.Listener {
	return &limitListener{
		Listener: inner,
		slots:    make(chan struct{}, limit),
	}
}

// Accept waits for a free slot before accepting the next connection.
func (l *limitListener) Accept() (net.Conn, error) {
	l.slots <- struct{}{}

	conn, err := l.Listener.Accept()
	if err != nil {
		<-l.slots
		return nil, err
	}
	return &limitConn{Conn: conn, release: func() { <-l.slots }}, nil
}

// limitConn returns its slot to the listener once, when it is closed.
type limitConn struct {
	net.Conn
	release func()
	once    sync.Once
}

// Close closes the connection and frees the slot it occupied.
func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}

package tlsutil

import (
	"bufio"
	"crypto/tls"
	"errors"
	"net"
	"sync"
)

type chanListener struct {
	addr   net.Addr
	conns  chan net.Conn
	closed chan struct{}
	once   sync.Once
}

func newChanListener(addr net.Addr) *chanListener {
	return &chanListener{
		addr:   addr,
		conns:  make(chan net.Conn, 64),
		closed: make(chan struct{}),
	}
}

func (l *chanListener) Accept() (net.Conn, error) {
	select {
	case c, ok := <-l.conns:
		if !ok {
			return nil, net.ErrClosed
		}
		return c, nil
	case <-l.closed:
		return nil, net.ErrClosed
	}
}

func (l *chanListener) Close() error {
	l.once.Do(func() {
		close(l.closed)
		close(l.conns)
	})
	return nil
}

func (l *chanListener) Addr() net.Addr {
	return l.addr
}

type peekConn struct {
	net.Conn
	reader *bufio.Reader
}

func (c *peekConn) Read(p []byte) (int, error) {
	return c.reader.Read(p)
}

// SplitListener routes incoming connections based on TLS handshake detection.
// It returns a plain listener and a TLS listener that can be served separately.
func SplitListener(base net.Listener, tlsConfig *tls.Config, allowPlain bool) (net.Listener, net.Listener) {
	plain := newChanListener(base.Addr())
	tlsln := newChanListener(base.Addr())

	go func() {
		defer func() {
			_ = plain.Close()
			_ = tlsln.Close()
		}()
		for {
			conn, err := base.Accept()
			if err != nil {
				if !errors.Is(err, net.ErrClosed) {
					continue
				}
				return
			}
			br := bufio.NewReader(conn)
			b, err := br.Peek(1)
			if err != nil {
				_ = conn.Close()
				continue
			}
			pc := &peekConn{Conn: conn, reader: br}
			if b[0] == 0x16 {
				tlsConn := tls.Server(pc, tlsConfig)
				select {
				case tlsln.conns <- tlsConn:
				case <-tlsln.closed:
					_ = tlsConn.Close()
				}
				continue
			}
			if !allowPlain {
				_ = conn.Close()
				continue
			}
			select {
			case plain.conns <- pc:
			case <-plain.closed:
				_ = conn.Close()
			}
		}
	}()

	return plain, tlsln
}

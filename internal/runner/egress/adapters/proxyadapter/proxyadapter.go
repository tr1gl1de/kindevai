// Package proxyadapter is the local proxy adapter of strict egress (KIN-31, KB KIN-A-22): the only way out of an
// attempt's sandbox. It listens on a Unix socket in the attempt's directory (the sandbox reaches it through a
// forwarder on its own loopback), accepts HTTP CONNECT and absolute-form plain-HTTP requests, checks the
// destination against the profile's allowlist and opens the connection through the upstream proxy only (HTTP
// CONNECT, HTTPS proxy or SOCKS5 with remote DNS: names are never resolved here). The upstream credentials stay in
// the runner process; the sandbox never sees them. Errors are reported to the client as bare status codes.
package proxyadapter

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"

	"github.com/tr1gl1de/kindevai/internal/protocol"
	"github.com/tr1gl1de/kindevai/internal/runner/egress/domain"
)

// DefaultMaxConns bounds the connections of one attempt (memory budget: two goroutines and buffers each).
const DefaultMaxConns = 128

// Server is one attempt's proxy adapter.
type Server struct {
	Upstream protocol.NetworkProxy
	Allow    domain.Allowlist
	RootCAs  *x509.CertPool // HTTPS proxy roots; nil: the system roots
	Timeout  time.Duration  // connecting through the upstream proxy; default 30s
	MaxConns int            // DefaultMaxConns when zero
	Logger   *slog.Logger
}

// Errors of an upstream connection.
var (
	ErrUpstream     = errors.New("upstream proxy unreachable")
	ErrUpstreamAuth = errors.New("upstream proxy refused the credentials")
	ErrRefused      = errors.New("upstream proxy refused the destination")
)

func (s *Server) logger() *slog.Logger {
	if s.Logger == nil {
		return slog.New(slog.DiscardHandler)
	}
	return s.Logger
}

func (s *Server) timeout() time.Duration {
	if s.Timeout <= 0 {
		return 30 * time.Second
	}
	return s.Timeout
}

// Serve accepts connections until ctx ends or l is closed; it closes l and every open connection on return.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	max := s.MaxConns
	if max <= 0 {
		max = DefaultMaxConns
	}
	sem := make(chan struct{}, max)
	var (
		mu    sync.Mutex
		conns = map[net.Conn]struct{}{}
		wg    sync.WaitGroup
	)
	stop := context.AfterFunc(ctx, func() { _ = l.Close() })
	defer stop()
	defer func() {
		_ = l.Close()
		mu.Lock()
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	}()
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				return nil
			}
			return fmt.Errorf("accepting: %w", err)
		}
		select {
		case sem <- struct{}{}:
		default:
			_, _ = io.WriteString(c, "HTTP/1.1 503 Service Unavailable\r\nConnection: close\r\n\r\n")
			_ = c.Close()
			continue
		}
		mu.Lock()
		conns[c] = struct{}{}
		mu.Unlock()
		wg.Go(func() {
			defer func() {
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
				_ = c.Close()
				<-sem
			}()
			s.handle(ctx, c, func(up net.Conn) {
				mu.Lock()
				conns[up] = struct{}{}
				mu.Unlock()
			}, func(up net.Conn) {
				mu.Lock()
				delete(conns, up)
				mu.Unlock()
			})
		})
	}
}

func reply(c net.Conn, status int) {
	_, _ = fmt.Fprintf(c, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: 0\r\n\r\n", status, http.StatusText(status))
}

// maxHeader bounds a request head from the sandbox.
const maxHeader = 64 << 10

func (s *Server) handle(ctx context.Context, c net.Conn, track, untrack func(net.Conn)) {
	_ = c.SetReadDeadline(time.Now().Add(s.timeout()))
	head := &io.LimitedReader{R: c, N: maxHeader} // bounds the head only: lifted once it is read
	br := bufio.NewReaderSize(head, 4096)
	req, err := http.ReadRequest(br)
	if err != nil {
		reply(c, http.StatusBadRequest)
		return
	}
	head.N = 1 << 62
	_ = c.SetReadDeadline(time.Time{})
	var (
		authority string
		connect   = req.Method == http.MethodConnect
	)
	switch {
	case connect:
		authority = req.RequestURI
	case req.URL != nil && req.URL.Scheme == "http" && req.URL.Host != "":
		authority = req.URL.Host
		if req.URL.Port() == "" {
			authority = net.JoinHostPort(req.URL.Hostname(), "80")
		}
	default: // origin-form or https:// in absolute form: not a proxy request this adapter serves
		reply(c, http.StatusBadRequest)
		return
	}
	host, port, err := domain.HostPort(authority)
	if err != nil {
		reply(c, http.StatusBadRequest)
		return
	}
	if domain.Reserved(host) || !s.Allow.Allows(host, port) {
		s.logger().Warn("strict egress: destination not in the allowlist", "host", host, "port", port)
		reply(c, http.StatusForbidden)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, s.timeout())
	up, err := s.Dial(dctx, host, port)
	cancel()
	if err != nil {
		s.logger().Warn("strict egress: upstream connection failed", "host", host, "port", port, "err", err)
		reply(c, http.StatusBadGateway)
		return
	}
	track(up)
	defer func() { untrack(up); _ = up.Close() }()
	if connect {
		if _, err := io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
			return
		}
	} else {
		for h := range req.Header {
			if len(h) > 6 && http.CanonicalHeaderKey(h)[:6] == "Proxy-" {
				req.Header.Del(h)
			}
		}
		req.Close = true // one request per tunnel: the response is relayed as it comes
		if err := req.Write(up); err != nil {
			return
		}
	}
	// Bytes the client sent after the head are in br's buffer.
	splice(c, io.MultiReader(br, c), up)
}

// splice copies both ways until either side is done.
func splice(c net.Conn, fromClient io.Reader, up net.Conn) {
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(up, fromClient); closeWrite(up); done <- struct{}{} }()
	go func() { _, _ = io.Copy(c, up); closeWrite(c); done <- struct{}{} }()
	<-done
	<-done
}

func closeWrite(c net.Conn) {
	if cw, ok := c.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// Dial opens a connection to host:port through the upstream proxy.
func (s *Server) Dial(ctx context.Context, host string, port int) (net.Conn, error) {
	u := s.Upstream
	var d net.Dialer
	addr := net.JoinHostPort(u.Host, strconv.Itoa(u.Port))
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, ErrUpstream
	}
	conn := raw
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	switch u.Scheme {
	case "https":
		tc := tls.Client(raw, &tls.Config{ServerName: u.Host, RootCAs: s.RootCAs, MinVersion: tls.VersionTLS12})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = raw.Close()
			return nil, ErrUpstream
		}
		conn = tc
		fallthrough
	case "http":
		conn, err = httpConnect(conn, u, host, port)
	case "socks5", "socks5h":
		err = socks5Connect(conn, u, host, port)
	default:
		err = fmt.Errorf("unsupported upstream proxy scheme %q", u.Scheme)
	}
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// bufferedConn keeps bytes read past a proxy's CONNECT response.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

func httpConnect(c net.Conn, u protocol.NetworkProxy, host string, port int) (net.Conn, error) {
	target := net.JoinHostPort(host, strconv.Itoa(port))
	req := "CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n"
	if u.Username != "" {
		req += "Proxy-Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte(u.Username+":"+u.Password)) + "\r\n"
	}
	if _, err := io.WriteString(c, req+"\r\n"); err != nil {
		return nil, ErrUpstream
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		return nil, ErrUpstream
	}
	_ = resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusProxyAuthRequired:
		return nil, ErrUpstreamAuth
	case resp.StatusCode/100 != 2:
		return nil, ErrRefused
	}
	return bufferedConn{Conn: c, r: br}, nil
}

// socks5Connect performs the SOCKS5 handshake (RFC 1928, username/password RFC 1929). Names are sent as names:
// the proxy resolves them (remote DNS).
func socks5Connect(c net.Conn, u protocol.NetworkProxy, host string, port int) error {
	methods := []byte{0x00}
	if u.Username != "" {
		methods = []byte{0x02}
	}
	if _, err := c.Write(append([]byte{0x05, 0x01}, methods...)); err != nil { // one method
		return ErrUpstream
	}
	var sel [2]byte
	if _, err := io.ReadFull(c, sel[:]); err != nil || sel[0] != 0x05 {
		return ErrUpstream
	}
	switch sel[1] {
	case 0x00:
		if u.Username != "" {
			return ErrUpstreamAuth // never fall back to sending without the configured credentials
		}
	case 0x02:
		if u.Username == "" || len(u.Username) > 255 || len(u.Password) > 255 {
			return ErrUpstreamAuth
		}
		m := append([]byte{0x01, byte(len(u.Username))}, u.Username...) //nolint:gosec // G115: length checked <= 255
		m = append(append(m, byte(len(u.Password))), u.Password...)     //nolint:gosec // G115: length checked <= 255
		if _, err := c.Write(m); err != nil {
			return ErrUpstream
		}
		var st [2]byte
		if _, err := io.ReadFull(c, st[:]); err != nil {
			return ErrUpstream
		}
		if st[1] != 0x00 {
			return ErrUpstreamAuth
		}
	default:
		return ErrUpstreamAuth
	}
	req := []byte{0x05, 0x01, 0x00}
	if ip, err := netip.ParseAddr(host); err == nil {
		ip = ip.Unmap()
		if ip.Is4() {
			req = append(req, 0x01)
		} else {
			req = append(req, 0x04)
		}
		req = append(req, ip.AsSlice()...)
	} else {
		if len(host) > 255 {
			return ErrRefused
		}
		req = append(append(req, 0x03, byte(len(host))), host...) //nolint:gosec // G115: length checked <= 255
	}
	req = binary.BigEndian.AppendUint16(req, uint16(port)) //nolint:gosec // port validated 1..65535
	if _, err := c.Write(req); err != nil {
		return ErrUpstream
	}
	var head [4]byte
	if _, err := io.ReadFull(c, head[:]); err != nil || head[0] != 0x05 {
		return ErrUpstream
	}
	if head[1] != 0x00 {
		return ErrRefused
	}
	var skip int
	switch head[3] {
	case 0x01:
		skip = 4
	case 0x04:
		skip = 16
	case 0x03:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return ErrUpstream
		}
		skip = int(n[0])
	default:
		return ErrUpstream
	}
	if _, err := io.CopyN(io.Discard, c, int64(skip+2)); err != nil {
		return ErrUpstream
	}
	return nil
}

// Package tunnel implements the standalone lab's SOCKS5 CONNECT and remote TCP
// dialing. It does not share the release SOCKS parser or change Legacy behavior.
package tunnel

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Bounds admitted handlers, not process RSS; each bridge owns two 32 KiB buffers.
const MaxStreams = 64
const SetupTimeout = 10 * time.Second

func StreamLimit(n int) (int, error) {
	if n == 0 {
		return MaxStreams, nil
	}
	if n < 1 || n > MaxStreams {
		return 0, errors.New("max_streams must be 1-64")
	}
	return n, nil
}

func MuxConfig() *yamux.Config {
	c := yamux.DefaultConfig()
	c.AcceptBacklog = MaxStreams
	// Yamux replenishes credit after half a stream window is consumed. Keep
	// that remaining half larger than the 512 KiB ordered-carrier window so
	// one healthy stream does not stall while its window update travels back.
	// 64 admitted streams have at most 128 MiB of unread payload credit.
	// Buffer capacity, carrier, Go and socket overhead are additional.
	c.MaxStreamWindowSize = 2 << 20
	c.KeepAliveInterval = 5 * time.Second
	c.ConnectionWriteTimeout = 10 * time.Second
	c.StreamOpenTimeout = 10 * time.Second
	c.StreamCloseTimeout = 60 * time.Second
	c.LogOutput = io.Discard
	return c
}

func WriteFull(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, e := w.Write(p)
		if n < 0 || n > len(p) {
			return io.ErrShortWrite
		}
		p = p[n:]
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func ReadTarget(r io.Reader) (string, error) {
	var b [2]byte
	if _, e := io.ReadFull(r, b[:]); e != nil {
		return "", e
	}
	n := int(binary.BigEndian.Uint16(b[:]))
	if n < 3 || n > 512 {
		return "", errors.New("invalid target length")
	}
	p := make([]byte, n)
	if _, e := io.ReadFull(r, p); e != nil {
		return "", e
	}
	target := string(p)
	h, port, e := net.SplitHostPort(target)
	if e != nil || h == "" {
		return "", errors.New("invalid target")
	}
	n, e = strconv.Atoi(port)
	if e != nil || n < 1 || n > 65535 {
		return "", errors.New("invalid target port")
	}
	return target, nil
}

func Request(ctx context.Context, s *yamux.Stream, target string) error {
	if len(target) > 512 {
		return errors.New("target too long")
	}
	d := time.Now().Add(SetupTimeout)
	if cd, ok := ctx.Deadline(); ok && cd.Before(d) {
		d = cd
	}
	s.SetDeadline(d)
	stop := context.AfterFunc(ctx, func() { s.SetDeadline(time.Now()) })
	defer stop()
	b := make([]byte, 2+len(target))
	binary.BigEndian.PutUint16(b[:2], uint16(len(target)))
	copy(b[2:], target)
	if e := WriteFull(s, b); e != nil {
		return e
	}
	var status [1]byte
	if _, e := io.ReadFull(s, status[:]); e != nil {
		return e
	}
	if status[0] != 0 {
		if status[0] == 2 {
			return ErrTargetDenied
		}
		return errors.New("remote target denied or unavailable")
	}
	return s.SetDeadline(time.Time{})
}

// Bridge preserves TCP half-close: local EOF sends a Yamux FIN, while receiving
// a FIN only shuts down the local socket's write half. Reads can continue until
// the response completes. An idle/error/cancel path bounds both goroutines.
func Bridge(ctx context.Context, tcp net.Conn, s *yamux.Stream, idle time.Duration) error {
	var mu sync.Mutex
	closed := false
	touch := func() bool {
		mu.Lock()
		defer mu.Unlock()
		if closed || ctx.Err() != nil {
			return false
		}
		d := time.Now().Add(idle)
		tcp.SetDeadline(d)
		s.SetDeadline(d)
		return true
	}
	abort := func() { mu.Lock(); closed = true; tcp.Close(); s.SetDeadline(time.Now()); mu.Unlock() }
	stop := context.AfterFunc(ctx, abort)
	defer stop()
	defer func() { abort(); s.Close() }()
	copyTo := func(dst io.Writer, src io.Reader) error {
		b := make([]byte, 32<<10)
		for {
			if !touch() {
				return context.Canceled
			}
			n, e := src.Read(b)
			if n > 0 {
				if !touch() {
					return context.Canceled
				}
				if we := WriteFull(dst, b[:n]); we != nil {
					return we
				}
			}
			if e == io.EOF {
				return nil
			}
			if e != nil {
				return e
			}
		}
	}
	done := make(chan error, 2)
	go func() {
		e := copyTo(s, tcp)
		if e == nil {
			e = s.Close()
		}
		done <- e
	}()
	go func() {
		e := copyTo(tcp, s)
		if e == nil {
			if c, ok := tcp.(interface{ CloseWrite() error }); ok {
				e = c.CloseWrite()
			} else {
				e = errors.New("TCP half-close unavailable")
			}
		}
		done <- e
	}()
	first := <-done
	if first != nil {
		abort()
	}
	second := <-done
	if first != nil {
		return first
	}
	return second
}

func ServeRemote(ctx context.Context, mux *yamux.Session, policy *TargetDialer, idle time.Duration, maxStreams int) error {
	limit, err := StreamLimit(maxStreams)
	if err != nil {
		return err
	}
	if policy == nil {
		return errors.New("missing egress policy")
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	defer func() { cancel(); wg.Wait() }()
	stop := context.AfterFunc(ctx, func() { mux.Close() })
	defer stop()
	slots := make(chan struct{}, limit)
	for {
		s, e := mux.AcceptStream()
		if e != nil {
			return e
		}
		// Bound half-closed rejects from a non-cooperating authenticated peer.
		if mux.NumStreams() > 2*MaxStreams {
			mux.Close()
			return errors.New("peer exceeded tracked stream limit")
		}
		select {
		case slots <- struct{}{}:
		default:
			s.SetWriteDeadline(time.Now().Add(SetupTimeout))
			WriteFull(s, []byte{1})
			s.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer s.Close()
			stop := context.AfterFunc(ctx, func() { s.SetDeadline(time.Now()) })
			defer stop()
			s.SetDeadline(time.Now().Add(SetupTimeout))
			target, e := ReadTarget(s)
			if e != nil {
				return
			}
			c, e := policy.DialContext(ctx, target)
			if e != nil {
				status := byte(5)
				if errors.Is(e, ErrTargetDenied) {
					status = 2
				}
				WriteFull(s, []byte{status})
				return
			}
			defer c.Close()
			if WriteFull(s, []byte{0}) != nil {
				return
			}
			s.SetDeadline(time.Time{})
			Bridge(ctx, c, s, idle)
		}()
	}
}

func readSOCKS(c net.Conn) (string, error) {
	c.SetDeadline(time.Now().Add(SetupTimeout))
	var b [4]byte
	if _, e := io.ReadFull(c, b[:2]); e != nil {
		return "", e
	}
	if b[0] != 5 || b[1] == 0 {
		return "", errors.New("invalid SOCKS greeting")
	}
	methods := make([]byte, int(b[1]))
	if _, e := io.ReadFull(c, methods); e != nil {
		return "", e
	}
	noauth := false
	for _, m := range methods {
		noauth = noauth || m == 0
	}
	if !noauth {
		WriteFull(c, []byte{5, 255})
		return "", errors.New("unsupported SOCKS authentication")
	}
	if e := WriteFull(c, []byte{5, 0}); e != nil {
		return "", e
	}
	if _, e := io.ReadFull(c, b[:]); e != nil {
		return "", e
	}
	if b[0] != 5 || b[2] != 0 {
		return "", errors.New("invalid SOCKS request")
	}
	if b[1] != 1 {
		reply(c, 7)
		return "", errors.New("only CONNECT is supported")
	}
	var host string
	switch b[3] {
	case 1, 4:
		n := 4
		if b[3] == 4 {
			n = 16
		}
		ip := make([]byte, n)
		if _, e := io.ReadFull(c, ip); e != nil {
			return "", e
		}
		host = net.IP(ip).String()
	case 3:
		if _, e := io.ReadFull(c, b[:1]); e != nil {
			return "", e
		}
		if b[0] == 0 {
			return "", errors.New("empty SOCKS domain")
		}
		name := make([]byte, int(b[0]))
		if _, e := io.ReadFull(c, name); e != nil {
			return "", e
		}
		host = string(name)
	default:
		reply(c, 8)
		return "", errors.New("unsupported SOCKS address")
	}
	if _, e := io.ReadFull(c, b[:2]); e != nil {
		return "", e
	}
	port := binary.BigEndian.Uint16(b[:2])
	if port == 0 {
		reply(c, 1)
		return "", errors.New("invalid SOCKS port")
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}
func reply(c net.Conn, status byte) error {
	return WriteFull(c, []byte{5, status, 0, 1, 0, 0, 0, 0, 0, 0})
}

type OpenFunc func(context.Context) (*yamux.Stream, error)

func ServeSOCKS(ctx context.Context, ln net.Listener, open OpenFunc, idle time.Duration, maxStreams int) error {
	limit, err := StreamLimit(maxStreams)
	if err != nil {
		return err
	}
	if a, ok := ln.Addr().(*net.TCPAddr); !ok || !a.IP.IsLoopback() {
		return errors.New("SOCKS lab listener must be loopback")
	}
	var wg sync.WaitGroup
	ctx, cancel := context.WithCancel(ctx)
	defer func() { cancel(); wg.Wait() }()
	stop := context.AfterFunc(ctx, func() { ln.Close() })
	defer stop()
	slots := make(chan struct{}, limit)
	for {
		c, e := ln.Accept()
		if e != nil {
			return e
		}
		select {
		case slots <- struct{}{}:
		default:
			c.Close()
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			defer c.Close()
			stop := context.AfterFunc(ctx, func() { c.Close() })
			defer stop()
			target, e := readSOCKS(c)
			if e != nil {
				return
			}
			setup, cancel := context.WithTimeout(ctx, SetupTimeout)
			defer cancel()
			s, e := open(setup)
			if e != nil {
				reply(c, 1)
				return
			}
			defer s.Close()
			if err := Request(setup, s, target); err != nil {
				status := byte(5)
				if errors.Is(err, ErrTargetDenied) {
					status = 2
				}
				reply(c, status)
				return
			}
			if reply(c, 0) != nil {
				return
			}
			c.SetDeadline(time.Time{})
			Bridge(ctx, c, s, idle)
		}()
	}
}

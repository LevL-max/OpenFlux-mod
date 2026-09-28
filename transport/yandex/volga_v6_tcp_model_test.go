//go:build volga

package yandex

// Explicit offline diagnostic, not a model of Yandex throughput. Copy into the
// yandex test package and set OPENFLUX_OFFLINE_TCP_MODEL=1 to reproduce. This
// comparator did NOT demonstrate a gain; it is not a transport fix.
import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"universal-bypass-tool/transport"
)

type tcpModelCarrier struct {
	ctx     context.Context
	peer    *volgaV6Runtime
	gate    volgaV6RelayGate
	ordered bool
	mu      sync.Mutex
	base    uint64
	pending map[uint64]volgaV6WireFrame
	posts   atomic.Uint64
	async   bool
	slots   chan struct{}
	wg      sync.WaitGroup
}

func (c *tcpModelCarrier) Generation() uint64          { return 1 }
func (c *tcpModelCarrier) Start(context.Context) error { return nil }
func (c *tcpModelCarrier) Stop() error                 { c.wg.Wait(); return nil }
func (c *tcpModelCarrier) SendVolgaV6(f volgaV6WireFrame) error {
	if c.async {
		select {
		case <-c.ctx.Done():
			return c.ctx.Err()
		case c.slots <- struct{}{}:
		}
		c.wg.Add(1)
		go func() { defer c.wg.Done(); defer func() { <-c.slots }(); _ = c.deliver(f) }()
		return nil
	}
	return c.deliver(f)
}
func (c *tcpModelCarrier) deliver(f volgaV6WireFrame) error {
	if err := c.gate.wait(c.ctx); err != nil {
		return err
	}
	n := c.posts.Add(1)
	// Fixed, deterministic latency variation. No packet loss or provider quota.
	delay := []time.Duration{40, 100, 55, 85}[n%4] * time.Millisecond
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-c.ctx.Done():
		return c.ctx.Err()
	case <-timer.C:
	}
	if !c.ordered || f.Kind != volgaV6FrameData {
		c.peer.handleIncoming(f)
		return nil
	}
	// Diagnostic-only comparator. This is deliberately not a production
	// receiver: it does not implement restart, bounded storage, or repair SACK.
	c.mu.Lock()
	defer c.mu.Unlock()
	if f.Seq <= c.base {
		c.peer.handleIncoming(f)
		return nil
	}
	c.pending[f.Seq] = f
	for {
		next, ok := c.pending[c.base+1]
		if !ok {
			break
		}
		delete(c.pending, c.base+1)
		c.base++
		c.peer.handleIncoming(next)
	}
	return nil
}

func tcpModelStack(t *testing.T, ctx context.Context, tr *YandexVolgaV6Transport, ip [4]byte) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	for _, option := range []tcpip.SettableTransportProtocolOption{
		&tcpip.TCPReceiveBufferSizeRangeOption{Min: 65536, Default: 262144, Max: 1048576},
		&tcpip.TCPSendBufferSizeRangeOption{Min: 65536, Default: 262144, Max: 1048576},
	} {
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, option); err != nil {
			t.Fatal(err)
		}
	}
	ep := channel.New(2048, 1500, "")
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4(ip), PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.AddRoute(tcpip.Route{Destination: header.IPv4EmptySubnet, NIC: 1})
	tr.Receive(func(p []byte) {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), p...))})
		ep.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()
	})
	go func() {
		for {
			p := ep.ReadContext(ctx)
			if p == nil {
				return
			}
			view := p.ToView()
			data := append([]byte(nil), view.AsSlice()...)
			view.Release()
			p.DecRef()
			for tr.Send(data) != nil {
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Millisecond):
				}
			}
		}
	}()
	t.Cleanup(func() { s.Close(); ep.Close(); s.Wait() })
	return s
}

func TestVolgaV6TCPReorderModel(t *testing.T) {
	if os.Getenv("OPENFLUX_OFFLINE_TCP_MODEL") != "1" {
		t.Skip("explicit offline diagnostic only")
	}
	for _, tc := range []struct{ ordered, sequenced bool }{{false, false}, {true, true}} {
		ordered := tc.ordered
		t.Run(fmt.Sprintf("ordered_%t_sequenced_%t", ordered, tc.sequenced), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			ca := &tcpModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}, ordered: ordered, pending: make(map[uint64]volgaV6WireFrame)}
			cb := &tcpModelCarrier{ctx: ctx, gate: volgaV6RelayGate{rate: 360}, ordered: ordered, pending: make(map[uint64]volgaV6WireFrame)}
			ca.async, cb.async = tc.sequenced, tc.sequenced
			ca.slots, cb.slots = make(chan struct{}, 64), make(chan struct{}, 64)
			cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
			cfg.Telemetry = false
			cfg.QueueSize, cfg.SendQueueSize, cfg.SendWorkers = 512, 64, 64
			if tc.sequenced {
				cfg.SendWorkers = 1
			}
			a := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return ca, nil })
			b := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) { return cb, nil })
			ca.peer, cb.peer = b.runtime, a.runtime
			sa := tcpModelStack(t, ctx, a, [4]byte{10, 20, 0, 1})
			sb := tcpModelStack(t, ctx, b, [4]byte{10, 20, 0, 2})
			// Registered after stack cleanup so cancellation happens first.
			t.Cleanup(func() { cancel(); a.Stop(); b.Stop() })
			if err := a.Start(); err != nil {
				t.Fatal(err)
			}
			if err := b.Start(); err != nil {
				t.Fatal(err)
			}
			addr := tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 20, 0, 2}), Port: 18080}
			ln, err := gonet.ListenTCP(sb, addr, ipv4.ProtocolNumber)
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			writerDone := make(chan struct{})
			go func() {
				defer close(writerDone)
				c, err := ln.Accept()
				if err != nil {
					return
				}
				defer c.Close()
				c.SetWriteDeadline(time.Now().Add(7 * time.Second))
				data := make([]byte, 32768)
				for i := range data {
					data[i] = byte(i % 251)
				}
				for {
					if _, err = c.Write(data); err != nil {
						return
					}
				}
			}()
			dialCtx, dialCancel := context.WithTimeout(ctx, 3*time.Second)
			c, err := gonet.DialTCPWithBind(dialCtx, sa, tcpip.FullAddress{}, addr, ipv4.ProtocolNumber)
			dialCancel()
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			start := time.Now()
			c.SetReadDeadline(start.Add(6 * time.Second))
			buf := make([]byte, 65536)
			var received int64
			for {
				n, err := c.Read(buf)
				for i := 0; i < n; i++ {
					if buf[i] != byte(((received+int64(i))%32768)%251) {
						t.Fatal("payload corruption")
					}
				}
				received += int64(n)
				if err != nil {
					if err == io.EOF {
						t.Fatal("unexpected early EOF")
					}
					break
				}
			}
			seconds := time.Since(start).Seconds()
			fmt.Printf("V6TCPMODEL ordered=%t sequenced=%t bytes=%d seconds=%.3f mbps=%.3f tcp_retransmits=%d posts=%d\n", ordered, tc.sequenced, received, seconds, float64(received)*8/seconds/1e6, sb.Stats().TCP.Retransmits.Value(), cb.posts.Load())
			if received == 0 {
				t.Fatal("no TCP data")
			}
			c.Close()
			select {
			case <-writerDone:
			case <-time.After(2 * time.Second):
				t.Fatal("writer did not finish")
			}
		})
	}
}

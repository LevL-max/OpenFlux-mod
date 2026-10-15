package mailru

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"os"
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

// Real gVisor TCP over the deployed OFB1 -> LZ4 -> chat chain. No raw socket,
// host route, SOCKS listener or production service is needed for this probe.
func TestLiveChatTCPIntegrityAcrossReconnect(t *testing.T) {
	doc := os.Getenv("OPENFLUX_MAILRU_TEST_DOCUMENT")
	if doc == "" {
		t.Skip("set OPENFLUX_MAILRU_TEST_DOCUMENT to a disposable public document")
	}
	runChatTCPProbe(t, doc)
}

func chatTCPStack(t *testing.T, link transport.Transport, last byte) *stack.Stack {
	t.Helper()
	s := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol}})
	ep := channel.New(1024, 1500, "")
	if err := s.CreateNIC(1, ep); err != nil {
		t.Fatal(err)
	}
	addr := tcpip.AddrFrom4([4]byte{10, 77, 0, last})
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: addr, PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	for _, opt := range []tcpip.SettableTransportProtocolOption{
		&tcpip.TCPReceiveBufferSizeRangeOption{Min: 65536, Default: 1048576, Max: 4194304},
		&tcpip.TCPSendBufferSizeRangeOption{Min: 65536, Default: 1048576, Max: 4194304},
	} {
		if err := s.SetTransportProtocolOption(tcp.ProtocolNumber, opt); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	link.Receive(func(data []byte) {
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
		ep.InjectInbound(ipv4.ProtocolNumber, pkt)
		pkt.DecRef()
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			pkt := ep.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			_ = link.Send(v.AsSlice())
			v.Release()
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-done
		_ = link.Stop()
		s.Close()
		ep.Close()
		s.Wait()
	})
	return s
}

func runChatTCPProbe(t *testing.T, doc string) {
	t.Helper()
	peers := []*MailruDocsTransport{NewMailruDocsTransport(doc, transport.DefaultConfig()), NewMailruDocsTransport(doc, transport.DefaultConfig())}
	stacks := make([]*stack.Stack, 2)
	for i, peer := range peers {
		link := transport.NewBatchingTransport(transport.NewCompressedTransport(peer), 512, 500000, 5*time.Millisecond)
		stacks[i] = chatTCPStack(t, link, byte(i+1))
		if err := link.Start(); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(60 * time.Second)
	for !(peers[0].IsConnected() && peers[1].IsConnected()) && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if !peers[0].IsConnected() || !peers[1].IsConnected() {
		t.Fatal("chat peers failed to authorize")
	}
	listener, err := gonet.ListenTCP(stacks[1], tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 77, 0, 2}), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	echoDone := make(chan error, 1)
	go func() {
		c, err := listener.Accept()
		if err != nil {
			echoDone <- err
			return
		}
		defer c.Close()
		c.SetDeadline(time.Now().Add(3 * time.Minute))
		_, err = io.Copy(c, c)
		echoDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := gonet.DialContextTCP(ctx, stacks[0], tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4([4]byte{10, 77, 0, 2}), Port: 8080}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Minute))
	start := time.Now()
	for i := 0; i < 8; i++ {
		if i == 4 {
			peers[0].Mu.RLock()
			old := peers[0].session
			peers[0].Mu.RUnlock()
			peers[0].dropConnection(old)
		}
		payload := make([]byte, 64<<10)
		if _, err := rand.Read(payload); err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Write(payload); err != nil {
			t.Fatalf("write chunk %d: %v", i, err)
		}
		got := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, got); err != nil {
			t.Fatalf("read chunk %d: %v", i, err)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("TCP corruption at chunk %d", i)
		}
		t.Logf("TCP chunk=%d verified=%d bytes each way", i+1, (i+1)*len(payload))
	}
	conn.Close()
	listener.Close()
	select {
	case <-echoDone:
	case <-time.After(5 * time.Second):
		t.Fatal("TCP echo did not stop")
	}
	t.Logf("TCP verified 524288 bytes each way, elapsed=%s reconnects=%d/%d", time.Since(start).Round(time.Millisecond), peers[0].Stats().Reconnects, peers[1].Stats().Reconnects)
}

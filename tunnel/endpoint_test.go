package tunnel

import (
	"encoding/binary"
	"net"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/network"
	"universal-bypass-tool/transport"
)

type captureTransport struct {
	mu      sync.Mutex
	receive func([]byte)
	sent    chan []byte
}

func (c *captureTransport) Start() error                    { return nil }
func (c *captureTransport) Stop() error                     { return nil }
func (c *captureTransport) IsConnected() bool               { return true }
func (c *captureTransport) Stats() transport.TransportStats { return transport.TransportStats{} }

func (c *captureTransport) Receive(callback func([]byte)) {
	c.mu.Lock()
	c.receive = callback
	c.mu.Unlock()
}

func (c *captureTransport) Send(data []byte) error {
	c.sent <- append([]byte(nil), data...)
	return nil
}

func (c *captureTransport) inject(data []byte) {
	c.mu.Lock()
	callback := c.receive
	c.mu.Unlock()
	callback(data)
}

// synPacket builds an IPv4 TCP SYN from 1.2.3.4:srcPort to 10.10.10.2:80.
func synPacket(srcPort uint16) []byte {
	src, dst := [4]byte{1, 2, 3, 4}, [4]byte{10, 10, 10, 2}
	pkt := make([]byte, 40)
	pkt[0] = 0x45
	binary.BigEndian.PutUint16(pkt[2:4], uint16(len(pkt)))
	pkt[8] = 64
	pkt[9] = 6
	copy(pkt[12:16], src[:])
	copy(pkt[16:20], dst[:])
	binary.BigEndian.PutUint16(pkt[10:12], network.IPChecksum(pkt[:20]))
	tcp := pkt[20:]
	binary.BigEndian.PutUint16(tcp[0:2], srcPort)
	binary.BigEndian.PutUint16(tcp[2:4], 80)
	binary.BigEndian.PutUint32(tcp[4:8], 1000)
	tcp[12] = 5 << 4
	tcp[13] = 0x02
	binary.BigEndian.PutUint16(tcp[14:16], 65535)
	binary.BigEndian.PutUint16(tcp[16:18], network.TCPChecksum(tcp, src, dst))
	return pkt
}

func TestInjectInboundBeforeAttachIsIgnored(t *testing.T) {
	NewTunnelLinkEndpoint().InjectInbound(synPacket(40000))
}

// Every injected packet must reach the stack intact, including after its
// pooled buffers have been returned and reused.
func TestInjectedSYNsReachListener(t *testing.T) {
	trans := &captureTransport{sent: make(chan []byte, 64)}
	tun := NewTCPTunnel(trans, false, 65536, 1048576)
	ln, err := tun.ListenTCP(80)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	for i := 0; i < 200; i++ {
		port := uint16(40000 + i)
		trans.inject(synPacket(port))
		deadline := time.After(2 * time.Second)
		for answered := false; !answered; {
			select {
			case reply := <-trans.sent:
				// Retransmitted SYN-ACKs for earlier ports may arrive in between.
				tcp := reply[20:]
				answered = binary.BigEndian.Uint16(tcp[2:4]) == port && tcp[13]&0x12 == 0x12
			case <-deadline:
				t.Fatalf("no SYN-ACK for port %d", port)
			}
		}
	}
}

func TestGetLocalIPIsIPv4(t *testing.T) {
	if ip := net.ParseIP(getLocalIP()); ip == nil || ip.To4() == nil {
		t.Fatalf("getLocalIP() = %q, want an IPv4 address", getLocalIP())
	}
}

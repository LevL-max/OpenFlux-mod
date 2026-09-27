package tunnel

import (
	"sync"
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"universal-bypass-tool/network"
	"universal-bypass-tool/utils"
)

type TunnelLinkEndpoint struct {
	mu               sync.RWMutex
	dispatcher       stack.NetworkDispatcher
	onOutgoingPacket func([]byte)
	packetIn         atomic.Uint64
	packetOut        atomic.Uint64
}

func NewTunnelLinkEndpoint() *TunnelLinkEndpoint {
	return &TunnelLinkEndpoint{}
}

func (e *TunnelLinkEndpoint) InjectInbound(data []byte) {
	e.mu.RLock()
	dispatcher := e.dispatcher
	e.mu.RUnlock()
	if dispatcher == nil {
		return
	}
	e.packetIn.Add(1)
	// Arguments are evaluated even when debug logging is off.
	if utils.IsVerbose() {
		utils.Debugf("<- %d bytes - %s\n", len(data), network.ParsePacketInfo(data))
	}
	// MakeWithData copies into a pooled chunk; DecRef returns it to the pool
	// once the stack has taken the references it keeps.
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{
		Payload: buffer.MakeWithData(data),
	})
	dispatcher.DeliverNetworkPacket(ipv4.ProtocolNumber, pkt)
	pkt.DecRef()
}

func (e *TunnelLinkEndpoint) WritePackets(pkts stack.PacketBufferList) (int, tcpip.Error) {
	n := 0
	for _, pkt := range pkts.AsSlice() {
		data := pkt.ToView().ToSlice()
		e.packetOut.Add(1)
		if e.onOutgoingPacket != nil {
			e.onOutgoingPacket(data)
		}
		n++
	}
	return n, nil
}

func (e *TunnelLinkEndpoint) MTU() uint32                                 { return 1500 }
func (e *TunnelLinkEndpoint) MaxHeaderLength() uint16                      { return 0 }
func (e *TunnelLinkEndpoint) LinkAddress() tcpip.LinkAddress               { return "\x02\x00\x00\x00\x00\x01" }
func (e *TunnelLinkEndpoint) Capabilities() stack.LinkEndpointCapabilities { return stack.CapabilityNone }
func (e *TunnelLinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.mu.Lock()
	e.dispatcher = dispatcher
	e.mu.Unlock()
}
func (e *TunnelLinkEndpoint) IsAttached() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.dispatcher != nil
}
func (e *TunnelLinkEndpoint) Wait()                                        {}
func (e *TunnelLinkEndpoint) ARPHardwareType() header.ARPHardwareType      { return header.ARPHardwareNone }
func (e *TunnelLinkEndpoint) AddHeader(*stack.PacketBuffer)                {}
func (e *TunnelLinkEndpoint) Close()                                       {}
func (e *TunnelLinkEndpoint) SetMTU(uint32)                                {}
func (e *TunnelLinkEndpoint) SetLinkAddress(tcpip.LinkAddress)             {}
func (e *TunnelLinkEndpoint) ParseHeader(*stack.PacketBuffer) bool         { return true }
func (e *TunnelLinkEndpoint) SetOnCloseAction(func())                      {}

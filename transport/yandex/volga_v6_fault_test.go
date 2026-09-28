//go:build volga

package yandex

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestVolgaV6RegressionFragmentLossHandoffAndDuplicate(t *testing.T) {
	var delivered [][]byte
	peer := newVolgaV6Runtime(200, nil, defaultVolgaV6RuntimeConfig(), func(p [][]byte) { delivered = append(delivered, p...) })
	var sender *volgaV6Runtime
	var dropped bool
	factory := func(g uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		wire, _ := localVolgaV6Wire(t, func(frame volgaV6WireFrame) {
			if g == 1 && frame.Kind == volgaV6FrameFragment && frame.Fragment.Index == 2 {
				dropped = true
				return
			}
			peer.handleIncoming(frame)
		})
		wire.generation = g
		return &volgaV6StartedWire{wire}, nil
	}
	sender = newVolgaV6Runtime(100, factory, defaultVolgaV6RuntimeConfig(), nil)
	if err := sender.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer sender.Stop()
	ackWire, _ := localVolgaV6Wire(t, sender.handleIncoming)
	defer ackWire.Stop()
	want := bytes.Repeat([]byte{19, 8, 27, 65}, 15000)
	seq, err := sender.Send([][]byte{want})
	if err != nil || seq != 1 || !dropped {
		t.Fatalf("initial send seq=%d err=%v dropped=%v", seq, err, dropped)
	}
	if len(delivered) != 0 {
		t.Fatal("partial DATA delivered")
	}
	if _, ok := peer.receiver.AckSnapshot(); ok {
		t.Fatal("partial DATA acknowledged")
	}
	if _, err := sender.Send([][]byte{[]byte("later")}); err != nil {
		t.Fatal(err)
	}
	ack, _ := peer.receiver.AckSnapshot()
	if ack.Base != 0 || len(ack.Ranges) != 1 {
		t.Fatalf("hole disappeared: %+v", ack)
	}
	if err := ackWire.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameAck, Session: ack.Session, Ack: ack}); err != nil {
		t.Fatal(err)
	}
	if sender.Snapshot(time.Now()).Reliable.ReplayDepth != 2 {
		t.Fatal("SACK discarded replay")
	}
	if _, _, err := sender.manager.Handoff(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := sender.session.replayAt(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 || !bytes.Equal(delivered[1], want) {
		t.Fatal("handoff did not reconstruct original record")
	}
	ack, _ = peer.receiver.AckSnapshot()
	if ack.Base != 2 {
		t.Fatalf("ACK did not cross repaired hole: %+v", ack)
	}
	// Simulate a lost ACK: a second complete replay must not duplicate data.
	if err := sender.session.replayAt(1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if len(delivered) != 2 {
		t.Fatal("replay delivered a duplicate record")
	}
	if err := ackWire.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameAck, Session: ack.Session, Ack: ack}); err != nil {
		t.Fatal(err)
	}
	snap := sender.Snapshot(time.Now())
	if snap.Reliable.NextSeq != 2 || snap.Reliable.ReplayDepth != 0 || snap.Carrier.ActiveGeneration != 2 {
		t.Fatalf("logical sequence changed across physical repair: %+v", snap)
	}
}

func TestVolgaV6RegressionFragmentsReverseOrderAndLimits(t *testing.T) {
	var frames []volgaV6WireFrame
	wire, _ := localVolgaV6Wire(t, func(f volgaV6WireFrame) { frames = append(frames, f) })
	defer wire.Stop()
	want := bytes.Repeat([]byte{7}, 60000)
	if err := wire.SendVolgaV6(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 100, Seq: 1, Floor: 1, Payload: [][]byte{want}}); err != nil {
		t.Fatal(err)
	}
	var assembler volgaV6Reassembler
	now := time.Now()
	for i := len(frames) - 1; i >= 0; i-- {
		frame, ok := assembler.Accept(frames[i], now)
		if ok != (i == 0) {
			t.Fatalf("completion at fragment %d: %t", i, ok)
		}
		if ok && !bytes.Equal(frame.Payload[0], want) {
			t.Fatal("reverse-order reconstruction corrupted")
		}
	}
	if assembler.bytes != 0 || len(assembler.entries) != 0 {
		t.Fatal("completed assembly retained")
	}
	bad := frames[0]
	bad.Fragment.Total = volgaV6MaxFrameBytes + 1
	if _, ok := assembler.Accept(bad, now); ok || assembler.bytes != 0 {
		t.Fatal("oversized allocation accepted")
	}
	bad = frames[0]
	bad.Fragment.Index = 65535
	if _, ok := assembler.Accept(bad, now); ok || assembler.bytes != 0 {
		t.Fatal("invalid index accepted")
	}
	for i := uint64(1); i <= 300; i++ {
		f := frames[0]
		f.Seq = i
		assembler.Accept(f, now)
	}
	if len(assembler.entries) > volgaV6MaxAssemblies || assembler.bytes > volgaV6MaxAssemblyBytes {
		t.Fatal("reassembly limits exceeded")
	}
	assembler.Accept(frames[0], now.Add(volgaV6AssemblyTTL))
	if len(assembler.entries) != 1 {
		t.Fatal("expired assemblies retained")
	}
}

func TestVolgaV6RegressionInvalidPayloadDoesNotConsumeSequence(t *testing.T) {
	sender := newVolgaV6ReliableSession(5, nil, defaultVolgaV6ReliableConfig())
	for _, payload := range [][][]byte{nil, {nil}, {make([]byte, 65536)}} {
		seq, err := sender.Send(payload)
		if seq != 0 || !errors.Is(err, errVolgaV6InvalidPayload) {
			t.Fatalf("invalid payload reserved seq=%d err=%v", seq, err)
		}
	}
	if seq, err := sender.Send([][]byte{[]byte("valid")}); seq != 1 || err != nil {
		t.Fatalf("hole after invalid payload seq=%d err=%v", seq, err)
	}
}

func TestVolgaV6RegressionHandoffDeadlineKeepsOldCarrier(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := defaultVolgaV6RuntimeConfig()
	cfg.CarrierStartTimeout = 30 * time.Millisecond
	cfg.Recovery.ProgressStall = time.Millisecond
	cfg.Reliable.BaseRTO = time.Hour
	r := newVolgaV6Runtime(10, func(g uint64, fn func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6GatedStart{g, entered, release, make(chan volgaV6Ack, 1), fn}, nil
	}, cfg, nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	if _, err := r.Send([][]byte{[]byte("outstanding")}); err != nil {
		t.Fatal(err)
	}
	result := r.Tick(context.Background(), time.Now().Add(time.Second))
	if !errors.Is(result.HandoffErr, context.DeadlineExceeded) {
		t.Fatalf("missing handoff deadline: %+v", result)
	}
	if snap := r.Snapshot(time.Now()); snap.Carrier.ActiveGeneration != 1 || snap.Reliable.ReplayDepth != 1 {
		t.Fatalf("failed handoff lost active carrier or replay: %+v", snap)
	}
}

func TestVolgaV6RegressionAuthorizationHonorsDeadline(t *testing.T) {
	entered := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := authorizeContext(ctx, server.URL)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("authorization ignored cancellation: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("authorization was not exercised")
	}
}

func TestVolgaV6RegressionStopCancelsBlockedHandoff(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.Telemetry = false
	cfg.BatchPackets = 1
	cfg.TickInterval = 5 * time.Millisecond
	cfg.Runtime.Recovery.ProgressStall = 10 * time.Millisecond
	cfg.Runtime.Reliable.BaseRTO = time.Hour
	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(g uint64, fn func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6GatedStart{g, entered, release, make(chan volgaV6Ack, 4), fn}, nil
	})
	if err := v6.Start(); err != nil {
		t.Fatal(err)
	}
	defer v6.Stop()
	if err := v6.Send([]byte("outstanding")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handoff not exercised")
	}
	done := make(chan error, 1)
	go func() { done <- v6.Stop() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not cancel handoff")
	}
}

func TestVolgaV6RegressionAckContinuesDuringBlockedRepair(t *testing.T) {
	var sends atomic.Int32
	entered, release := make(chan struct{}), make(chan struct{})
	acks := make(chan volgaV6Ack, 16)
	wire, _ := localVolgaV6Wire(t, func(frame volgaV6WireFrame) {
		if frame.Kind == volgaV6FrameData && sends.Add(1) == 2 {
			close(entered)
			<-release
		}
		if frame.Kind == volgaV6FrameAck {
			acks <- frame.Ack
		}
	})
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.Telemetry = false
	cfg.BatchPackets = 1
	cfg.TickInterval = 5 * time.Millisecond
	cfg.Runtime.Recovery.ProgressStall = time.Hour
	cfg.Runtime.Reliable.BaseRTO = 10 * time.Millisecond
	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6StartedWire{wire}, nil
	})
	if err := v6.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); v6.Stop() }()
	if err := v6.Send([]byte("outstanding")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("repair not exercised")
	}
	v6.runtime.handleIncoming(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 100, Seq: 1, Floor: 1, Payload: [][]byte{[]byte("incoming")}})
	select {
	case ack := <-acks:
		if ack.Base != 1 {
			t.Fatal("wrong ACK")
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ACK blocked by DATA repair")
	}
}

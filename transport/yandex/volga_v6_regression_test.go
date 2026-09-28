//go:build volga

package yandex

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

type volgaV6LocalRoundTripper func(*http.Request) (*http.Response, error)

func (f volgaV6LocalRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// This adapter invokes the production serializer and decoder, but never dials.
// HTTP success is returned independently of the peer's logical ACK.
func localVolgaV6Wire(t *testing.T, onFrame func(volgaV6WireFrame)) (*volgaV6YandexCarrier, *atomic.Int64) {
	t.Helper()
	c := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	c.started.Store(true)
	posts := &atomic.Int64{}
	decoder := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	decoder.auth.UserID = 4243
	decoder.onFrame = onFrame
	ws := newVolgaV6YandexWS(decoder, decoder.auth)
	t.Cleanup(func() { ws.Stop(); decoder.Stop() })
	c.http = &http.Client{Transport: volgaV6LocalRoundTripper(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		if len(body) > 8000 {
			return nil, errors.New("offline relay: body exceeds limit")
		}
		_, ok := extractVolgaV6LogicalFrameFromRelayBody(body)
		if !ok {
			return nil, errors.New("offline relay: invalid frame")
		}
		posts.Add(1)
		var relay struct {
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(body, &relay); err != nil {
			return nil, err
		}
		inner, _ := json.Marshal(map[string]interface{}{"t": "relay", "userId": 4242, "message": relay.Message})
		envelope, _ := json.Marshal(map[string]interface{}{"operation": "SESSION", "message": string(inner)})
		ws.handleMessage(envelope)
		return &http.Response{StatusCode: 204, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	return c, posts
}

type volgaV6StartedWire struct{ *volgaV6YandexCarrier }

func (c *volgaV6StartedWire) Start(context.Context) error { return nil }

func TestVolgaV6RegressionRealWireBulkDoesNotStall(t *testing.T) {
	var sent *YandexVolgaV6Transport
	var received atomic.Int64
	peer := newVolgaV6Runtime(9001, nil, defaultVolgaV6RuntimeConfig(), func(payload [][]byte) {
		received.Add(int64(len(payload)))
	})
	wire, _ := localVolgaV6Wire(t, func(frame volgaV6WireFrame) {
		peer.handleIncoming(frame)
		if ack, ok := peer.receiver.AckSnapshot(); ok {
			sent.runtime.session.HandleAck(ack)
		}
	})
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.BatchTimeout = 20 * time.Millisecond
	cfg.SendWorkers = 1
	cfg.Telemetry = false
	cfg.Runtime.Recovery.ProgressStall = time.Hour
	sent = newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6StartedWire{wire}, nil
	})
	// Preload: the byte threshold, rather than scheduling, controls this test.
	for i := 0; i < 12; i++ {
		sent.queue <- bytes.Repeat([]byte{byte(i)}, 1500)
	}
	if err := sent.Start(); err != nil {
		t.Fatal(err)
	}
	defer sent.Stop()
	deadline := time.Now().Add(time.Second)
	for received.Load() != 12 || sent.Snapshot(time.Now()).Reliable.ReplayDepth != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("real wire stalled: delivered=%d/12 replay=%+v physical=%+v", received.Load(), sent.Snapshot(time.Now()).Reliable, wire.Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func TestVolgaV6RegressionLargeRecordFitsPhysicalBodies(t *testing.T) {
	want := make([]byte, 60000)
	for i := range want {
		want[i] = byte((i*31 + i/97) % 251)
	}
	var got []byte
	peer := newVolgaV6Runtime(9002, nil, defaultVolgaV6RuntimeConfig(), func(payload [][]byte) { got = append([]byte(nil), payload[0]...) })
	wire, posts := localVolgaV6Wire(t, peer.handleIncoming)
	defer wire.Stop()
	frame := volgaV6WireFrame{Kind: volgaV6FrameData, Session: 42, Seq: 1, Floor: 1, Payload: [][]byte{want}}
	if err := wire.SendVolgaV6(frame); err != nil {
		t.Fatalf("large record cannot be delivered: %v", err)
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("large record corrupted or absent: got=%d want=%d", len(got), len(want))
	}
	if posts.Load() < 2 {
		t.Fatal("large record did not use bounded physical fragments")
	}
	ack, ok := peer.receiver.AckSnapshot()
	if !ok || ack.Base != 1 {
		t.Fatalf("logical sequence not acknowledged after reassembly: %+v", ack)
	}
}

type volgaV6GatedStart struct {
	generation uint64
	entered    chan struct{}
	release    chan struct{}
	acks       chan volgaV6Ack
	onFrame    func(volgaV6WireFrame)
}

func (c *volgaV6GatedStart) Generation() uint64 { return c.generation }
func (c *volgaV6GatedStart) Start(ctx context.Context) error {
	if c.generation == 1 {
		return nil
	}
	close(c.entered)
	select {
	case <-c.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
func (c *volgaV6GatedStart) Stop() error { return nil }
func (c *volgaV6GatedStart) SendVolgaV6(frame volgaV6WireFrame) error {
	if frame.Kind == volgaV6FrameAck {
		select {
		case c.acks <- frame.Ack:
		default:
		}
	}
	return nil
}

func TestVolgaV6RegressionAckContinuesDuringBlockedHandoff(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	acks := make(chan volgaV6Ack, 8)
	var initial *volgaV6GatedStart
	cfg := DefaultVolgaV6TransportConfig([]string{"offline"})
	cfg.BatchPackets = 1
	cfg.TickInterval = 5 * time.Millisecond
	cfg.Telemetry = false
	cfg.Runtime.Recovery.ProgressStall = 20 * time.Millisecond
	cfg.Runtime.Recovery.RecycleCooldown = time.Hour
	cfg.Runtime.Reliable.BaseRTO = time.Hour
	v6 := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, func(g uint64, fn func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		c := &volgaV6GatedStart{g, entered, release, acks, fn}
		if g == 1 {
			initial = c
		}
		return c, nil
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
		t.Fatal("handoff never started")
	}
	initial.onFrame(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 88, Seq: 1, Floor: 1, Payload: [][]byte{[]byte("incoming")}})
	select {
	case ack := <-acks:
		if ack.Session != 88 || ack.Base != 1 {
			t.Fatalf("wrong ACK: %+v", ack)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("ACK blocked behind replacement Start")
	}
}

// Guard against clearing the dirty flag for data that arrived during an ACK POST.
func TestVolgaV6RegressionAckDirtySurvivesConcurrentReceive(t *testing.T) {
	var r *volgaV6Runtime
	var once sync.Once
	wire, _ := localVolgaV6Wire(t, func(frame volgaV6WireFrame) {
		if frame.Kind == volgaV6FrameAck {
			once.Do(func() {
				r.handleIncoming(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 44, Seq: 2, Floor: 1, Payload: [][]byte{[]byte("two")}})
			})
		}
	})
	r = newVolgaV6Runtime(9003, func(uint64, func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		return &volgaV6StartedWire{wire}, nil
	}, defaultVolgaV6RuntimeConfig(), nil)
	if err := r.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer r.Stop()
	r.handleIncoming(volgaV6WireFrame{Kind: volgaV6FrameData, Session: 44, Seq: 1, Floor: 1, Payload: [][]byte{[]byte("one")}})
	if err := r.repeatAckIfDue(time.Now()); err != nil {
		t.Fatal(err)
	}
	if !r.ackDirty.Load() {
		t.Fatal("ACK completion erased newer pending acknowledgement")
	}
}

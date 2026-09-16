package yandex

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func newOfflineVolgaV6YandexCarrier(t *testing.T, generation uint64, limit int) *volgaV6YandexCarrier {
	t.Helper()
	cfg := defaultVolgaV6YandexConfig()
	cfg.HTTPBodyLimit = limit
	carrier := newVolgaV6YandexCarrier(generation, "https://disk.yandex.ru/i/test", cfg, nil)
	carrier.mu.Lock()
	carrier.auth = &volgaAuth{UserID: 4242}
	carrier.mu.Unlock()
	return carrier
}

func relayOperationIDs(t *testing.T, body []byte) (string, string) {
	t.Helper()
	var payload struct {
		Message struct {
			Bundle []json.RawMessage `json:"bundle"`
		} `json:"message"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Message.Bundle) < 2 {
		t.Fatalf("bundle entries=%d want at least 2", len(payload.Message.Bundle))
	}
	var first, second struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(payload.Message.Bundle[0], &first); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload.Message.Bundle[1], &second); err != nil {
		t.Fatal(err)
	}
	return first.ID, second.ID
}

func TestVolgaV6YandexCarrierFactoryRotatesDocumentsByPhysicalGeneration(t *testing.T) {
	factory := newVolgaV6YandexCarrierFactory([]string{"doc-a", "doc-b"}, defaultVolgaV6YandexConfig())
	for generation, want := range map[uint64]string{1: "doc-a", 2: "doc-b", 3: "doc-a", 4: "doc-b"} {
		physical, err := factory(generation, nil)
		if err != nil {
			t.Fatal(err)
		}
		carrier, ok := physical.(*volgaV6YandexCarrier)
		if !ok {
			t.Fatalf("generation %d type=%T", generation, physical)
		}
		if carrier.generation != generation || carrier.docURL != want {
			t.Fatalf("generation=%d doc=%q want=%q", carrier.generation, carrier.docURL, want)
		}
	}
}

func TestVolgaV6YandexCarrierFactoryRejectsMissingOrEmptyDocuments(t *testing.T) {
	if _, err := newVolgaV6YandexCarrierFactory(nil, defaultVolgaV6YandexConfig())(1, nil); err == nil {
		t.Fatal("missing document list was accepted")
	}
	if _, err := newVolgaV6YandexCarrierFactory([]string{"doc-a", ""}, defaultVolgaV6YandexConfig())(1, nil); err == nil {
		t.Fatal("empty document URL was accepted")
	}
}

func TestVolgaV6YandexCarrierSameLogicalReplayGetsFreshWireIDs(t *testing.T) {
	carrier := newOfflineVolgaV6YandexCarrier(t, 7, 8000)
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 111,
		Seq:     9,
		Floor:   9,
		Payload: [][]byte{[]byte("same-logical-payload")},
	}
	records, ok := encodeVolgaV6Records(frame)
	if !ok {
		t.Fatal("could not encode logical frame")
	}
	firstBody, err := carrier.buildRelayBody(records)
	if err != nil {
		t.Fatal(err)
	}
	secondBody, err := carrier.buildRelayBody(records)
	if err != nil {
		t.Fatal(err)
	}
	firstA, firstB := relayOperationIDs(t, firstBody)
	secondA, secondB := relayOperationIDs(t, secondBody)
	if firstA == secondA || firstB == secondB {
		t.Fatalf("wire IDs reused: first=(%s,%s) second=(%s,%s)", firstA, firstB, secondA, secondB)
	}

	decodedFirst, ok := extractVolgaV6LogicalFrameFromRelayBody(firstBody)
	if !ok {
		t.Fatal("could not recover logical frame from first relay body")
	}
	decodedSecond, ok := extractVolgaV6LogicalFrameFromRelayBody(secondBody)
	if !ok {
		t.Fatal("could not recover logical frame from second relay body")
	}
	if decodedFirst.Session != decodedSecond.Session || decodedFirst.Seq != decodedSecond.Seq {
		t.Fatalf("logical identity changed across physical replay: first=%+v second=%+v", decodedFirst, decodedSecond)
	}
}

func extractVolgaV6LogicalFrameFromRelayBody(body []byte) (volgaV6WireFrame, bool) {
	var payload struct {
		Message struct {
			Bundle []json.RawMessage `json:"bundle"`
		} `json:"message"`
	}
	if json.Unmarshal(body, &payload) != nil || len(payload.Message.Bundle) < 3 {
		return volgaV6WireFrame{}, false
	}
	var encoded string
	if json.Unmarshal(payload.Message.Bundle[2], &encoded) != nil {
		return volgaV6WireFrame{}, false
	}
	blob, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return volgaV6WireFrame{}, false
	}
	records := make([][]byte, 0)
	for len(blob) >= 2 {
		n := int(binary.BigEndian.Uint16(blob[:2]))
		blob = blob[2:]
		if n <= 0 || len(blob) < n {
			return volgaV6WireFrame{}, false
		}
		records = append(records, append([]byte(nil), blob[:n]...))
		blob = blob[n:]
	}
	if len(blob) != 0 {
		return volgaV6WireFrame{}, false
	}
	return decodeVolgaV6Records(records)
}

func TestVolgaV6YandexCarrierEnforcesHTTPBodyLimitBeforeNetwork(t *testing.T) {
	carrier := newOfflineVolgaV6YandexCarrier(t, 2, 256)
	frame := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 222,
		Seq:     1,
		Floor:   1,
		Payload: [][]byte{[]byte(strings.Repeat("x", 512))},
	}
	records, ok := encodeVolgaV6Records(frame)
	if !ok {
		t.Fatal("could not encode logical frame")
	}
	_, err := carrier.buildRelayBody(records)
	if !errors.Is(err, errVolgaV6BodyTooLarge) {
		t.Fatalf("body-limit error=%v want %v", err, errVolgaV6BodyTooLarge)
	}
}

func TestVolgaV6YandexWSSelfActionUpdatesFrontierWithoutLoopbackData(t *testing.T) {
	deliveries := 0
	carrier := newOfflineVolgaV6YandexCarrier(t, 3, 8000)
	carrier.onFrame = func(volgaV6WireFrame) { deliveries++ }
	ws := newVolgaV6YandexWS(carrier, &volgaAuth{UserID: 4242})

	action, _ := json.Marshal(map[string]any{"id": "1-4242.77", "actionName": "textInsert"})
	ws.handleBundleItem(action, false)
	if got := carrier.frontierSnapshot(); !reflect.DeepEqual(got, []interface{}{"1-4242.77"}) {
		t.Fatalf("frontier=%v", got)
	}
	if deliveries != 0 {
		t.Fatalf("self action looped into logical delivery count=%d", deliveries)
	}
}

func TestVolgaV6YandexWSDecodesRemoteLogicalFrame(t *testing.T) {
	var got []volgaV6WireFrame
	carrier := newOfflineVolgaV6YandexCarrier(t, 4, 8000)
	carrier.onFrame = func(frame volgaV6WireFrame) { got = append(got, frame) }
	ws := newVolgaV6YandexWS(carrier, &volgaAuth{UserID: 4242})

	want := volgaV6WireFrame{
		Kind:    volgaV6FrameData,
		Session: 333,
		Seq:     5,
		Floor:   4,
		Payload: [][]byte{[]byte("remote-data")},
	}
	records, ok := encodeVolgaV6Records(want)
	if !ok {
		t.Fatal("could not encode logical frame")
	}
	encoded, err := encodeVolgaV6BatchRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := json.Marshal(string(encoded))
	ws.handleBundleItem(item, true)
	if len(got) != 1 {
		t.Fatalf("deliveries=%d want 1", len(got))
	}
	if got[0].Session != want.Session || got[0].Seq != want.Seq || got[0].Floor != want.Floor || !reflect.DeepEqual(got[0].Payload, want.Payload) {
		t.Fatalf("decoded=%+v want=%+v", got[0], want)
	}
}

func TestVolgaV6YandexWSDecodesRemoteAckFrame(t *testing.T) {
	var got []volgaV6WireFrame
	carrier := newOfflineVolgaV6YandexCarrier(t, 5, 8000)
	carrier.onFrame = func(frame volgaV6WireFrame) { got = append(got, frame) }
	ws := newVolgaV6YandexWS(carrier, &volgaAuth{UserID: 4242})

	want := volgaV6WireFrame{
		Kind: volgaV6FrameAck,
		Ack: volgaV6Ack{
			Session: 444,
			Base:    7,
			Ranges:  []volgaV6AckRange{{Start: 9, End: 11}},
		},
	}
	records, ok := encodeVolgaV6Records(want)
	if !ok {
		t.Fatal("could not encode ACK frame")
	}
	encoded, err := encodeVolgaV6BatchRecords(records)
	if err != nil {
		t.Fatal(err)
	}
	item, _ := json.Marshal(string(encoded))
	ws.handleBundleItem(item, true)
	if len(got) != 1 || !reflect.DeepEqual(got[0].Ack, want.Ack) {
		t.Fatalf("decoded=%+v want=%+v", got, want)
	}
}

func TestVolgaV6YandexWSStopWithoutConnectionIsIdempotent(t *testing.T) {
	carrier := newOfflineVolgaV6YandexCarrier(t, 6, 8000)
	ws := newVolgaV6YandexWS(carrier, &volgaAuth{UserID: 4242})
	ws.Stop()
	ws.Stop()
	if ws.IsConnected() {
		t.Fatal("stopped offline websocket reports connected")
	}
}

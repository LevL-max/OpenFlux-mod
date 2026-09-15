package yandex

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

func TestDefaultVolgaConfig(t *testing.T) {
	cfg := DefaultVolgaConfig()

	if cfg.WorkerCount != 2000 {
		t.Fatalf("WorkerCount=%d want 2000", cfg.WorkerCount)
	}
	if cfg.QueueSize != 1_000_000 {
		t.Fatalf("QueueSize=%d want 1000000", cfg.QueueSize)
	}
	if cfg.BatchSize != 20 {
		t.Fatalf("BatchSize=%d want 20", cfg.BatchSize)
	}
	if cfg.BatchTimeout != 2*time.Millisecond {
		t.Fatalf("BatchTimeout=%v want 2ms", cfg.BatchTimeout)
	}
	if cfg.BatchMaxBytes != 5000 {
		t.Fatalf("BatchMaxBytes=%d want 5000", cfg.BatchMaxBytes)
	}
	if cfg.ReconnectMinDelay != 500*time.Millisecond {
		t.Fatalf("ReconnectMinDelay=%v want 500ms", cfg.ReconnectMinDelay)
	}
	if cfg.ReconnectMaxDelay != 30*time.Second {
		t.Fatalf("ReconnectMaxDelay=%v want 30s", cfg.ReconnectMaxDelay)
	}
	if cfg.ReconnectMultiplier != 1.5 {
		t.Fatalf("ReconnectMultiplier=%v want 1.5", cfg.ReconnectMultiplier)
	}
}

func TestDecodeBatch(t *testing.T) {
	packets := [][]byte{
		{0x01, 0x02, 0x03},
		[]byte("openflux-volga"),
	}

	var encoded bytes.Buffer
	var lenBuf [2]byte
	for _, p := range packets {
		binary.BigEndian.PutUint16(lenBuf[:], uint16(len(p)))
		encoded.Write(lenBuf[:])
		encoded.Write(p)
	}

	got := decodeBatch(encoded.Bytes())
	if len(got) != len(packets) {
		t.Fatalf("decoded %d packets want %d", len(got), len(packets))
	}
	for i := range packets {
		if !bytes.Equal(got[i], packets[i]) {
			t.Fatalf("packet %d=%x want %x", i, got[i], packets[i])
		}
	}
}

func TestFormatTTL(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{name: "json number", in: json.Number("123456"), want: "123456"},
		{name: "float", in: float64(42), want: "42"},
		{name: "string", in: "98765", want: "98765"},
		{name: "nil", in: nil, want: "0"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatTTL(tc.in); got != tc.want {
				t.Fatalf("formatTTL(%v)=%q want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestNewYandexVolgaTransport(t *testing.T) {
	tr := NewYandexVolgaTransport("https://disk.yandex.ru/i/test", transport.TransportConfig{})
	if tr == nil {
		t.Fatal("transport is nil")
	}
	if tr.docURL != "https://disk.yandex.ru/i/test" {
		t.Fatalf("docURL=%q", tr.docURL)
	}
}

package mailru

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"universal-bypass-tool/transport"
)

type memoryBlocks struct {
	mu        sync.Mutex
	data      map[string][]byte
	corrupted bool
	block     bool
	started   chan struct{}
	once      sync.Once
}

func (m *memoryBlocks) Put(ctx context.Context, id string, data []byte) error {
	if m.block {
		m.once.Do(func() { close(m.started) })
		<-ctx.Done()
		return ctx.Err()
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[id] = append([]byte(nil), data...)
	return nil
}
func (m *memoryBlocks) Get(ctx context.Context, id string, size int) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	data, ok := m.data[id]
	if !ok {
		return nil, errors.New("missing")
	}
	out := append([]byte(nil), data...)
	if !m.corrupted {
		m.corrupted = true
		out[len(out)-1] ^= 1
	}
	return out, nil
}
func (m *memoryBlocks) Delete(ctx context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, id)
	return nil
}

type testChat struct {
	*transport.BaseTransport
	peer    *testChat
	mu      sync.Mutex
	dropped map[byte]bool
	faults  bool
}

func (c *testChat) Start() error { c.SetConnected(true); return c.BaseTransport.Start() }
func (c *testChat) Send(data []byte) error {
	c.mu.Lock()
	drop := c.faults && len(data) > 4 && data[4] != hybridInline && !c.dropped[data[4]]
	if drop {
		c.dropped[data[4]] = true
	}
	c.mu.Unlock()
	if !drop {
		c.peer.CallReceive(append([]byte(nil), data...))
	}
	return nil
}
func chatPair() (*testChat, *testChat) {
	a := &testChat{BaseTransport: transport.NewBaseTransport(transport.DefaultConfig()), dropped: map[byte]bool{}}
	b := &testChat{BaseTransport: transport.NewBaseTransport(transport.DefaultConfig()), dropped: map[byte]bool{}}
	a.peer = b
	b.peer = a
	return a, b
}
func testIP(sequence, payload int) []byte {
	p := make([]byte, 40+payload)
	p[0] = 0x45
	p[9] = 6
	binary.BigEndian.PutUint16(p[2:4], uint16(len(p)))
	copy(p[12:20], []byte{10, 10, 10, 2, 1, 1, 1, 1})
	binary.BigEndian.PutUint16(p[20:22], 1234)
	binary.BigEndian.PutUint16(p[22:24], 443)
	binary.BigEndian.PutUint32(p[24:28], uint32(sequence))
	p[32] = 0x50
	p[33] = 0x10
	for i := 40; i < len(p); i++ {
		p[i] = byte(sequence + i)
	}
	return p
}
func TestHybridLostNoticeAckAndCorruption(t *testing.T) {
	a, b := chatPair()
	a.faults = true
	b.faults = true
	store := &memoryBlocks{data: map[string][]byte{}}
	left, right := newHybridWithStore(a, store), newHybridWithStore(b, store)
	defer left.Stop()
	defer right.Stop()
	received := make(chan []byte, 256)
	right.Receive(func(p []byte) { received <- append([]byte(nil), p...) })
	if err := right.Start(); err != nil {
		t.Fatal(err)
	}
	if err := left.Start(); err != nil {
		t.Fatal(err)
	}
	const count = 128
	for i := 0; i < count; i++ {
		if err := left.Send(testIP(i, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[int]bool{}
	deadline := time.After(7 * time.Second)
	for len(seen) < count {
		select {
		case p := <-received:
			index := int(binary.BigEndian.Uint32(p[24:28]))
			if seen[index] || !bytes.Equal(p, testIP(index, 1024)) {
				t.Fatal("duplicate or corrupt IP delivery")
			}
			seen[index] = true
		case <-deadline:
			t.Fatalf("only %d/%d IP packets delivered", len(seen), count)
		}
	}
	until := time.Now().Add(4 * time.Second)
	for time.Now().Before(until) {
		store.mu.Lock()
		remaining := len(store.data)
		store.mu.Unlock()
		if remaining == 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	store.mu.Lock()
	remaining := len(store.data)
	store.mu.Unlock()
	if remaining != 0 {
		t.Fatal("ack retry did not remove object")
	}
	if right.errors.Load() == 0 || left.txFiles.Load() == 0 || right.rxFiles.Load() == 0 {
		t.Fatal("faults/bulk path not exercised")
	}
	select {
	case <-received:
		t.Fatal("replayed file injected twice")
	case <-time.After(50 * time.Millisecond):
	}
}
func TestHybridStopCancelsUpload(t *testing.T) {
	a, _ := chatPair()
	store := &memoryBlocks{data: map[string][]byte{}, block: true, started: make(chan struct{})}
	h := newHybridWithStore(a, store)
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := h.Send(testIP(i, 1024)); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-store.started:
	case <-time.After(time.Second):
		t.Fatal("no upload")
	}
	done := make(chan struct{})
	go func() { h.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel file request")
	}
	if h.Send(testIP(9, 10)) == nil {
		t.Fatal("accepted send after stop")
	}
}
func TestHybridAckCoalescingPreservesLatest(t *testing.T) {
	a, b := chatPair()
	store := &memoryBlocks{data: map[string][]byte{}, corrupted: true}
	h := newHybridWithStore(a, store)
	defer h.Stop()
	received := make(chan []byte, 128)
	b.Receive(func(frame []byte) { received <- append([]byte(nil), frame...) })
	if err := h.Start(); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if err := h.Send(testIP(i, 0)); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.After(time.Second)
	for {
		select {
		case frame := <-received:
			if len(frame) != 45 {
				t.Fatal("not inline ACK")
			}
			if binary.BigEndian.Uint32(frame[29:33]) == 99 {
				return
			}
		case <-deadline:
			t.Fatal("latest ACK lost")
		}
	}
}
func TestHybridBoundsAndPaths(t *testing.T) {
	for _, raw := range []string{"http://cloud.mail.ru/a", "https://cloud.mail.ru.evil/a", "https://cloud.mail.ru@evil/a", "https://cloud.mail.ru:444/a"} {
		if cloudURL(raw, true) {
			t.Fatalf("accepted %q", raw)
		}
	}
	if !cloudURL("https://cloclo1.datacloudmail.ru/a", true) || cloudURL("https://cloclo1.datacloudmail.ru/a", false) {
		t.Fatal("wrong download-only scope")
	}
	store := newCloudStore("/unused", "https://cloud.mail.ru/public/test/document")
	if _, err := store.path("../../user-file"); err == nil {
		t.Fatal("path traversal")
	}
	if _, err := decodeHybridBlock([]byte{0, 40, 1}); err == nil {
		t.Fatal("truncated block")
	}
	if _, _, _, ok := tcpInfo([]byte{0x45}); ok {
		t.Fatal("short packet")
	}
}

func TestDispatcherCookieScope(t *testing.T) {
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(cloudBase)
	jar.SetCookies(base, []*http.Cookie{{Name: "session", Value: "test", Secure: true, Path: "/"}})
	for _, target := range []string{"https://upload1.cloud.mail.ru/upload", "https://download.cloud.mail.ru/get", "https://cloclo1.datacloudmail.ru/a", "https://cloud.mail.ru.evil/a"} {
		seedCloudCookies(jar, base, target)
		u, _ := url.Parse(target)
		if got := len(jar.Cookies(u)) > 0; got != cloudURL(target, false) {
			t.Fatalf("wrong cookie scope for %s", u.Host)
		}
	}
}

type cloudTestRoundTrip func(*http.Request) (*http.Response, error)

func (f cloudTestRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUploadCreatedResponseRegistersFile(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "profile.json")
	raw := []byte(`{"cookies":{"session":"test"}}`)
	if err := os.WriteFile(profile, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, status := range []int{200, 201, 403} {
		s := newCloudStore(profile, "test/document")
		registered := false
		s.state = &cloudState{profile: sha256.Sum256(raw), expires: time.Now().Add(time.Minute), upload: "https://upload.cloud.mail.ru/", client: &http.Client{Transport: cloudTestRoundTrip(func(r *http.Request) (*http.Response, error) {
			code, body := status, strings.Repeat("a", 40)+";4"
			if r.Method == "POST" {
				registered = true
				code, body = 200, `{"status":200}`
			}
			return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})}}
		err := s.Put(context.Background(), strings.Repeat("a", 32)+"-"+strings.Repeat("b", 16), []byte("test"))
		if (err == nil) != (status != 403) || registered != (status != 403) {
			t.Fatalf("status %d: registered=%v error=%v", status, registered, err)
		}
	}
}

// Package recordconn orders the reliable, but out-of-order, V6 records into a
// bounded byte stream. Reliability/retransmission stays in V6. This experimental
// wire format is deliberately separate from the Legacy and IP packet formats.
package recordconn

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// Window is the default sender credit window.
	Window = 512 << 10
	// MaxWindow bounds what a receiver buffers. A receiver accepts any sender
	// window up to this bound, so the two peers need not share a window setting;
	// each sender still self-limits to its own configured window.
	MaxWindow = 8 << 20
	// Chunk is the default useful bytes sealed into one V6 record. One record
	// fills one relay POST, so this is chosen as large as possible while the
	// minimal relay body stays safely under the 8000-byte limit (a Chunk+74
	// record yields ~7676 body at 5600, versus the observed ~8294 HTTP 413
	// point). The carrier is POST-rate-bound, so more useful bytes per POST is
	// a direct throughput gain with no extra requests.
	Chunk = 5600
	// MaxChunk bounds a received record, so a sender may choose its own chunk.
	MaxChunk = 16 << 10
	// FlushDelay is how long a partial record waits for more writes before it is
	// sent anyway. Yamux writes each 12-byte frame header with a separate Write;
	// without coalescing every header became its own record and its own relay
	// POST. Merging adjacent writes into full records cuts POSTs per byte.
	FlushDelay = time.Millisecond
	// ServerAnnounce is how long a new server epoch sends hellos. A client
	// still bound to the previous epoch learns of the restart at once; after
	// that an idle server stays silent until a client hello arrives, so it has
	// nothing for the reliable carrier to replay.
	ServerAnnounce = 3 * time.Second
	headerSize     = 46
	maxPending     = 8192
)

// Options tune the sending side. Zero values select the package defaults; a
// negative FlushDelay disables coalescing (one record per Write, as in 0.1.2).
type Options struct {
	Chunk      int
	Window     int
	FlushDelay time.Duration
	Announce   time.Duration // server hello period of a new epoch; zero is ServerAnnounce
}

var ErrPeerRestart = errors.New("record stream: peer session changed")
var ErrProtocol = errors.New("record stream: invalid authenticated record")

type Epoch [16]byte
type SendFunc func(context.Context, []byte) error

// Conn requires a SendFunc that observes cancellation and takes ownership of
// the supplied bytes only on success. Receive must be called for incoming V6
// records; it never sends or waits for the carrier.
type Conn struct {
	ctx                               context.Context
	cancel                            context.CancelFunc
	send                              SendFunc
	aead                              cipher.AEAD
	role                              byte
	local                             Epoch
	mu                                sync.Mutex
	readMu, writeMu                   sync.Mutex
	peer                              Epoch
	ready                             bool
	err                               error
	changed                           chan struct{}
	control                           chan struct{}
	done                              chan struct{}
	pending                           map[uint64][]byte
	readOffset, writeOffset, peerRead uint64
	buffered, peak                    int
	readDeadline, writeDeadline       time.Time

	// Sending side. chunk/window/flushDelay are set once at construction and
	// then read-only. stage is guarded by writeMu.
	chunk, window  int
	flushDelay     time.Duration
	announce       time.Duration
	stage          []byte
	kick           chan struct{}
	dataRecords    atomic.Uint64
	partialRecords atomic.Uint64
	timerFlushes   atomic.Uint64
	workers        sync.WaitGroup
}

type Stats struct {
	Ready          bool   `json:"ready"`
	ReadBytes      uint64 `json:"read_bytes"`
	WrittenBytes   uint64 `json:"written_bytes"`
	PeerReadBytes  uint64 `json:"peer_read_bytes"`
	BufferedBytes  int    `json:"buffered_bytes"`
	PeakBytes      int    `json:"peak_bytes"`
	Chunk          int    `json:"chunk"`
	Window         int    `json:"window"`
	Records        uint64 `json:"data_records"`
	PartialRecords uint64 `json:"partial_records"`
	TimerFlushes   uint64 `json:"timer_flushes"`
}

func New(ctx context.Context, key []byte, client bool, send SendFunc) (*Conn, error) {
	return NewWithOptions(ctx, key, client, send, Options{})
}

func NewWithOptions(ctx context.Context, key []byte, client bool, send SendFunc, o Options) (*Conn, error) {
	if len(key) != 32 || send == nil {
		return nil, errors.New("record stream requires a 32-byte shared key and sender")
	}
	if o.Chunk == 0 {
		o.Chunk = Chunk
	}
	if o.Window == 0 {
		o.Window = Window
	}
	// A negative FlushDelay explicitly disables coalescing; zero selects the
	// default. Normalize the disabled case to 0 for the send path.
	if o.FlushDelay == 0 {
		o.FlushDelay = FlushDelay
	} else if o.FlushDelay < 0 {
		o.FlushDelay = 0
	}
	if o.Announce <= 0 {
		o.Announce = ServerAnnounce
	}
	if o.Chunk < 256 || o.Chunk > MaxChunk || o.Window < 4*o.Chunk || o.Window > MaxWindow {
		return nil, errors.New("record stream: chunk must be 256-16384 bytes and window 4 chunks to 8 MiB")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	c := &Conn{ctx: ctx, cancel: cancel, send: send, aead: aead, role: 2,
		changed: make(chan struct{}), control: make(chan struct{}, 1), done: make(chan struct{}), pending: make(map[uint64][]byte),
		chunk: o.Chunk, window: o.Window, flushDelay: o.FlushDelay, announce: o.Announce, kick: make(chan struct{}, 1)}
	if client {
		c.role = 1
	}
	if _, err = rand.Read(c.local[:]); err != nil {
		cancel()
		return nil, err
	}
	go c.controls()
	if c.flushDelay > 0 {
		c.workers.Add(1)
		go c.flusher()
	}
	return c, nil
}

func (c *Conn) notifyLocked() { close(c.changed); c.changed = make(chan struct{}) }
func (c *Conn) signal() {
	select {
	case c.control <- struct{}{}:
	default:
	}
}
func (c *Conn) failLocked(err error) {
	if c.err == nil {
		c.err = err
		c.cancel()
		c.notifyLocked()
	}
}
func (c *Conn) Close() error          { c.mu.Lock(); c.failLocked(net.ErrClosed); c.mu.Unlock(); return nil }
func (c *Conn) Done() <-chan struct{} { return c.done }
func (c *Conn) Peer() Epoch           { c.mu.Lock(); defer c.mu.Unlock(); return c.peer }
func (c *Conn) Stats() Stats {
	c.mu.Lock()
	s := Stats{Ready: c.ready, ReadBytes: c.readOffset, WrittenBytes: c.writeOffset,
		PeerReadBytes: c.peerRead, BufferedBytes: c.buffered, PeakBytes: c.peak,
		Chunk: c.chunk, Window: c.window}
	c.mu.Unlock()
	s.Records = c.dataRecords.Load()
	s.PartialRecords = c.partialRecords.Load()
	s.TimerFlushes = c.timerFlushes.Load()
	return s
}

func (c *Conn) seal(kind byte, target Epoch, off uint64, p []byte) ([]byte, error) {
	head := make([]byte, headerSize+c.aead.NonceSize())
	copy(head, "VLS1")
	head[4] = c.role
	head[5] = kind
	copy(head[6:22], c.local[:])
	copy(head[22:38], target[:])
	binary.BigEndian.PutUint64(head[38:46], off)
	if _, err := rand.Read(head[headerSize:]); err != nil {
		return nil, err
	}
	return c.aead.Seal(head, head[headerSize:], p, head[:headerSize]), nil
}

// Identify authenticates without changing state. The runner uses it to ignore
// retired peer epochs after a session restart. Plain provider broadcasts and
// records encrypted with a different key are ignored, never fed to Yamux.
func (c *Conn) Identify(p []byte) (Epoch, bool) {
	peer, _, _, _, _, ok := c.open(p)
	return peer, ok
}
func (c *Conn) open(p []byte) (peer, target Epoch, kind byte, off uint64, data []byte, ok bool) {
	if len(p) < headerSize+c.aead.NonceSize()+c.aead.Overhead() || len(p) > headerSize+12+16+MaxChunk || string(p[:4]) != "VLS1" || p[4] != 3-c.role {
		return
	}
	data, err := c.aead.Open(nil, p[headerSize:headerSize+c.aead.NonceSize()], p[headerSize+c.aead.NonceSize():], p[:headerSize])
	if err != nil {
		return peer, target, kind, off, nil, false
	}
	copy(peer[:], p[6:22])
	copy(target[:], p[22:38])
	kind = p[5]
	off = binary.BigEndian.Uint64(p[38:46])
	ok = peer != (Epoch{})
	return
}

func (c *Conn) Receive(p []byte) {
	peer, target, kind, off, data, ok := c.open(p)
	if !ok {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.err != nil {
		return
	}
	if kind == 'H' {
		if target != (Epoch{}) || off != 0 || len(data) != 0 {
			return
		}
		if c.peer != peer && c.ready {
			c.failLocked(ErrPeerRestart)
			return
		}
		c.peer = peer
		c.signal()
		return
	}
	if target != c.local || peer != c.peer {
		return
	}
	switch kind {
	case 'K':
		if off != 0 || len(data) != 0 {
			c.failLocked(ErrProtocol)
			return
		}
		c.ready = true
	case 'A':
		if len(data) != 0 || off > c.writeOffset {
			c.failLocked(ErrProtocol)
			return
		}
		if off > c.peerRead {
			c.peerRead = off
		}
	case 'D':
		// Authenticated data targeted at this epoch also confirms the handshake.
		// K and D may arrive out of order on different physical lanes. Bounds use
		// MaxWindow so a peer with a larger configured window interoperates; the
		// sender still self-limits to its own window.
		c.ready = true
		end := off + uint64(len(data))
		if len(data) == 0 || end < off || end > c.readOffset+MaxWindow {
			c.failLocked(ErrProtocol)
			return
		}
		if end <= c.readOffset {
			return
		}
		if off < c.readOffset {
			data = data[c.readOffset-off:]
			off = c.readOffset
		}
		if old, exists := c.pending[off]; exists {
			if !bytes.Equal(old, data) {
				c.failLocked(ErrProtocol)
			}
			return
		}
		// Bound metadata as well as bytes; overlaps must not poison the map.
		if len(c.pending) >= maxPending || c.buffered+len(data) > MaxWindow {
			c.failLocked(ErrProtocol)
			return
		}
		for start, old := range c.pending {
			if off < start+uint64(len(old)) && start < end {
				c.failLocked(ErrProtocol)
				return
			}
		}
		c.pending[off] = data
		c.buffered += len(data)
		c.peak = max(c.peak, c.buffered)
	default:
		c.failLocked(ErrProtocol)
		return
	}
	c.notifyLocked()
}

func (c *Conn) sendRecord(ctx context.Context, kind byte, target Epoch, off uint64, p []byte) error {
	b, err := c.seal(kind, target, off, p)
	if err == nil {
		err = c.send(ctx, b)
	}
	return err
}

func (c *Conn) controls() {
	defer close(c.done)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	var helloAt, ackAt time.Time
	var creditAt time.Time
	var reason error
	var sentCredit uint64
	born := time.Now()
	for {
		c.mu.Lock()
		peer, ready, read, err := c.peer, c.ready, c.readOffset, c.err
		c.mu.Unlock()
		if err != nil {
			return
		}
		now := time.Now()
		// A client announces itself until ready; a server only for its announce
		// period or in answer to a client hello (see ServerAnnounce).
		announce := c.role == 1 || peer != (Epoch{}) || now.Sub(born) < c.announce
		if !ready && announce && now.Sub(helloAt) >= 300*time.Millisecond {
			if err = c.sendRecord(c.ctx, 'H', Epoch{}, 0, nil); err != nil {
				reason = err
				break
			}
			helloAt = now
		}
		if peer != (Epoch{}) && (!ready || now.Sub(ackAt) >= time.Second) {
			if err = c.sendRecord(c.ctx, 'K', peer, 0, nil); err != nil {
				reason = err
				break
			}
			ackAt = now
		}
		if ready && read != sentCredit && now.Sub(creditAt) >= 20*time.Millisecond {
			if err = c.sendRecord(c.ctx, 'A', peer, read, nil); err != nil {
				reason = err
				break
			}
			sentCredit = read
			creditAt = now
		}
		select {
		case <-c.ctx.Done():
			reason = c.ctx.Err()
		case <-tick.C:
			continue
		case <-c.control:
			continue
		}
		break
	}
	if reason == nil {
		reason = io.ErrClosedPipe
	}
	c.mu.Lock()
	c.failLocked(reason)
	c.mu.Unlock()
}

// flusher sends a partial staged record after a period of write inactivity, so
// a sub-chunk tail is never left undelivered while the stream is idle.
func (c *Conn) flusher() {
	defer c.workers.Done()
	timer := time.NewTimer(c.flushDelay)
	if !timer.Stop() {
		<-timer.C
	}
	armed := false
	for {
		select {
		case <-c.ctx.Done():
			if armed && !timer.Stop() {
				<-timer.C
			}
			return
		case <-c.kick:
			if !armed {
				timer.Reset(c.flushDelay)
				armed = true
			}
		case <-timer.C:
			armed = false
			c.flushTail()
		}
	}
}

func (c *Conn) kickFlusher() {
	select {
	case c.kick <- struct{}{}:
	default:
	}
}

func (c *Conn) flushTail() {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if len(c.stage) == 0 {
		return
	}
	sent := false
	for len(c.stage) > 0 {
		n := min(len(c.stage), c.chunk)
		if err := c.sendData(c.stage[:n]); err != nil {
			// Write already reported these bytes accepted. An asynchronous
			// failure must wake the session, not silently strand its tail.
			c.mu.Lock()
			c.failLocked(err)
			c.mu.Unlock()
			return
		}
		c.stage = c.stage[n:]
		sent = true
	}
	c.stage = c.stage[:0]
	if sent {
		c.timerFlushes.Add(1)
	}
}

func wait(ctx context.Context, changed <-chan struct{}, deadline time.Time) error {
	if !deadline.IsZero() {
		left := time.Until(deadline)
		if left <= 0 {
			return os.ErrDeadlineExceeded
		}
		timer := time.NewTimer(left)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
			return nil
		case <-timer.C:
			return os.ErrDeadlineExceeded
		}
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-changed:
		return nil
	}
}
func (c *Conn) Handshake(ctx context.Context) error {
	for {
		c.mu.Lock()
		ready, err, ch := c.ready, c.err, c.changed
		c.mu.Unlock()
		if err != nil {
			return err
		}
		if ready {
			return nil
		}
		if err = wait(ctx, ch, time.Time{}); err != nil {
			return err
		}
	}
}
func (c *Conn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	c.readMu.Lock()
	defer c.readMu.Unlock()
	for {
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return 0, err
		}
		if !c.readDeadline.IsZero() && !time.Now().Before(c.readDeadline) {
			c.mu.Unlock()
			return 0, os.ErrDeadlineExceeded
		}
		if b, ok := c.pending[c.readOffset]; ok {
			delete(c.pending, c.readOffset)
			n := copy(p, b)
			c.readOffset += uint64(n)
			c.buffered -= n
			if n < len(b) {
				c.pending[c.readOffset] = b[n:]
			}
			c.mu.Unlock()
			c.signal()
			return n, nil
		}
		ch, deadline := c.changed, c.readDeadline
		c.mu.Unlock()
		if err := wait(c.ctx, ch, deadline); err != nil {
			c.mu.Lock()
			closed := c.err
			c.mu.Unlock()
			if closed != nil {
				return 0, closed
			}
			return 0, err
		}
	}
}

// watchDeadline cancels an in-flight carrier enqueue when the write deadline
// changes or expires, so a bounded carrier queue cannot block past the deadline.
func (c *Conn) watchDeadline(ctx context.Context, cancel context.CancelFunc, stop chan struct{}) {
	for {
		c.mu.Lock()
		ch, d := c.changed, c.writeDeadline
		c.mu.Unlock()
		var timer *time.Timer
		var expired <-chan time.Time
		if !d.IsZero() {
			timer = time.NewTimer(max(time.Until(d), 0))
			expired = timer.C
		}
		select {
		case <-stop:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-expired:
			cancel()
			return
		case <-ch:
			if timer != nil {
				timer.Stop()
			}
		}
	}
}

// sendData emits exactly one authenticated data record. The caller holds
// writeMu, which serializes offset assignment across Write and flushTail.
func (c *Conn) sendData(p []byte) error {
	for {
		c.mu.Lock()
		if c.err != nil {
			err := c.err
			c.mu.Unlock()
			return err
		}
		deadline := c.writeDeadline
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			c.mu.Unlock()
			return os.ErrDeadlineExceeded
		}
		if !c.ready || c.writeOffset+uint64(len(p)) > c.peerRead+uint64(c.window) {
			ch := c.changed
			c.mu.Unlock()
			if err := wait(c.ctx, ch, deadline); err != nil {
				return err
			}
			continue
		}
		off, peer := c.writeOffset, c.peer
		c.writeOffset += uint64(len(p))
		c.mu.Unlock()
		ctx, cancel := context.WithCancel(c.ctx)
		stop := make(chan struct{})
		go c.watchDeadline(ctx, cancel, stop)
		err := c.sendRecord(ctx, 'D', peer, off, p)
		close(stop)
		cancel()
		if err != nil {
			c.mu.Lock()
			if !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
				err = os.ErrDeadlineExceeded
			}
			c.failLocked(err)
			c.mu.Unlock()
			return err
		}
		c.dataRecords.Add(1)
		if len(p) < c.chunk {
			c.partialRecords.Add(1)
		}
		return nil
	}
}

// writeDirect sends each chunk immediately with no staging (coalescing off).
func (c *Conn) writeDirect(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := min(len(p), c.chunk)
		if err := c.sendData(p[:n]); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

func (c *Conn) Write(p []byte) (int, error) {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.flushDelay <= 0 {
		return c.writeDirect(p)
	}
	// Coalesce adjacent writes: fill the stage to a full chunk before sending,
	// so a 12-byte Yamux header and its following body share relay POSTs. A byte
	// of p is only counted consumed once it is sent in a full chunk or accepted
	// as the trailing partial record for deferred flush.
	consumed := 0
	for {
		// A short write may never call sendData, so validate before staging.
		c.mu.Lock()
		err := c.err
		if err == nil {
			err = c.ctx.Err()
		}
		if err == nil && !c.writeDeadline.IsZero() && !time.Now().Before(c.writeDeadline) {
			err = os.ErrDeadlineExceeded
		}
		c.mu.Unlock()
		if err != nil {
			return consumed, err
		}
		for len(c.stage) < c.chunk && consumed < len(p) {
			take := min(c.chunk-len(c.stage), len(p)-consumed)
			c.stage = append(c.stage, p[consumed:consumed+take]...)
			consumed += take
		}
		if len(c.stage) < c.chunk {
			if len(c.stage) > 0 {
				c.kickFlusher()
			}
			return consumed, nil
		}
		if err := c.sendData(c.stage[:c.chunk]); err != nil {
			// Remove this call's unaccepted bytes before a caller retries.
			// Retain only a tail already accepted by an earlier Write.
			unsent := min(consumed, len(c.stage))
			c.stage = c.stage[:len(c.stage)-unsent]
			return consumed - unsent, err
		}
		c.stage = c.stage[c.chunk:]
	}
}
func (c *Conn) SetDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.writeDeadline = t
	c.notifyLocked()
	return nil
}
func (c *Conn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.readDeadline = t
	c.notifyLocked()
	return nil
}
func (c *Conn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeDeadline = t
	c.notifyLocked()
	return nil
}

type addr string

func (a addr) Network() string       { return "volga-stream-v1" }
func (a addr) String() string        { return string(a) }
func (c *Conn) LocalAddr() net.Addr  { return addr("local") }
func (c *Conn) RemoteAddr() net.Addr { return addr("peer") }

// DeriveKey separates this experimental format from any other use of a secret.
func DeriveKey(secret []byte) []byte {
	h := hmac.New(sha256.New, secret)
	h.Write([]byte("openflux-volga-stream-v1"))
	return h.Sum(nil)
}

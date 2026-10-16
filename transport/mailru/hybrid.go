package mailru

// Hybrid is opt-in and wire-incompatible with chat-only peers. IP packets keep
// their end-to-end TCP sequencing; only file notifications/acknowledgements are
// retried here. The queues and object lifetime are bounded independently of TCP.
import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

var hybridMagic = []byte("OFH1")

const (
	hybridInline byte = iota
	hybridNotice
	hybridAck
)

type hybridPending struct {
	frame         []byte
	created, last time.Time
	deleting      bool
}
type flowBudget struct {
	used int
	last time.Time
}
type hybridDownload struct {
	id   string
	size int
	hash [32]byte
}
type HybridTransport struct {
	*transport.BaseTransport
	inner                                  transport.Transport
	store                                  blockStore
	ctx                                    context.Context
	cancel                                 context.CancelFunc
	sender                                 string
	seq                                    atomic.Uint64
	startOnce, stopOnce                    sync.Once
	wg                                     sync.WaitGroup
	packets                                chan []byte
	uploads                                chan []byte
	downloads                              chan hybridDownload
	deletes                                chan string
	control                                chan []byte
	mu                                     sync.Mutex
	pending                                map[string]*hybridPending
	inflight                               map[string]bool
	completed                              map[string]time.Time
	flows                                  map[string]flowBudget
	acks                                   map[string][]byte
	txFiles, rxFiles, errors, controlBytes atomic.Uint64
}

func NewHybridTransport(inner transport.Transport, profile, document string) (*HybridTransport, error) {
	if profile == "" {
		return nil, errors.New("Mail.ru hybrid requires a browser profile on both peers")
	}
	return newHybridWithStore(inner, newCloudStore(profile, document)), nil
}
func newHybridWithStore(inner transport.Transport, store blockStore) *HybridTransport {
	var random [16]byte
	_, err := rand.Read(random[:])
	if err != nil {
		panic("random source unavailable")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &HybridTransport{BaseTransport: transport.NewBaseTransport(transport.DefaultConfig()), inner: inner, store: store, ctx: ctx, cancel: cancel, sender: hex.EncodeToString(random[:]), packets: make(chan []byte, 4096), uploads: make(chan []byte, 16), downloads: make(chan hybridDownload, 32), deletes: make(chan string, 128), control: make(chan []byte, 1024), pending: map[string]*hybridPending{}, inflight: map[string]bool{}, completed: map[string]time.Time{}, flows: map[string]flowBudget{}, acks: map[string][]byte{}}
}
func (h *HybridTransport) Start() error {
	if h.ctx.Err() != nil {
		return errors.New("hybrid stopped")
	}
	if s, ok := h.store.(*cloudStore); ok {
		ctx, cancel := context.WithTimeout(h.ctx, 30*time.Second)
		_, err := s.session(ctx)
		cancel()
		if err != nil {
			return err
		}
	}
	var startErr error
	h.startOnce.Do(func() {
		h.inner.Receive(h.receiveFrame)
		if err := h.inner.Start(); err != nil {
			startErr = err
			return
		}
		h.BaseTransport.Start()
		launch := func(fn func()) { h.wg.Add(1); go func() { defer h.wg.Done(); fn() }() }
		launch(h.collect)
		launch(h.controlLoop)
		launch(h.maintenance)
		launch(h.deleteLoop)
		if store, ok := h.store.(*cloudStore); ok {
			launch(func() {
				tick := time.NewTicker(5 * time.Minute)
				defer tick.Stop()
				for {
					ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
					if store.Sweep(ctx) != nil {
						h.errors.Add(1)
					}
					cancel()
					select {
					case <-h.ctx.Done():
						return
					case <-tick.C:
					}
				}
			})
		}
		for i := 0; i < 4; i++ {
			launch(h.uploadLoop)
			launch(h.downloadLoop)
		}
		utils.Statusf("[M-HYBRID] ready lanes=4 block_bytes=%d inline_flow_bytes=4096", hybridBlockLimit)
	})
	return startErr
}
func (h *HybridTransport) Stop() error {
	h.stopOnce.Do(func() {
		h.BaseTransport.Stop()
		h.cancel()
		h.inner.Stop()
		h.wg.Wait()
		// Best effort cleanup of only this process's registered objects.
		h.mu.Lock()
		ids := make([]string, 0, len(h.pending))
		for id := range h.pending {
			ids = append(ids, id)
		}
		h.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		for _, id := range ids {
			if ctx.Err() != nil {
				break
			}
			_ = h.store.Delete(ctx, id)
		}
	})
	return nil
}
func (h *HybridTransport) IsConnected() bool               { return h.IsRunning() && h.inner.IsConnected() }
func (h *HybridTransport) Stats() transport.TransportStats { return h.inner.Stats() }

// cloudStore errors contain fixed descriptions/status codes, never request URLs.
func (h *HybridTransport) fileError(phase string, err error) {
	n := h.errors.Add(1)
	if h.ctx.Err() == nil && (n <= 8 || n%100 == 0) {
		utils.Statusf("[M-HYBRID] file_error phase=%s error=%v count=%d", phase, err, n)
	}
}

// Classify complete IPv4 TCP packets only. Control packets stay low latency.
// Pure ACKs are coalesced by flow, retaining the latest window/SACK information.
func tcpInfo(p []byte) (key string, payload int, flags byte, ok bool) {
	if len(p) < 40 || p[0]>>4 != 4 || p[9] != 6 || binary.BigEndian.Uint16(p[6:8])&0x3fff != 0 {
		return
	}
	ihl := int(p[0]&15) * 4
	total := int(binary.BigEndian.Uint16(p[2:4]))
	if ihl < 20 || total != len(p) || ihl+20 > total {
		return
	}
	thl := int(p[ihl+12]>>4) * 4
	if thl < 20 || ihl+thl > total {
		return
	}
	key = string(p[12:20]) + string(p[ihl:ihl+4])
	return key, total - ihl - thl, p[ihl+13], true
}
func (h *HybridTransport) Send(p []byte) error {
	if !h.IsRunning() {
		return errors.New("hybrid stopped")
	}
	if len(p) == 0 || len(p) > 65535 {
		return errors.New("invalid IP packet size")
	}
	key, payload, flags, ok := tcpInfo(p)
	inline := false
	h.mu.Lock()
	if ok {
		if payload == 0 && flags == 0x10 && len(h.acks) < 4096 {
			h.acks[key] = append([]byte(nil), p...)
			h.mu.Unlock()
			return nil
		}
		budget, known := h.flows[key]
		if flags&2 != 0 || time.Since(budget.last) > 2*time.Minute {
			budget.used = 0
		}
		inline = payload == 0 || flags&7 != 0 || budget.used+payload <= 4096
		budget.used += payload
		budget.last = time.Now()
		if len(h.flows) < 4096 || known {
			h.flows[key] = budget
		} else if payload > 0 && flags&7 == 0 {
			inline = false
		}
	}
	h.mu.Unlock()
	copyP := append([]byte(nil), p...)
	if inline {
		return h.queueControl(append(append([]byte(nil), hybridMagic...), append([]byte{hybridInline}, copyP...)...))
	}
	select {
	case h.packets <- copyP:
		return nil
	case <-h.ctx.Done():
		return h.ctx.Err()
	default:
		h.errors.Add(1)
		return errors.New("hybrid packet queue full")
	}
}
func (h *HybridTransport) queueControl(frame []byte) error {
	select {
	case h.control <- frame:
		return nil
	case <-h.ctx.Done():
		return h.ctx.Err()
	default:
		h.errors.Add(1)
		return errors.New("hybrid control queue full")
	}
}
func (h *HybridTransport) controlLoop() {
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	send := func(frame []byte) {
		if h.inner.Send(frame) != nil {
			h.errors.Add(1)
		} else {
			h.controlBytes.Add(uint64(len(frame)))
		}
	}
	for {
		select {
		case <-h.ctx.Done():
			return
		case frame := <-h.control:
			send(frame)
		case <-tick.C:
			h.mu.Lock()
			acks := h.acks
			h.acks = map[string][]byte{}
			h.mu.Unlock()
			for _, p := range acks {
				send(append(append([]byte(nil), hybridMagic...), append([]byte{hybridInline}, p...)...))
			}
		}
	}
}
func (h *HybridTransport) collect() {
	tick := time.NewTicker(80 * time.Millisecond)
	defer tick.Stop()
	var block []byte
	flush := func() {
		if len(block) == 0 {
			return
		}
		select {
		case h.uploads <- block:
		case <-h.ctx.Done():
		}
		block = nil
	}
	for {
		select {
		case <-h.ctx.Done():
			return
		case p := <-h.packets:
			if len(block)+2+len(p) > hybridBlockLimit {
				flush()
			}
			block = binary.BigEndian.AppendUint16(block, uint16(len(p)))
			block = append(block, p...)
			if len(block) >= hybridBlockLimit-65537 {
				flush()
			}
		case <-tick.C:
			flush()
		}
	}
}
func (h *HybridTransport) uploadLoop() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case data := <-h.uploads:
			h.mu.Lock()
			full := len(h.pending) >= 64
			h.mu.Unlock()
			if full {
				h.errors.Add(1)
				continue
			}
			id := fmt.Sprintf("%s-%016x", h.sender, h.seq.Add(1))
			digest := sha256.Sum256(data)
			frame := append(append([]byte(nil), hybridMagic...), hybridNotice)
			frame = append(frame, []byte(id)...)
			frame = binary.BigEndian.AppendUint32(frame, uint32(len(data)))
			frame = append(frame, digest[:]...)
			h.mu.Lock()
			h.pending[id] = &hybridPending{created: time.Now()}
			h.mu.Unlock()
			ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
			err := h.store.Put(ctx, id, data)
			cancel()
			if err != nil {
				h.fileError("upload", err)
				h.scheduleDelete(id)
				continue
			}
			h.txFiles.Add(1)
			h.mu.Lock()
			if p := h.pending[id]; p != nil {
				p.frame = frame
				p.last = time.Now()
			}
			h.mu.Unlock()
			_ = h.queueControl(frame)
		}
	}
}
func (h *HybridTransport) receiveFrame(frame []byte) {
	if !h.IsRunning() || len(frame) < 5 || !bytes.Equal(frame[:4], hybridMagic) {
		return
	}
	switch frame[4] {
	case hybridInline:
		if len(frame) > 5 && len(frame) <= 65540 {
			h.CallReceive(frame[5:])
		}
	case hybridAck:
		if len(frame) != 54 {
			return
		}
		id := string(frame[5:])
		h.mu.Lock()
		_, owned := h.pending[id]
		h.mu.Unlock()
		if owned {
			h.scheduleDelete(id)
		}
	case hybridNotice:
		if len(frame) != 90 {
			return
		}
		id := string(frame[5:54])
		if !hybridObjectID.MatchString(id) || stringsHasSender(id, h.sender) {
			return
		}
		size := int(binary.BigEndian.Uint32(frame[54:58]))
		if size <= 0 || size > hybridBlockLimit {
			return
		}
		h.mu.Lock()
		_, done := h.completed[id]
		busy := h.inflight[id]
		if !done && !busy && len(h.inflight) < 32 {
			h.inflight[id] = true
		} else if !done && !busy {
			h.mu.Unlock()
			return
		}
		h.mu.Unlock()
		if done {
			h.ack(id)
			return
		}
		if busy {
			return
		}
		task := hybridDownload{id: id, size: size}
		copy(task.hash[:], frame[58:])
		select {
		case h.downloads <- task:
		default:
			h.mu.Lock()
			delete(h.inflight, id)
			h.mu.Unlock()
		}
	}
}
func stringsHasSender(id, sender string) bool { return len(id) > 32 && id[:32] == sender }
func (h *HybridTransport) ack(id string) {
	_ = h.queueControl(append(append(append([]byte(nil), hybridMagic...), hybridAck), []byte(id)...))
}
func decodeHybridBlock(data []byte) ([][]byte, error) {
	var packets [][]byte
	for len(data) > 0 {
		if len(data) < 2 {
			return nil, errors.New("short hybrid block")
		}
		n := int(binary.BigEndian.Uint16(data))
		data = data[2:]
		if n < 20 || n > len(data) || len(packets) >= 16384 {
			return nil, errors.New("invalid hybrid block")
		}
		packets = append(packets, data[:n])
		data = data[n:]
	}
	return packets, nil
}
func (h *HybridTransport) downloadLoop() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case task := <-h.downloads:
			ctx, cancel := context.WithTimeout(h.ctx, 15*time.Second)
			data, err := h.store.Get(ctx, task.id, task.size)
			cancel()
			var packets [][]byte
			if err == nil && sha256.Sum256(data) != task.hash {
				err = errors.New("hybrid hash mismatch")
			}
			if err == nil {
				packets, err = decodeHybridBlock(data)
			}
			if err == nil {
				for _, p := range packets {
					h.CallReceive(p)
				}
				h.rxFiles.Add(1)
				h.mu.Lock()
				h.completed[task.id] = time.Now()
				h.mu.Unlock()
				h.ack(task.id)
			} else {
				h.fileError("download", err)
			}
			h.mu.Lock()
			delete(h.inflight, task.id)
			h.mu.Unlock()
		}
	}
}
func (h *HybridTransport) scheduleDelete(id string) {
	h.mu.Lock()
	p := h.pending[id]
	if p == nil || p.deleting {
		h.mu.Unlock()
		return
	}
	p.deleting = true
	h.mu.Unlock()
	select {
	case h.deletes <- id:
	default:
		h.mu.Lock()
		if p := h.pending[id]; p != nil {
			p.deleting = false
		}
		h.mu.Unlock()
	}
}
func (h *HybridTransport) deleteLoop() {
	for {
		select {
		case <-h.ctx.Done():
			return
		case id := <-h.deletes:
			ctx, cancel := context.WithTimeout(h.ctx, 10*time.Second)
			err := h.store.Delete(ctx, id)
			cancel()
			h.mu.Lock()
			if err == nil {
				delete(h.pending, id)
			} else if p := h.pending[id]; p != nil {
				p.deleting = false
				p.last = time.Now()
			}
			h.mu.Unlock()
			if err != nil {
				h.fileError("delete", err)
			}
		}
	}
}
func (h *HybridTransport) maintenance() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	count := 0
	for {
		select {
		case <-h.ctx.Done():
			return
		case now := <-tick.C:
			var resend [][]byte
			var expired []string
			h.mu.Lock()
			for id, p := range h.pending {
				if now.Sub(p.created) > 2*time.Minute {
					expired = append(expired, id)
				} else if !p.deleting && len(p.frame) > 0 && now.Sub(p.last) > time.Second {
					resend = append(resend, p.frame)
					p.last = now
				}
			}
			for id, at := range h.completed {
				if now.Sub(at) > 3*time.Minute {
					delete(h.completed, id)
				}
			}
			for id, flow := range h.flows {
				if now.Sub(flow.last) > 2*time.Minute {
					delete(h.flows, id)
				}
			}
			pending := len(h.pending)
			h.mu.Unlock()
			for _, frame := range resend {
				_ = h.queueControl(frame)
			}
			for _, id := range expired {
				h.scheduleDelete(id)
			}
			count++
			if count%5 == 0 {
				utils.Statusf("[M-HYBRID] tx_files=%d rx_files=%d pending=%d errors=%d control_bytes=%d", h.txFiles.Load(), h.rxFiles.Load(), pending, h.errors.Load(), h.controlBytes.Load())
			}
		}
	}
}

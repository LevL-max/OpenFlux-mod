package mailru

import (
	"crypto/sha256"
	"errors"
	"sync"
	"time"
)

// SetChatSessionBudget configures an opt-in bounded physical session. It never
// restarts the IP/TCP stack. Configure before Start, together with a smaller
// batch limit. A server echo proves chat acceptance, not remote TCP delivery.
func (t *MailruDocsTransport) SetChatSessionBudget(bytes int) error {
	if t.IsRunning() || bytes < 0 || bytes > 16<<20 || bytes > 0 && bytes < 65536 {
		return errors.New("invalid Mail.ru session budget: 0 or 65536..16777216; configure before start")
	}
	t.sessionByteBudget = bytes
	return nil
}

// Track only hashes, never retain payload copies. There can be at most one
// budget's worth of pending chat writes in a physical session.
type chatEchoes struct {
	mu      sync.Mutex
	pending map[[32]byte]int
	changed chan struct{}
}

func (e *chatEchoes) expect(message string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.pending == nil {
		e.pending = make(map[[32]byte]int)
		e.changed = make(chan struct{})
	}
	e.pending[sha256.Sum256([]byte(message))]++
}

func (e *chatEchoes) observe(message string) {
	hash := sha256.Sum256([]byte(message))
	e.mu.Lock()
	defer e.mu.Unlock()
	if n := e.pending[hash]; n > 0 {
		if n == 1 {
			delete(e.pending, hash)
		} else {
			e.pending[hash] = n - 1
		}
		close(e.changed)
		e.changed = make(chan struct{})
	}
}

// Missing echoes cannot strand the writer forever; ordinary TCP retransmission
// still handles any packet lost at the physical connection boundary.
func (e *chatEchoes) wait(done <-chan struct{}, timeout time.Duration) bool {
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		e.mu.Lock()
		empty, changed := len(e.pending) == 0, e.changed
		e.mu.Unlock()
		if empty {
			return true
		}
		select {
		case <-done:
			return false
		case <-timer.C:
			return true
		case <-changed:
		}
	}
}

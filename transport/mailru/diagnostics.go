package mailru

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"universal-bypass-tool/utils"
)

// One bounded metadata record per socket. Never retain frames, URLs, user IDs,
// close-reason text or err.Error(): any of those may contain browser credentials
// or document/chat contents. All strings below are locally defined categories.
type sessionReport struct {
	ID              uint64 `json:"id"`
	Phase           string `json:"phase"`
	Cause           string `json:"cause"`
	Error           string `json:"error,omitempty"`
	CloseCode       int    `json:"close_code,omitempty"`
	AgeMS           int64  `json:"age_ms"`
	ReadyMS         int64  `json:"ready_ms"`
	HeartbeatMS     int64  `json:"heartbeat_ms"`
	PingIntervalMS  int    `json:"ping_interval_ms"`
	PingTimeoutMS   int    `json:"ping_timeout_ms"`
	RXMessages      uint64 `json:"rx_messages"`
	RXBytes         uint64 `json:"rx_bytes"`
	TXMessages      uint64 `json:"tx_messages"`
	TXBytes         uint64 `json:"tx_bytes"`
	MaxTXBytes      int    `json:"max_tx_bytes"`
	LastTXBytes     int    `json:"last_tx_bytes"`
	FailedTXBytes   int    `json:"failed_tx_bytes,omitempty"`
	LastRXAgoMS     int64  `json:"last_rx_ago_ms"`
	Pings           uint64 `json:"pings"`
	Pongs           uint64 `json:"pongs"`
	LastPingAgoMS   int64  `json:"last_ping_ago_ms"`
	MaxWriteMS      int64  `json:"max_write_ms"`
	MaxWriteWaitMS  int64  `json:"max_write_wait_ms"`
	MaxPongMS       int64  `json:"max_pong_ms"`
	LastControl     string `json:"last_control,omitempty"`
	TerminalControl string `json:"terminal_control,omitempty"`
	EditorEvent     string `json:"editor_event,omitempty"`
	EditorCode      *int   `json:"editor_code,omitempty"`
}

type sessionTrace struct {
	mu       sync.Mutex
	report   sessionReport
	started  time.Time
	ready    time.Time
	lastRX   time.Time
	lastPing time.Time
	logOnce  sync.Once
}

func (d *sessionTrace) start(id uint64) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.started = time.Now()
	d.report = sessionReport{ID: id, Phase: "engine_handshake"}
}

func (d *sessionTrace) phase(name string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.report.Phase = name
	if name == "ready" {
		d.ready = time.Now()
	}
}

func sessionError(err error) (string, int) {
	var closeErr *websocket.CloseError
	var netErr net.Error
	switch {
	case err == nil:
		return "", 0
	case errors.As(err, &closeErr):
		return "websocket_close", closeErr.Code
	case errors.As(err, &netErr) && netErr.Timeout():
		return "timeout", 0
	case errors.Is(err, net.ErrClosed):
		return "socket_closed", 0
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "eof", 0
	case errors.Is(err, errEditorAuthRejected):
		return "auth_rejected", 0
	default:
		return "io_or_protocol", 0
	}
}

func (d *sessionTrace) fail(cause string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	// Preserve the initiating failure when closing the socket wakes its reader.
	if d.report.Cause == "" {
		d.report.Cause = cause
		d.report.Error, d.report.CloseCode = sessionError(err)
	}
}

func (d *sessionTrace) received(data []byte) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.lastRX = time.Now()
	d.report.RXMessages++
	d.report.RXBytes += uint64(len(data))
	// Engine.IO / Socket.IO control prefixes only. No server-supplied text is
	// copied into diagnostics; in particular CONNECT_ERROR can contain secrets.
	control := ""
	switch {
	case len(data) == 1 && data[0] == '2':
		d.report.Pings++
		d.lastPing = d.lastRX
		control = "engine_ping"
	case len(data) == 1 && data[0] == '1':
		control = "engine_close"
	case len(data) >= 2 && data[0] == '4' && data[1] == '1':
		control = "socket_disconnect"
	case len(data) >= 2 && data[0] == '4' && data[1] == '4':
		control = "socket_connect_error"
	}
	if control != "" {
		d.report.LastControl = control
		if control != "engine_ping" {
			d.report.TerminalControl = control
		}
	}
	// Do not parse chat payloads/history or retain arbitrary event names. A
	// warning/error immediately before an empty close is otherwise invisible.
	if len(data) <= 4096 && !bytes.Contains(data, []byte(`"messages"`)) {
		if event, ok := parseEditorEvent(data); ok {
			switch event.Type {
			case "error", "warning", "disconnectReason", "drop":
				d.report.EditorEvent = event.Type
				d.report.EditorCode = nil
				var code int
				if json.Unmarshal(event.Code, &code) == nil && string(event.Code) != "null" {
					d.report.EditorCode = &code
				}
			}
		}
	}
}

func (d *sessionTrace) wrote(data []byte, wait, elapsed time.Duration, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.report.MaxWriteWaitMS = max(d.report.MaxWriteWaitMS, wait.Milliseconds())
	d.report.MaxWriteMS = max(d.report.MaxWriteMS, elapsed.Milliseconds())
	if err != nil {
		d.report.FailedTXBytes = len(data)
		if d.report.Cause == "" {
			d.report.Cause = "write"
			d.report.Error, d.report.CloseCode = sessionError(err)
		}
		return
	}
	d.report.TXMessages++
	d.report.TXBytes += uint64(len(data))
	d.report.LastTXBytes = len(data)
	d.report.MaxTXBytes = max(d.report.MaxTXBytes, len(data))
	if len(data) == 1 && data[0] == '3' {
		d.report.Pongs++
		d.report.MaxPongMS = max(d.report.MaxPongMS, (wait + elapsed).Milliseconds())
	}
}

func ageMS(now, then time.Time) int64 {
	if then.IsZero() {
		return -1 // distinguish "never observed" from a recent event
	}
	return max(0, now.Sub(then).Milliseconds())
}

func (d *sessionTrace) snapshot(heartbeat time.Duration) sessionReport {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := time.Now()
	r := d.report
	r.AgeMS, r.ReadyMS = ageMS(now, d.started), ageMS(now, d.ready)
	r.LastRXAgoMS, r.LastPingAgoMS = ageMS(now, d.lastRX), ageMS(now, d.lastPing)
	r.HeartbeatMS = heartbeat.Milliseconds()
	return r
}

func (s *DocSession) readMessage() (int, []byte, error) {
	kind, data, err := s.Conn.ReadMessage()
	if err != nil {
		s.trace.fail("read", err)
	} else {
		s.trace.received(data)
	}
	return kind, data, err
}

func (s *DocSession) closeWithReason(reason string) {
	s.trace.fail(reason, nil)
	if s.Conn != nil {
		_ = s.Conn.Close()
	}
}

func (s *DocSession) reportClosed() {
	s.trace.logOnce.Do(func() {
		r := s.trace.snapshot(s.heartbeatTimeout)
		r.PingIntervalMS, r.PingTimeoutMS = s.pingIntervalMS, s.pingTimeoutMS
		data, _ := json.Marshal(r)
		utils.Statusf("[M-SESSION] %s", data)
	})
}

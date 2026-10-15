package mailru

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"universal-bypass-tool/utils"
)

type editorEvent struct {
	Type     string `json:"type"`
	Result   *int   `json:"result"`
	WaitAuth bool   `json:"waitAuth"`
}

var errEditorAuthRejected = errors.New("document authorization rejected")

func parseEditorEvent(data []byte) (editorEvent, bool) {
	var event editorEvent
	if len(data) < 2 || string(data[:2]) != "42" {
		return event, false
	}
	var parts []json.RawMessage
	var name string
	if json.Unmarshal(data[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[0], &name) != nil || name != "message" || json.Unmarshal(parts[1], &event) != nil {
		return event, false
	}
	return event, true
}

// Quoted names cannot occur inside a Base64 chat payload, so a data message
// never matches. Parsing every data message as JSON cost about as much as the
// chat decode itself and lowered downloads on the mini-PC by about a quarter.
var editorControlTypes = [][]byte{[]byte(`"authChanges"`), []byte(`"connectState"`)}

func isEditorControl(data []byte) bool {
	for _, name := range editorControlTypes {
		if bytes.Contains(data, name) {
			return true
		}
	}
	return false
}

func handleMailruControl(session *DocSession, data []byte) (bool, error) {
	var response string
	if string(data) == "2" {
		response = "3"
		if session.heartbeatTimeout > 0 {
			_ = session.Conn.SetReadDeadline(time.Now().Add(session.heartbeatTimeout))
		}
	} else if !isEditorControl(data) {
		return false, nil
	} else if event, ok := parseEditorEvent(data); ok {
		switch {
		case event.Type == "authChanges":
			response = `42["message",{"type":"authChangesAck"}]`
		case event.Type == "connectState" && event.WaitAuth:
			response = `42["message",{"type":"unLockDocument","unlock":true,"isSave":false,"releaseLocks":false}]`
		}
	}
	if response == "" {
		return false, nil
	}
	err := session.safeWrite(websocket.TextMessage, []byte(response))
	if err == nil && response == `42["message",{"type":"authChangesAck"}]` {
		session.chatReady.Store(true)
	}
	return true, err
}

// Socket.IO connect acknowledges the namespace, not access to the document.
func waitMailruEditorAuth(session *DocSession) error {
	deadline := time.Now().Add(mailruSocketIOHandshakeTimeout)
	for {
		// Ping handling must not extend the fixed authorization deadline.
		if err := session.Conn.SetReadDeadline(deadline); err != nil {
			return err
		}
		_, data, err := session.Conn.ReadMessage()
		if err != nil {
			return err
		}
		if strings.HasPrefix(string(data), "44") {
			return errEditorAuthRejected
		}
		if handled, err := handleMailruControl(session, data); handled {
			if err != nil {
				return err
			}
			continue
		}
		event, ok := parseEditorEvent(data)
		if !ok || event.Type != "auth" || event.Result == nil {
			continue
		}
		if *event.Result != 1 {
			return errEditorAuthRejected
		}
		var readDeadline time.Time
		if session.heartbeatTimeout > 0 {
			readDeadline = time.Now().Add(session.heartbeatTimeout)
		}
		return session.Conn.SetReadDeadline(readDeadline)
	}
}

// An old writer/reader must never disconnect a newer generation.
func (t *MailruDocsTransport) dropConnection(session *DocSession) {
	if session == nil {
		return
	}
	t.Mu.Lock()
	if t.session == session {
		t.SetConnected(false)
	}
	t.Mu.Unlock()
	if session.Conn != nil {
		_ = session.Conn.Close()
	}
}

func (t *MailruDocsTransport) Stop() error {
	t.Mu.Lock()
	err := t.BaseTransport.Stop()
	session := t.session
	opening := t.opening
	t.cancel()
	t.stopOnce.Do(func() { close(t.done) })
	t.Mu.Unlock()
	if session != nil && session.Conn != nil {
		_ = session.Conn.Close()
	}
	if opening != nil && opening.Conn != nil {
		_ = opening.Conn.Close()
	}
	return err
}

func (t *MailruDocsTransport) perfStatsLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	var previousWrites, previousBytes uint64
	for t.IsRunning() {
		select {
		case <-ticker.C:
		case <-t.done:
			return
		}
		writes, bytes := t.wsWrites.Load(), t.wsBytes.Load()
		utils.Statusf("[M-PERF] ready=%t ws_msg_s=%.1f ws_mbps=%.3f queue_bytes=%d queue_drops=%d write_errors=%d reconnects=%d", t.IsConnected(), float64(writes-previousWrites)/5, float64(bytes-previousBytes)*8/5e6, t.queuedBytes.Load(), t.queueDrops.Load(), t.writeErrors.Load(), t.Stats().Reconnects)
		previousWrites, previousBytes = writes, bytes
	}
}

// A public URL can acquire a new editor key while an old WebSocket remains
// healthy. Both sessions authorize, but their messages belong to different rooms.
// Refresh metadata without disturbing a connection whose key is unchanged.
func (t *MailruDocsTransport) documentKeyLoop(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.refreshDocumentKey()
		}
	}
}

func (t *MailruDocsTransport) refreshDocumentKey() {
	t.Mu.RLock()
	session := t.session
	ready := t.IsConnected() && t.IsRunning()
	t.Mu.RUnlock()
	if !ready || session == nil {
		return
	}
	info, err := t.fetchDocInfo(t.weblink)
	if err != nil {
		if errors.Is(err, errBrowserAuth) {
			t.dropConnection(session)
			return
		}
		if t.IsRunning() {
			t.statusThrottled("key-refresh", time.Minute, "[M-DOCS] cannot refresh document metadata; keeping current connection")
		}
		return
	}
	if info.DocKey == session.Info.DocKey {
		return
	}
	t.Mu.Lock()
	current := t.session == session && t.IsRunning() && t.IsConnected()
	if current {
		t.SetConnected(false)
	}
	t.Mu.Unlock()
	if current {
		utils.Statusf("[M-DOCS] document key changed; reconnecting to current editor session")
		_ = session.Conn.Close() // reader owns reconnect and fetches a fresh token
	}
}

package yandex

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

var errDocumentAuthRejected = errors.New("document authentication rejected")

// The first editor must acknowledge the transition to co-editing when another
// participant joins. Otherwise OnlyOffice holds the new participant in waitAuth
// and eventually disconnects the first editor when its document-lock timer fires.
// OpenFlux has no pending document edits or editor locks to flush.
func acknowledgeCoediting(session *DocSession, data []byte) (bool, error) {
	if !strings.HasPrefix(string(data), "42") {
		return false, nil
	}
	var parts []json.RawMessage
	if json.Unmarshal(data[2:], &parts) != nil || len(parts) != 2 {
		return false, nil
	}
	var name string
	var event struct {
		Type     string `json:"type"`
		WaitAuth bool   `json:"waitAuth"`
	}
	if json.Unmarshal(parts[0], &name) != nil || name != "message" || json.Unmarshal(parts[1], &event) != nil || event.Type != "connectState" {
		return false, nil
	}
	if !event.WaitAuth {
		return true, nil
	}
	return true, session.safeWrite(websocket.TextMessage, []byte(`42["message",{"type":"unLockDocument","unlock":true,"isSave":false,"releaseLocks":false}]`))
}

type docAuthEvent struct {
	Type   string `json:"type"`
	Result *int   `json:"result"`
}

// Other participants can send cursor and document events while a new session
// authenticates. Bound the wait by time, not by the number of unrelated frames.
func readDocAuthEvent(session *DocSession) (docAuthEvent, error) {
	for {
		_, message, err := session.Conn.ReadMessage()
		if err != nil {
			return docAuthEvent{}, err
		}
		text := string(message)
		if text == "2" {
			if err := session.safeWrite(websocket.TextMessage, []byte("3")); err != nil {
				return docAuthEvent{}, err
			}
			continue
		}
		if strings.HasPrefix(text, "44") {
			return docAuthEvent{}, errDocumentAuthRejected
		}
		if !strings.HasPrefix(text, "42") {
			continue
		}
		var parts []json.RawMessage
		if json.Unmarshal(message[2:], &parts) != nil || len(parts) != 2 {
			continue
		}
		var name string
		if json.Unmarshal(parts[0], &name) != nil || name != "message" {
			continue
		}
		var event docAuthEvent
		if json.Unmarshal(parts[1], &event) != nil {
			continue
		}
		return event, nil
	}
}

func authenticateDoc(session *DocSession, timeout time.Duration) error {
	// The caller takes ownership only after successful authentication. Failed
	// attempts must close before retrying, without closing a successful session.
	complete := false
	defer func() {
		if !complete {
			session.Conn.Close()
		}
	}()
	conn := session.Conn
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	_, hello, err := conn.ReadMessage()
	if err != nil {
		return fmt.Errorf("engine hello: %w", err)
	}
	if !strings.HasPrefix(string(hello), "0") {
		return errors.New("unexpected engine hello")
	}
	auth, err := json.Marshal(map[string]string{"token": session.Info.Token})
	if err != nil {
		return err
	}
	if err = session.safeWrite(websocket.TextMessage, append([]byte("40"), auth...)); err != nil {
		return err
	}
	for {
		event, err := readDocAuthEvent(session)
		if err != nil {
			return fmt.Errorf("license handshake: %w", err)
		}
		if event.Type == "license" {
			break
		}
	}
	info := session.Info
	authData := map[string]interface{}{
		"type": "auth", "docid": info.DocID, "token": "fghhfgsjdgfjs",
		"user": map[string]interface{}{"id": session.UserID}, "editorType": 0,
		"lastOtherSaveTime": -1, "permissions": info.Permissions,
		"openCmd": info.OpenCmd, "coEditingMode": "fast", "jwtOpen": info.Token,
	}
	message, err := json.Marshal([]interface{}{"message", authData})
	if err != nil {
		return err
	}
	// Opening/synchronizing the document gets its own complete deadline.
	if err = conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return err
	}
	if err = session.safeWrite(websocket.TextMessage, append([]byte("42"), message...)); err != nil {
		return err
	}
	for {
		event, err := readDocAuthEvent(session)
		if err != nil {
			return fmt.Errorf("document handshake: %w", err)
		}
		switch event.Type {
		case "authChanges":
			if err := session.safeWrite(websocket.TextMessage, []byte(`42["message",{"type":"authChangesAck"}]`)); err != nil {
				return err
			}
		case "auth":
			if event.Result == nil {
				continue
			}
			if *event.Result != 1 {
				return errDocumentAuthRejected
			}
			if err := conn.SetReadDeadline(time.Time{}); err != nil {
				return err
			}
			complete = true
			return nil
		}
	}
}

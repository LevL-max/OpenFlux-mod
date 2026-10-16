package mailru

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// SetCursorProbe selects the historical wire carrier for a bounded comparison.
// Authentication, cookie reload, heartbeat and document-key refresh stay current.
// This does not enable synthetic document edits or change the default carrier.
func (t *MailruDocsTransport) SetCursorProbe(enabled bool) error {
	if t.IsRunning() {
		return fmt.Errorf("configure cursor control before start")
	}
	t.cursorProbe = enabled
	return nil
}

func encodeProbeCursor(packet []byte) []byte {
	return []byte(fmt.Sprintf(`42["message",{"type":"cursor","cursor":"%d;%s"}]`, len(packet), base64.StdEncoding.EncodeToString(packet)))
}

func decodeProbeCursors(frame []byte) [][]byte {
	if !strings.HasPrefix(string(frame), "42") {
		return nil
	}
	var parts []json.RawMessage
	var name string
	var event struct {
		Type     string `json:"type"`
		Messages []struct {
			Cursor string `json:"cursor"`
		} `json:"messages"`
	}
	if json.Unmarshal(frame[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[0], &name) != nil || name != "message" || json.Unmarshal(parts[1], &event) != nil || event.Type != "cursor" {
		return nil
	}
	var packets [][]byte
	for _, m := range event.Messages {
		length, payload, ok := strings.Cut(m.Cursor, ";")
		n, err := strconv.Atoi(length)
		if !ok || err != nil || n < 1 || n > maxQueuedBytes {
			continue
		}
		packet, err := base64.StdEncoding.DecodeString(payload)
		if err == nil && len(packet) == n {
			packets = append(packets, packet)
		}
	}
	return packets
}

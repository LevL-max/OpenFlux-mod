package mailru

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const chatDataTag = "OFX1"
const chatReadyFallback = 3 * time.Second

func encodeChatData(packet []byte) []byte {
	// Base64 and the tag need no JSON escaping.
	return []byte(`42["message",{"type":"message","message":"` + chatDataTag + base64.StdEncoding.EncodeToString(packet) + `"}]`)
}

// Decode only live chat broadcasts, never auth history, cursors or human chat.
// Mail.ru echoes writes to their sender; discard those before delivery to TCP.
func decodeChatData(frame []byte, ownIDs ...string) [][]byte {
	if len(frame) < 2 || string(frame[:2]) != "42" {
		return nil
	}
	var parts []json.RawMessage
	var envelope string
	if json.Unmarshal(frame[2:], &parts) != nil || len(parts) != 2 || json.Unmarshal(parts[0], &envelope) != nil || envelope != "message" {
		return nil
	}
	var event struct {
		Type     string `json:"type"`
		Messages []struct {
			Message        string `json:"message"`
			UserIDOriginal string `json:"useridoriginal"`
		} `json:"messages"`
	}
	if json.Unmarshal(parts[1], &event) != nil || event.Type != "message" {
		return nil
	}
	var packets [][]byte
	for _, msg := range event.Messages {
		own := false
		for _, id := range ownIDs {
			if id != "" && msg.UserIDOriginal == id {
				own = true
				break
			}
		}
		if own || !strings.HasPrefix(msg.Message, chatDataTag) {
			continue
		}
		packet, err := base64.StdEncoding.DecodeString(msg.Message[len(chatDataTag):])
		if err == nil && len(packet) > 0 {
			packets = append(packets, packet)
		}
	}
	return packets
}

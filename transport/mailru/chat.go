package mailru

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

const chatDataTag = "OFX1"
const chatReadyFallback = 3 * time.Second

func encodeChatData(packet []byte, sender ...string) []byte {
	return []byte(`42["message",{"type":"message","message":"` + chatDataText(packet, sender...) + `"}]`)
}

func chatDataText(packet []byte, sender ...string) string {
	// Base64 and the tag need no JSON escaping.
	tag := chatDataTag
	if len(sender) > 0 && sender[0] != "" {
		tag = "OFX2" + sender[0] + ":"
	}
	return tag + base64.StdEncoding.EncodeToString(packet)
}

// Decode only live chat broadcasts, never auth history, cursors or human chat.
// Mail.ru echoes writes to their sender; discard those before delivery to TCP.
func decodeChatData(frame []byte, ownIDs ...string) [][]byte {
	return decodeSessionChatData(frame, "", ownIDs...)
}

// Authenticated sessions can share an account ID. A per-process marker filters
// echoes without discarding the other endpoint's traffic from the same account.
func decodeSessionChatData(frame []byte, sender string, ownIDs ...string) [][]byte {
	return decodeObservedChatData(frame, sender, nil, ownIDs...)
}

func decodeObservedChatData(frame []byte, sender string, ownEcho func(string), ownIDs ...string) [][]byte {
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
		if strings.HasPrefix(msg.Message, "OFX2") {
			id, payload, ok := strings.Cut(msg.Message[4:], ":")
			if !ok || len(id) != 26 || strings.Trim(id, "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567") != "" {
				continue
			}
			if id == sender {
				if ownEcho != nil {
					ownEcho(msg.Message)
				}
				continue
			}
			packet, err := base64.StdEncoding.DecodeString(payload)
			if err == nil && len(packet) > 0 {
				packets = append(packets, packet)
			}
			continue
		}
		own := false
		for _, id := range ownIDs {
			if id != "" && msg.UserIDOriginal == id {
				own = true
				break
			}
		}
		if own || !strings.HasPrefix(msg.Message, chatDataTag) {
			if own && ownEcho != nil {
				ownEcho(msg.Message)
			}
			continue
		}
		packet, err := base64.StdEncoding.DecodeString(msg.Message[len(chatDataTag):])
		if err == nil && len(packet) > 0 {
			packets = append(packets, packet)
		}
	}
	return packets
}

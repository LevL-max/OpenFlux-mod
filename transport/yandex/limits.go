package yandex

import (
	"encoding/base64"
	"strconv"
	"time"
)

// SetCursorSafety configures a conservative editor budget before Start.
// Every application write, including heartbeats, shares the session's pacer.
// Zero values retain the historical unbounded mode for controlled comparisons.
func (t *YandexDocsTransport) SetCursorSafety(maxBytes int, interval time.Duration) {
	t.cursorMaxBytes = maxBytes
	t.writeEvery = interval
}

func cursorWireSize(binaryBytes int) int {
	return len(`42["message",{"type":"cursor","cursor":";"}]`) + len(strconv.Itoa(binaryBytes)) + base64.StdEncoding.EncodedLen(binaryBytes)
}

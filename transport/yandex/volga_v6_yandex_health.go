package yandex

import "time"

func (c *volgaV6YandexCarrier) VolgaV6PhysicalHealth(now time.Time) volgaV6PhysicalHealth {
	snap := c.Snapshot()
	return volgaV6PhysicalHealth{
		Known:         true,
		Generation:    snap.Generation,
		Document:      c.docURL,
		Connected:     snap.Connected,
		Posts:         snap.Posts,
		PostFailures:  snap.PostFailures,
		PostBytes:     snap.PostBytes,
		PostMicros:    snap.PostMicros,
		MaxPostMicros: snap.MaxPostMicros,
		WSReconnects:  snap.WSReconnects,
	}
}

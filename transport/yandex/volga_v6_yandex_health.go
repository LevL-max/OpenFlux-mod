//go:build volga

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
		HTTP1Posts:    snap.HTTP1Posts, HTTP2Posts: snap.HTTP2Posts,
		WSDataFrames: snap.WSDataFrames, WSAckFrames: snap.WSAckFrames, WSDecodeErrors: snap.WSDecodeErrors,
		WSRawMessages: snap.WSRawMessages, WSIgnoredMessages: snap.WSIgnoredMessages, WSJSONErrors: snap.WSJSONErrors,
		HTTPStatuses: snap.HTTPStatuses, HTTPTransportErrors: snap.HTTPTransportErrors,
		RelayPostsPerSecond: snap.RelayPostsPerSecond, RelayRetryAt: snap.RelayRetryAt,
	}
}

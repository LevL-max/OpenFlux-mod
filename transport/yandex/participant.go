package yandex

// NewParticipant creates an independent connection with the same document and
// bootstrap configuration. Call before Start. The cookie store object is shared
// so its write mutex also serializes the temporary-file/rename transaction.
func (t *YandexDocsTransport) NewParticipant() *YandexDocsTransport {
	peer := NewYandexDocsTransport(t.url, t.GetConfig())
	peer.browserCookie, peer.browserUserAgent = t.currentBrowserState()
	peer.cookieStore = t.cookieStore
	t.cookieMu.RLock()
	peer.cookieStoreSum = t.cookieStoreSum
	t.cookieMu.RUnlock()
	peer.SetCursorSafety(t.cursorMaxBytes, t.writeEvery)
	return peer
}

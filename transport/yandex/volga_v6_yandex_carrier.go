//go:build volga

package yandex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

var errVolgaV6BodyTooLarge = errors.New("volga v6 relay body too large")

type volgaV6YandexConfig struct {
	RelayTimeout        time.Duration
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	IdleConnTimeout     time.Duration
	ForceHTTP2          bool
	HTTPProtocol        string // empty/auto preserves Go negotiation; http1/http2 selects explicitly
	HTTPBodyLimit       int
	RelayPostsPerSecond float64 // optional initial ceiling; 0 preserves initial behavior
	RelayEnvelope       string  // diagnostic opt-in: "minimal" omits editor operations
	relayGate           *volgaV6RelayGate
	authorize           func(context.Context, string) (*volgaAuth, error)

	WSHandshakeTimeout  time.Duration
	WSReadTimeout       time.Duration
	ReconnectMinDelay   time.Duration
	ReconnectMaxDelay   time.Duration
	ReconnectMultiplier float64
}

func defaultVolgaV6YandexConfig() volgaV6YandexConfig {
	return volgaV6YandexConfig{
		RelayTimeout:        30 * time.Second,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 256,
		IdleConnTimeout:     90 * time.Second,
		ForceHTTP2:          false,
		HTTPBodyLimit:       8000,
		WSHandshakeTimeout:  10 * time.Second,
		WSReadTimeout:       60 * time.Second,
		ReconnectMinDelay:   500 * time.Millisecond,
		ReconnectMaxDelay:   30 * time.Second,
		ReconnectMultiplier: 1.5,
	}
}

type volgaV6YandexCarrierSnapshot struct {
	Generation          uint64
	Connected           bool
	Posts               uint64
	PostFailures        uint64
	PostBytes           uint64
	PostMicros          uint64
	MaxPostMicros       uint64
	WSReconnects        uint64
	HTTP1Posts          uint64
	HTTP2Posts          uint64
	WSDataFrames        uint64
	WSAckFrames         uint64
	WSDecodeErrors      uint64
	WSRawMessages       uint64
	WSIgnoredMessages   uint64
	WSJSONErrors        uint64
	HTTPStatuses        map[int]uint64
	HTTPTransportErrors uint64
	RelayPostsPerSecond float64
	RelayRetryAt        time.Time
}

// volgaV6YandexCarrier is a disposable physical generation. It intentionally
// does not own logical sequence, replay, ACK or recovery state.
type volgaV6YandexCarrier struct {
	generation uint64
	docURL     string
	config     volgaV6YandexConfig
	onFrame    func(volgaV6WireFrame)

	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.RWMutex
	auth     *volgaAuth
	http     *http.Client
	ws       *volgaV6YandexWS
	frontier string

	wireSeq  atomic.Uint64
	bundleID atomic.Uint64
	localID  atomic.Uint64

	posts               atomic.Uint64
	postFailures        atomic.Uint64
	postBytes           atomic.Uint64
	postMicros          atomic.Uint64
	maxPostMicros       atomic.Uint64
	wsReconnects        atomic.Uint64
	http1Posts          atomic.Uint64
	http2Posts          atomic.Uint64
	wsDataFrames        atomic.Uint64
	wsAckFrames         atomic.Uint64
	wsDecodeErrors      atomic.Uint64
	wsRawMessages       atomic.Uint64
	wsIgnoredMessages   atomic.Uint64
	wsJSONErrors        atomic.Uint64
	httpStatuses        [600]atomic.Uint64
	httpTransportErrors atomic.Uint64

	started  atomic.Bool
	stopOnce sync.Once
}

func newVolgaV6YandexCarrier(generation uint64, docURL string, cfg volgaV6YandexConfig, onFrame func(volgaV6WireFrame)) *volgaV6YandexCarrier {
	defaults := defaultVolgaV6YandexConfig()
	if cfg.RelayTimeout <= 0 {
		cfg.RelayTimeout = defaults.RelayTimeout
	}
	if cfg.MaxIdleConns <= 0 {
		cfg.MaxIdleConns = defaults.MaxIdleConns
	}
	if cfg.MaxIdleConnsPerHost <= 0 {
		cfg.MaxIdleConnsPerHost = defaults.MaxIdleConnsPerHost
	}
	if cfg.IdleConnTimeout <= 0 {
		cfg.IdleConnTimeout = defaults.IdleConnTimeout
	}
	if cfg.HTTPBodyLimit <= 0 {
		cfg.HTTPBodyLimit = defaults.HTTPBodyLimit
	}
	if cfg.WSHandshakeTimeout <= 0 {
		cfg.WSHandshakeTimeout = defaults.WSHandshakeTimeout
	}
	if cfg.WSReadTimeout <= 0 {
		cfg.WSReadTimeout = defaults.WSReadTimeout
	}
	if cfg.ReconnectMinDelay <= 0 {
		cfg.ReconnectMinDelay = defaults.ReconnectMinDelay
	}
	if cfg.ReconnectMaxDelay <= 0 {
		cfg.ReconnectMaxDelay = defaults.ReconnectMaxDelay
	}
	if cfg.ReconnectMultiplier <= 1 {
		cfg.ReconnectMultiplier = defaults.ReconnectMultiplier
	}
	ctx, cancel := context.WithCancel(context.Background())
	if cfg.relayGate == nil {
		cfg.relayGate = &volgaV6RelayGate{rate: cfg.RelayPostsPerSecond}
	}
	return &volgaV6YandexCarrier{
		generation: generation,
		docURL:     docURL,
		config:     cfg,
		onFrame:    onFrame,
		ctx:        ctx,
		cancel:     cancel,
	}
}

func newVolgaV6YandexCarrierFactory(docURLs []string, cfg volgaV6YandexConfig) volgaV6CarrierFactory {
	if cfg.relayGate == nil {
		cfg.relayGate = &volgaV6RelayGate{rate: cfg.RelayPostsPerSecond}
	}
	urls := append([]string(nil), docURLs...)
	return func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		if len(urls) == 0 {
			return nil, fmt.Errorf("volga v6 requires at least one Yandex document URL")
		}
		for _, docURL := range urls {
			if strings.TrimSpace(docURL) == "" {
				return nil, fmt.Errorf("volga v6 Yandex document URL is empty")
			}
		}
		idx := int((generation - 1) % uint64(len(urls)))
		return newVolgaV6YandexCarrier(generation, urls[idx], cfg, onFrame), nil
	}
}

func (c *volgaV6YandexCarrier) Generation() uint64 { return c.generation }

func volgaV6HTTPTransport(cfg volgaV6YandexConfig) *http.Transport {
	tr := &http.Transport{
		MaxIdleConns: cfg.MaxIdleConns, MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
		IdleConnTimeout: cfg.IdleConnTimeout, DisableCompression: true,
		ForceAttemptHTTP2: cfg.ForceHTTP2,
	}
	if cfg.HTTPProtocol == "http1" || cfg.HTTPProtocol == "http2" {
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetHTTP1(true)
		tr.Protocols.SetHTTP2(cfg.HTTPProtocol == "http2")
	}
	return tr
}

func (c *volgaV6YandexCarrier) Start(ctx context.Context) error {
	if c.started.Load() {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	authorize := c.config.authorize
	if authorize == nil {
		authorize = authorizeContext
	}
	auth, err := authorize(ctx, c.docURL)
	if err != nil {
		return fmt.Errorf("volga v6 generation %d auth: %w", c.generation, err)
	}
	transport := volgaV6HTTPTransport(c.config)
	httpClient := &http.Client{
		Transport: transport,
		Timeout:   c.config.RelayTimeout,
		Jar:       auth.Session.Jar,
	}

	c.mu.Lock()
	c.auth = auth
	c.http = httpClient
	c.mu.Unlock()

	ws := newVolgaV6YandexWS(c, auth)
	c.mu.Lock()
	c.ws = ws
	c.mu.Unlock()
	ws.Start()

	if err := ws.WaitReady(ctx); err != nil {
		_ = c.Stop()
		return fmt.Errorf("volga v6 generation %d websocket readiness: %w", c.generation, err)
	}
	c.started.Store(true)
	return nil
}

func (c *volgaV6YandexCarrier) Stop() error {
	c.stopOnce.Do(func() {
		c.cancel()
		c.mu.Lock()
		ws := c.ws
		httpClient := c.http
		c.ws = nil
		c.http = nil
		c.mu.Unlock()
		if ws != nil {
			ws.Stop()
		}
		if httpClient != nil {
			httpClient.CloseIdleConnections()
		}
		c.started.Store(false)
	})
	return nil
}

func (c *volgaV6YandexCarrier) setFrontier(opID string) {
	if opID == "" {
		return
	}
	c.mu.Lock()
	c.frontier = opID
	c.mu.Unlock()
}

func (c *volgaV6YandexCarrier) frontierSnapshot() []interface{} {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.frontier == "" {
		return []interface{}{}
	}
	return []interface{}{c.frontier}
}

func (c *volgaV6YandexCarrier) authSnapshot() (*volgaAuth, *http.Client) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.auth, c.http
}

func encodeVolgaV6BatchRecords(records [][]byte) ([]byte, error) {
	blob, err := volgaV6RecordBlob(records)
	if err != nil {
		return nil, err
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(len(blob)))
	base64.StdEncoding.Encode(encoded, blob)
	return encoded, nil
}

func (c *volgaV6YandexCarrier) buildRelayBody(records [][]byte) ([]byte, error) {
	auth, _ := c.authSnapshot()
	if auth == nil {
		return nil, fmt.Errorf("volga v6 generation %d not authorized", c.generation)
	}
	encoded, err := encodeVolgaV6BatchRecords(records)
	if err != nil {
		return nil, err
	}

	wireBase := c.wireSeq.Add(2)
	opID := fmt.Sprintf("1-%d.%d", auth.UserID, wireBase-1)
	relayOpID := fmt.Sprintf("1-%d.%d", auth.UserID, wireBase)
	bundleID := c.bundleID.Add(1)

	bundle := []interface{}{
		map[string]interface{}{
			"id":         opID,
			"frontier":   c.frontierSnapshot(),
			"undoable":   true,
			"actionName": "textInsert",
			"ops":        []interface{}{[]interface{}{"it", "vyd:t/00000000000008", 0, "A"}},
			"sideEffect": false,
			"localId":    c.localID.Add(1),
		},
		map[string]interface{}{
			"id":         relayOpID,
			"frontier":   []interface{}{opID},
			"undoable":   false,
			"actionName": "setCaret",
			"ops": []interface{}{
				[]interface{}{"us", auth.UserID, []interface{}{
					[]interface{}{
						[]interface{}{"vyd:t/00000000000008", 0, -1},
						[]interface{}{"vyd:t/00000000000008", 0, -1},
					},
				}},
			},
			"sideEffect": true,
			"localId":    c.localID.Add(1),
		},
		string(encoded),
	}
	if c.config.RelayEnvelope == "minimal" {
		bundle = []interface{}{string(encoded)}
	}
	payload := map[string]interface{}{
		"message": map[string]interface{}{
			"bundleId": bundleID,
			"bundle":   bundle,
		},
		"targetUserId": nil,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if c.config.HTTPBodyLimit > 0 && len(body) > c.config.HTTPBodyLimit {
		return nil, fmt.Errorf("%w: generation=%d bytes=%d limit=%d", errVolgaV6BodyTooLarge, c.generation, len(body), c.config.HTTPBodyLimit)
	}
	return body, nil
}

func (c *volgaV6YandexCarrier) SendVolgaV6(frame volgaV6WireFrame) error {
	if !c.started.Load() {
		return fmt.Errorf("volga v6 generation %d not started", c.generation)
	}
	records, ok := encodeVolgaV6Records(frame)
	if !ok {
		return fmt.Errorf("volga v6 invalid logical frame kind=%d session=%d seq=%d", frame.Kind, frame.Session, frame.Seq)
	}
	body, err := c.buildRelayBody(records)
	if errors.Is(err, errVolgaV6BodyTooLarge) && frame.Kind == volgaV6FrameData {
		// Keep one logical sequence across all physical pieces. In particular,
		// do not remove replay or allocate a fresh sequence after a size error.
		blob, blobErr := volgaV6RecordBlob(records)
		if blobErr != nil {
			return blobErr
		}
		var bodies [][]byte
		for offset := 0; offset < len(blob); offset += volgaV6FragmentBytes {
			fragment := volgaV6WireFrame{Kind: volgaV6FrameFragment, Session: frame.Session, Seq: frame.Seq,
				Fragment: volgaV6Fragment{Total: uint32(len(blob)), Index: uint16(offset / volgaV6FragmentBytes),
					Data: blob[offset:min(offset+volgaV6FragmentBytes, len(blob))]}}
			parts, _ := encodeVolgaV6Records(fragment)
			partBody, buildErr := c.buildRelayBody(parts)
			if buildErr != nil {
				c.postFailures.Add(1)
				return buildErr
			}
			bodies = append(bodies, partBody)
		}
		// Preflight every body before any POST, including JSON/ID overhead.
		for _, partBody := range bodies {
			if err := c.postRelayBody(partBody); err != nil {
				return err
			}
		}
		return nil
	}
	if err != nil {
		c.postFailures.Add(1)
		return err
	}
	return c.postRelayBody(body)
}

func (c *volgaV6YandexCarrier) postRelayBody(body []byte) error {
	if err := c.config.relayGate.wait(c.ctx); err != nil {
		return err
	}
	auth, httpClient := c.authSnapshot()
	if auth == nil || httpClient == nil {
		c.postFailures.Add(1)
		return fmt.Errorf("volga v6 generation %d transport unavailable", c.generation)
	}

	urlStr := fmt.Sprintf("https://volga.yandex.ru/session/main/%s/relay", auth.RequestPath)
	req, err := http.NewRequestWithContext(c.ctx, http.MethodPost, urlStr, bytes.NewReader(body))
	if err != nil {
		c.postFailures.Add(1)
		return err
	}
	req.Header.Set("User-Agent", volgaUserAgent)
	req.Header.Set("Authorization", "Bearer "+auth.Token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://volga.yandex.ru")
	req.Header.Set("Referer", "https://volga.yandex.ru/document/?request-path="+auth.RequestPath)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.ContentLength = int64(len(body))

	cookieParts := make([]string, 0, len(auth.Cookies))
	for _, cookie := range auth.Cookies {
		cookieParts = append(cookieParts, cookie.Name+"="+cookie.Value)
	}
	if len(cookieParts) > 0 {
		req.Header.Set("Cookie", strings.Join(cookieParts, "; "))
	}

	started := time.Now()
	resp, err := httpClient.Do(req)
	elapsed := time.Since(started)
	micros := uint64(elapsed.Microseconds())
	if micros == 0 {
		micros = 1
	}
	c.posts.Add(1)
	c.postMicros.Add(micros)
	atomicMax(&c.maxPostMicros, micros)
	if err != nil {
		c.httpTransportErrors.Add(1)
		c.postFailures.Add(1)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 0 && resp.StatusCode < len(c.httpStatuses) {
		c.httpStatuses[resp.StatusCode].Add(1)
	}
	if resp.ProtoMajor == 1 {
		c.http1Posts.Add(1)
	}
	if resp.ProtoMajor == 2 {
		c.http2Posts.Add(1)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		c.config.relayGate.limited(resp.Header.Get("Retry-After"), time.Now())
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
		c.postFailures.Add(1)
		return fmt.Errorf("volga v6 relay status %d", resp.StatusCode)
	}
	c.postBytes.Add(uint64(len(body)))
	return nil
}

func (c *volgaV6YandexCarrier) Snapshot() volgaV6YandexCarrierSnapshot {
	c.mu.RLock()
	ws := c.ws
	c.mu.RUnlock()
	connected := ws != nil && ws.IsConnected()
	rate, retryAt := c.config.relayGate.snapshot()
	statuses := make(map[int]uint64)
	for code := range c.httpStatuses {
		if n := c.httpStatuses[code].Load(); n > 0 {
			statuses[code] = n
		}
	}
	return volgaV6YandexCarrierSnapshot{
		Generation:    c.generation,
		Connected:     connected,
		Posts:         c.posts.Load(),
		PostFailures:  c.postFailures.Load(),
		PostBytes:     c.postBytes.Load(),
		PostMicros:    c.postMicros.Load(),
		MaxPostMicros: c.maxPostMicros.Load(),
		WSReconnects:  c.wsReconnects.Load(),
		HTTP1Posts:    c.http1Posts.Load(), HTTP2Posts: c.http2Posts.Load(),
		WSDataFrames: c.wsDataFrames.Load(), WSAckFrames: c.wsAckFrames.Load(), WSDecodeErrors: c.wsDecodeErrors.Load(),
		WSRawMessages: c.wsRawMessages.Load(), WSIgnoredMessages: c.wsIgnoredMessages.Load(), WSJSONErrors: c.wsJSONErrors.Load(),
		HTTPStatuses: statuses, HTTPTransportErrors: c.httpTransportErrors.Load(),
		RelayPostsPerSecond: rate, RelayRetryAt: retryAt,
	}
}

type volgaV6YandexWS struct {
	carrier *volgaV6YandexCarrier
	auth    *volgaAuth

	ctx    context.Context
	cancel context.CancelFunc

	connected atomic.Bool
	readyOnce sync.Once
	ready     chan struct{}

	connMu sync.Mutex
	conn   *websocket.Conn
}

func newVolgaV6YandexWS(carrier *volgaV6YandexCarrier, auth *volgaAuth) *volgaV6YandexWS {
	ctx, cancel := context.WithCancel(carrier.ctx)
	return &volgaV6YandexWS{
		carrier: carrier,
		auth:    auth,
		ctx:     ctx,
		cancel:  cancel,
		ready:   make(chan struct{}),
	}
}

func (w *volgaV6YandexWS) Start() { go w.run() }

func (w *volgaV6YandexWS) Stop() {
	w.cancel()
	w.connMu.Lock()
	conn := w.conn
	w.conn = nil
	w.connMu.Unlock()
	if conn != nil {
		_ = conn.Close()
	}
}

func (w *volgaV6YandexWS) IsConnected() bool { return w.connected.Load() }

func (w *volgaV6YandexWS) WaitReady(ctx context.Context) error {
	select {
	case <-w.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-w.ctx.Done():
		return context.Canceled
	}
}

func (w *volgaV6YandexWS) run() {
	delay := w.carrier.config.ReconnectMinDelay
	for {
		select {
		case <-w.ctx.Done():
			return
		default:
		}

		connected, err := w.connect()
		if w.ctx.Err() != nil {
			return
		}
		if connected {
			// A successfully established session gets a fresh backoff baseline.
			delay = w.carrier.config.ReconnectMinDelay
		}
		if err != nil {
			w.carrier.wsReconnects.Add(1)
		}
		select {
		case <-time.After(delay):
		case <-w.ctx.Done():
			return
		}
		delay = time.Duration(float64(delay) * w.carrier.config.ReconnectMultiplier)
		if delay > w.carrier.config.ReconnectMaxDelay {
			delay = w.carrier.config.ReconnectMaxDelay
		}
	}
}

func (w *volgaV6YandexWS) connect() (bool, error) {
	wsURL := "wss://push.yandex.ru/v2/subscribe/websocket?" +
		"service=volga" +
		"&user=" + url.QueryEscape(w.auth.UserIDStr) +
		"&sign=" + w.auth.Sign +
		"&ts=" + w.auth.TS +
		"&client=web" +
		"&session=" + w.auth.SessionID +
		"&fetch_history=" + url.QueryEscape(w.auth.UserIDStr+":volga:0:1") +
		"&x_request_attempt=0"

	header := http.Header{}
	header.Set("User-Agent", volgaUserAgent)
	header.Set("Origin", "https://volga.yandex.ru")
	cookieParts := make([]string, 0, len(w.auth.Cookies))
	for _, cookie := range w.auth.Cookies {
		cookieParts = append(cookieParts, cookie.Name+"="+cookie.Value)
	}
	header.Set("Cookie", strings.Join(cookieParts, "; "))

	dialer := websocket.Dialer{
		HandshakeTimeout: w.carrier.config.WSHandshakeTimeout,
		ReadBufferSize:   4 << 20,
		WriteBufferSize:  4 << 20,
	}
	conn, _, err := dialer.DialContext(w.ctx, wsURL, header)
	if err != nil {
		return false, fmt.Errorf("volga v6 websocket dial: %w", err)
	}
	w.connMu.Lock()
	if w.ctx.Err() != nil {
		w.connMu.Unlock()
		_ = conn.Close()
		return false, w.ctx.Err()
	}
	w.conn = conn
	w.connMu.Unlock()
	w.connected.Store(true)
	w.readyOnce.Do(func() { close(w.ready) })
	defer func() {
		w.connected.Store(false)
		w.connMu.Lock()
		if w.conn == conn {
			w.conn = nil
		}
		w.connMu.Unlock()
		_ = conn.Close()
	}()

	for {
		select {
		case <-w.ctx.Done():
			return true, nil
		default:
		}
		_ = conn.SetReadDeadline(time.Now().Add(w.carrier.config.WSReadTimeout))
		_, message, err := conn.ReadMessage()
		if err != nil {
			return true, fmt.Errorf("volga v6 websocket read: %w", err)
		}
		w.handleMessage(message)
	}
}

func (w *volgaV6YandexWS) handleMessage(raw []byte) {
	w.carrier.wsRawMessages.Add(1)
	var envelope struct {
		Operation string `json:"operation"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		w.carrier.wsJSONErrors.Add(1)
		return
	}
	if envelope.Message == "" {
		w.carrier.wsIgnoredMessages.Add(1)
		return
	}
	if envelope.Operation == "ping" || (envelope.Operation != "SESSION" && envelope.Operation != "WORKER") {
		w.carrier.wsIgnoredMessages.Add(1)
		return
	}

	var inner struct {
		T       string          `json:"t"`
		UserID  int             `json:"userId"`
		Bundle  json.RawMessage `json:"bundle"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(envelope.Message), &inner); err != nil {
		w.carrier.wsJSONErrors.Add(1)
		return
	}
	deliverData := inner.UserID != w.auth.UserID
	switch inner.T {
	case "relay":
		w.handleRelay(inner.Message, deliverData)
	case "exchange":
		w.handleExchange(inner.Bundle, deliverData)
	default:
		w.carrier.wsIgnoredMessages.Add(1)
	}
}

func (w *volgaV6YandexWS) handleRelay(raw json.RawMessage, deliverData bool) {
	var relay struct {
		Bundle []json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &relay); err != nil {
		w.carrier.wsJSONErrors.Add(1)
		return
	}
	for _, item := range relay.Bundle {
		w.handleBundleItem(item, deliverData)
	}
}

func (w *volgaV6YandexWS) handleExchange(raw json.RawMessage, deliverData bool) {
	var asArray []json.RawMessage
	if err := json.Unmarshal(raw, &asArray); err == nil {
		for _, item := range asArray {
			w.handleBundleItem(item, deliverData)
		}
		return
	}
	var asObject struct {
		Value []json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(raw, &asObject); err == nil {
		for _, item := range asObject.Value {
			w.handleBundleItem(item, deliverData)
		}
	} else {
		w.carrier.wsJSONErrors.Add(1)
	}
}

func (w *volgaV6YandexWS) handleBundleItem(raw json.RawMessage, deliverData bool) {
	var action struct {
		ID     string `json:"id"`
		Action string `json:"actionName"`
	}
	if err := json.Unmarshal(raw, &action); err == nil && action.Action != "" {
		w.carrier.setFrontier(action.ID)
		return
	}
	if !deliverData {
		return
	}

	var encoded string
	if err := json.Unmarshal(raw, &encoded); err != nil || encoded == "" {
		return
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		w.carrier.wsDecodeErrors.Add(1)
		return
	}
	records, valid := decodeVolgaV6RecordBlob(decoded)
	if !valid {
		w.carrier.wsDecodeErrors.Add(1)
		return
	}
	frame, ok := decodeVolgaV6Records(records)
	if !ok {
		w.carrier.wsDecodeErrors.Add(1)
		return
	}
	if frame.Kind == volgaV6FrameData || frame.Kind == volgaV6FrameFragment {
		w.carrier.wsDataFrames.Add(1)
	}
	if frame.Kind == volgaV6FrameAck {
		w.carrier.wsAckFrames.Add(1)
	}
	if w.carrier.onFrame != nil {
		w.carrier.onFrame(frame)
	}
}

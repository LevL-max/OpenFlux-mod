package yandex

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
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
	HTTPBodyLimit       int

	WSHandshakeTimeout time.Duration
	WSReadTimeout      time.Duration
	ReconnectMinDelay  time.Duration
	ReconnectMaxDelay  time.Duration
	ReconnectMultiplier float64
}

func defaultVolgaV6YandexConfig() volgaV6YandexConfig {
	return volgaV6YandexConfig{
		RelayTimeout:          30 * time.Second,
		MaxIdleConns:          256,
		MaxIdleConnsPerHost:   256,
		IdleConnTimeout:       90 * time.Second,
		ForceHTTP2:            false,
		HTTPBodyLimit:         8000,
		WSHandshakeTimeout:    10 * time.Second,
		WSReadTimeout:         60 * time.Second,
		ReconnectMinDelay:     500 * time.Millisecond,
		ReconnectMaxDelay:     30 * time.Second,
		ReconnectMultiplier:   1.5,
	}
}

type volgaV6YandexCarrierSnapshot struct {
	Generation      uint64
	Connected       bool
	Posts           uint64
	PostFailures    uint64
	PostBytes       uint64
	PostMicros      uint64
	MaxPostMicros   uint64
	WSReconnects    uint64
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

	posts         atomic.Uint64
	postFailures  atomic.Uint64
	postBytes     atomic.Uint64
	postMicros    atomic.Uint64
	maxPostMicros atomic.Uint64
	wsReconnects  atomic.Uint64

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

func (c *volgaV6YandexCarrier) Start(ctx context.Context) error {
	if c.started.Load() {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	auth, err := authorize(c.docURL)
	if err != nil {
		return fmt.Errorf("volga v6 generation %d auth: %w", c.generation, err)
	}
	transport := &http.Transport{
		MaxIdleConns:        c.config.MaxIdleConns,
		MaxIdleConnsPerHost: c.config.MaxIdleConnsPerHost,
		IdleConnTimeout:     c.config.IdleConnTimeout,
		DisableCompression:  true,
		ForceAttemptHTTP2:   c.config.ForceHTTP2,
	}
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
			if tr, ok := httpClient.Transport.(*http.Transport); ok {
				tr.CloseIdleConnections()
			}
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
	var blob bytes.Buffer
	var length [2]byte
	for _, record := range records {
		if len(record) == 0 || len(record) > 0xffff {
			return nil, fmt.Errorf("volga v6 record length %d outside uint16 framing", len(record))
		}
		binary.BigEndian.PutUint16(length[:], uint16(len(record)))
		blob.Write(length[:])
		blob.Write(record)
	}
	if blob.Len() == 0 {
		return nil, fmt.Errorf("volga v6 empty record batch")
	}
	encoded := make([]byte, base64.StdEncoding.EncodedLen(blob.Len()))
	base64.StdEncoding.Encode(encoded, blob.Bytes())
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
	if err != nil {
		c.postFailures.Add(1)
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
		c.postFailures.Add(1)
		return err
	}
	defer resp.Body.Close()
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
	return volgaV6YandexCarrierSnapshot{
		Generation:    c.generation,
		Connected:     connected,
		Posts:         c.posts.Load(),
		PostFailures:  c.postFailures.Load(),
		PostBytes:     c.postBytes.Load(),
		PostMicros:    c.postMicros.Load(),
		MaxPostMicros: c.maxPostMicros.Load(),
		WSReconnects:  c.wsReconnects.Load(),
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
	conn, _, err := dialer.Dial(wsURL, header)
	if err != nil {
		return false, fmt.Errorf("volga v6 websocket dial: %w", err)
	}
	w.connMu.Lock()
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
	var envelope struct {
		Operation string `json:"operation"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Message == "" {
		return
	}
	if envelope.Operation == "ping" || (envelope.Operation != "SESSION" && envelope.Operation != "WORKER") {
		return
	}

	var inner struct {
		T       string          `json:"t"`
		UserID  int             `json:"userId"`
		Bundle  json.RawMessage `json:"bundle"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal([]byte(envelope.Message), &inner); err != nil {
		return
	}
	deliverData := inner.UserID != w.auth.UserID
	switch inner.T {
	case "relay":
		w.handleRelay(inner.Message, deliverData)
	case "exchange":
		w.handleExchange(inner.Bundle, deliverData)
	}
}

func (w *volgaV6YandexWS) handleRelay(raw json.RawMessage, deliverData bool) {
	var relay struct {
		Bundle []json.RawMessage `json:"bundle"`
	}
	if err := json.Unmarshal(raw, &relay); err != nil {
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
		return
	}
	records := decodeBatch(decoded)
	frame, ok := decodeVolgaV6Records(records)
	if !ok {
		return
	}
	if w.carrier.onFrame != nil {
		w.carrier.onFrame(frame)
	}
}

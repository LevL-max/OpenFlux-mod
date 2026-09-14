package yandex

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

type YandexDocsInfo struct {
	CookieStr   string
	Token       string
	DocID       string
	CallbackURL string
	UserID      string
	Origin      string
	Host        string
	WsURL       string
	Permissions map[string]interface{}
	OpenCmd     map[string]interface{}
}

type DocSession struct {
	Info       YandexDocsInfo
	Conn       *websocket.Conn
	WriteQueue chan []byte
	UserID     string
	writeMu    sync.Mutex
}

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.Conn.WriteMessage(messageType, data)
}

type YandexDocsTransport struct {
	*transport.BaseTransport

	url     string
	session *DocSession

	userCounter atomic.Int32
	baseUserID  string

	perfQueueFull   atomic.Uint64
	perfWSWrites    atomic.Uint64
	perfWSBytes     atomic.Uint64
	perfWriteErrors atomic.Uint64
	perfQueuePeak   atomic.Int64
}

func NewYandexDocsTransport(url string, config transport.TransportConfig) *YandexDocsTransport {
	t := &YandexDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		url:           url,
	}
	t.baseUserID = randUserID()
	return t
}

func (t *YandexDocsTransport) Start() error {
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}

	t.baseUserID = randUserID()
	utils.SafeGo("yandex.keepAlive", t.keepAliveLoop)
	utils.SafeGo("yandex.perfStats", t.perfStatsLoop)
	t.connectToDoc(0)

	return nil
}

func (t *YandexDocsTransport) Send(data []byte) error {
	if !t.IsConnected() {
		return fmt.Errorf("transport not connected")
	}

	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()

	if session == nil {
		return fmt.Errorf("no active session")
	}

	select {
	case session.WriteQueue <- data:
		t.RecordSend(len(data))
		t.observeQueueDepth(len(session.WriteQueue))
		return nil
	default:
		t.perfQueueFull.Add(1)
		t.observeQueueDepth(cap(session.WriteQueue))
		return fmt.Errorf("write queue full")
	}
}

func (t *YandexDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}

	utils.Debugf("[YDOCS] connectToDoc attempt ...")

	go func() {
		defer func() {
			if r := recover(); r != nil {
				utils.Debugf("[PANIC] recovered in yandex.connect: %v", r)
			}
		}()
		t.Mu.Lock()
		existingSession := t.session
		t.Mu.Unlock()

		var userID string
		if existingSession != nil {
			userID = existingSession.UserID
		} else {
			suffix := fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
			userID = t.baseUserID + suffix
		}

		info, err := t.fetchDocInfo(t.url, userID)
		if err != nil {
			utils.Debugf("[YDOCS] fetchDocInfo failed: %v", err)
			t.scheduleReconnect(attempt)
			return
		}

		if info.UserID != "" {
			userID = info.UserID
		}

		dialer := websocket.Dialer{
			HandshakeTimeout: 15 * time.Second,
			NetDialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", "Mozilla/5.0")
		headers.Set("Origin", info.Origin)
		headers.Set("Cookie", info.CookieStr)
		headers.Set("Host", info.Host)

		utils.Debugf("[YDOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.Dial(info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
			}
			utils.Debugf("[YDOCS] WebSocket dial failed (http %d): %v", status, err)
			t.scheduleReconnect(attempt)
			return
		}

		writeQueue := make(chan []byte, t.GetConfig().MaxQueueSize)
		if existingSession != nil {
			writeQueue = existingSession.WriteQueue
		}

		session := &DocSession{
			Info:       info,
			Conn:       conn,
			WriteQueue: writeQueue,
			UserID:     userID,
		}

		t.Mu.Lock()
		t.session = session
		t.Mu.Unlock()

		if existingSession == nil {
			utils.SafeGo("yandex.writer", t.writerLoop)
		}

		// OnlyOffice 2026 / Engine.IO + Socket.IO handshake.
		conn.SetReadDeadline(time.Now().Add(10 * time.Second))

		_, hello, err := conn.ReadMessage()
		if err != nil {
			utils.Debugf("[YDOCS] Engine.IO hello failed: %v", err)
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}
		if !strings.HasPrefix(string(hello), "0") {
			utils.Debugf("[YDOCS] Unexpected Engine.IO hello")
			conn.Close()
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}

		auth1 := fmt.Sprintf(`40{"token":"%s"}`, info.Token)
		if err := session.safeWrite(websocket.TextMessage, []byte(auth1)); err != nil {
			utils.Debugf("[YDOCS] Socket.IO auth write failed: %v", err)
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}

		licenseSeen := false
		for i := 0; i < 10; i++ {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[YDOCS] Pre-auth read failed: %v", err)
				t.SetConnected(false)
				t.scheduleReconnect(attempt)
				return
			}

			text := string(message)
			if text == "2" {
				session.safeWrite(websocket.TextMessage, []byte("3"))
				continue
			}
			if strings.Contains(text, `"type":"license"`) {
				licenseSeen = true
				break
			}
		}

		if !licenseSeen {
			utils.Debugf("[YDOCS] License message not received")
			conn.Close()
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}

		authData := map[string]interface{}{
			"type": "auth", "docid": info.DocID, "token": "fghhfgsjdgfjs",
			"user": map[string]interface{}{"id": userID}, "editorType": 0,
			"lastOtherSaveTime": -1, "permissions": info.Permissions,
			"openCmd": info.OpenCmd, "coEditingMode": "fast", "jwtOpen": info.Token,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authData})

		if err := session.safeWrite(
			websocket.TextMessage,
			[]byte(fmt.Sprintf("42%s", string(messagePart))),
		); err != nil {
			utils.Debugf("[YDOCS] Document auth write failed: %v", err)
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}

		authOK := false
		for i := 0; i < 20; i++ {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[YDOCS] Auth response read failed: %v", err)
				t.SetConnected(false)
				t.scheduleReconnect(attempt)
				return
			}

			text := string(message)

			if text == "2" {
				session.safeWrite(websocket.TextMessage, []byte("3"))
				continue
			}

			if strings.Contains(text, `"type":"authChanges"`) {
				ack := `42["message",{"type":"authChangesAck"}]`
				if err := session.safeWrite(websocket.TextMessage, []byte(ack)); err != nil {
					utils.Debugf("[YDOCS] authChangesAck write failed: %v", err)
					t.SetConnected(false)
					t.scheduleReconnect(attempt)
					return
				}
				continue
			}

			if strings.Contains(text, `"type":"auth"`) &&
				strings.Contains(text, `"result":1`) {
				authOK = true
				break
			}
		}

		if !authOK {
			utils.Debugf("[YDOCS] Document authentication not accepted")
			conn.Close()
			t.SetConnected(false)
			t.scheduleReconnect(attempt)
			return
		}

		conn.SetReadDeadline(time.Time{})
		t.SetConnected(true)
		utils.Debugf("[YDOCS] OnlyOffice authentication successful")

		connectedAt := time.Now()
		for t.IsRunning() {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[YDOCS] Read error: %v", err)
				t.SetConnected(false)
				conn.Close()
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	}()
}

func (t *YandexDocsTransport) writerLoop() {
	for t.IsRunning() {
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		if session == nil || session.Conn == nil {
			time.Sleep(10 * time.Millisecond)
			continue
		}

		select {
		case packet := <-session.WriteQueue:
			payload := base64.StdEncoding.EncodeToString(packet)
			msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"18;%s"}]`, payload)

			if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
				t.perfWriteErrors.Add(1)
				utils.Debugf("[YDOCS] Write error: %v", err)
			} else {
				t.perfWSWrites.Add(1)
				t.perfWSBytes.Add(uint64(len(msg)))
			}
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
}

func (t *YandexDocsTransport) observeQueueDepth(depth int) {
	d := int64(depth)
	for {
		old := t.perfQueuePeak.Load()
		if d <= old {
			return
		}
		if t.perfQueuePeak.CompareAndSwap(old, d) {
			return
		}
	}
}

func (t *YandexDocsTransport) perfStatsLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	prev := t.Stats()
	prevWSWrites := t.perfWSWrites.Load()
	prevWSBytes := t.perfWSBytes.Load()
	prevQueueFull := t.perfQueueFull.Load()
	prevWriteErrors := t.perfWriteErrors.Load()
	last := time.Now()

	for t.IsRunning() {
		<-ticker.C

		now := time.Now()
		seconds := now.Sub(last).Seconds()
		if seconds <= 0 {
			seconds = 5
		}
		last = now

		stats := t.Stats()
		wsWrites := t.perfWSWrites.Load()
		wsBytes := t.perfWSBytes.Load()
		queueFull := t.perfQueueFull.Load()
		writeErrors := t.perfWriteErrors.Load()

		queueLen := 0
		queueCap := 0

		t.Mu.RLock()
		session := t.session
		if session != nil && session.WriteQueue != nil {
			queueLen = len(session.WriteQueue)
			queueCap = cap(session.WriteQueue)
		}
		t.Mu.RUnlock()

		peak := t.perfQueuePeak.Swap(int64(queueLen))
		if peak < int64(queueLen) {
			peak = int64(queueLen)
		}

		txBytes := stats.BytesSent - prev.BytesSent
		rxBytes := stats.BytesReceived - prev.BytesReceived
		txPackets := stats.PacketsSent - prev.PacketsSent
		rxPackets := stats.PacketsRecv - prev.PacketsRecv
		wsWriteDelta := wsWrites - prevWSWrites
		wsByteDelta := wsBytes - prevWSBytes

		utils.Debugf(
			"[PERF] connected=%t tx=%.2fMbps rx=%.2fMbps tx_pps=%.0f rx_pps=%.0f ws_msg_s=%.0f ws_payload=%.2fMbps queue=%d/%d peak=%d queue_full=%d(+%d) write_err=%d(+%d) reconnects=%d(+%d)",
			stats.Connected,
			float64(txBytes)*8/seconds/1000000,
			float64(rxBytes)*8/seconds/1000000,
			float64(txPackets)/seconds,
			float64(rxPackets)/seconds,
			float64(wsWriteDelta)/seconds,
			float64(wsByteDelta)*8/seconds/1000000,
			queueLen,
			queueCap,
			peak,
			queueFull,
			queueFull-prevQueueFull,
			writeErrors,
			writeErrors-prevWriteErrors,
			stats.Reconnects,
			stats.Reconnects-prev.Reconnects,
		)

		prev = stats
		prevWSWrites = wsWrites
		prevWSBytes = wsBytes
		prevQueueFull = queueFull
		prevWriteErrors = writeErrors
	}
}

func (t *YandexDocsTransport) keepAliveLoop() {
	ticker := time.NewTicker(t.GetConfig().KeepAliveInterval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"18;---KA---"}]`

	for t.IsRunning() {
		<-ticker.C
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()

		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[YDOCS] Keep-alive failed: %v", err)
				t.SetConnected(false)
			}
		}
	}
}

func (t *YandexDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)

	if strings.Contains(text, "---KA---") {
		return
	}

	// Socket.IO ping - respond with pong (use safeWrite)
	if text == "2" {
		if session != nil && session.Conn != nil {
			session.safeWrite(websocket.TextMessage, []byte("3"))
		}
		return
	}
	if text == "3" {
		return
	}

	if strings.Contains(text, "saveChanges") || strings.Contains(text, "cursor") {
		base64Str := t.extractBase64String(text)
		if base64Str == "" {
			return
		}

		decoded, err := base64.StdEncoding.DecodeString(base64Str)
		if err != nil {
			utils.Debugf("[YDOCS] Base64 decode error: %v", err)
			return
		}

		t.RecordReceive(len(decoded))
		t.CallReceive(decoded)
	}
}

func (t *YandexDocsTransport) extractBase64String(response string) string {
	if strings.Contains(response, "saveChanges") {
		marker := `"excelAdditionalInfo":"`
		left := strings.Index(response, marker) + len(marker)
		if left < len(marker) {
			return ""
		}
		right := strings.Index(response[left:], `"`)
		if right == -1 {
			return ""
		}
		return response[left : left+right]
	}

	re := regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)
	matches := re.FindStringSubmatch(response)
	if len(matches) > 1 {
		return matches[1]
	}
	return ""
}

func (t *YandexDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}

	d := reconnectBackoff(next)
	utils.Debugf("[YDOCS] reconnecting in %v (attempt %d)", d, next)
	time.Sleep(d)
	if !t.IsRunning() {
		return
	}

	t.RecordReconnect()
	t.connectToDoc(next)
}

func reconnectBackoff(n int) time.Duration {
	if n < 1 {
		n = 1
	}
	shift := n - 1
	if shift > 5 {
		shift = 5
	}
	d := 500 * time.Millisecond * time.Duration(1<<uint(shift))
	if d > 15*time.Second {
		d = 15 * time.Second
	}
	d += time.Duration(rand.Int63n(int64(d/2) + 1))
	return d
}

func (t *YandexDocsTransport) fetchDocInfo(url, userID string) (YandexDocsInfo, error) {
	if strings.HasPrefix(url, "file://") {
		return loadStaticDocInfo(strings.TrimPrefix(url, "file://"), userID)
	}
	client := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("stopped after 10 redirects (login required? doc not public?)")
			}
			return nil
		},
		Timeout: 15 * time.Second,
	}

	utils.Debugf("[YDOCS] fetchDocInfo GET %s", url)
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("User-Agent", "Mozilla/5.0")
	resp, err := client.Do(req)
	if err != nil {
		return YandexDocsInfo{}, err
	}
	defer resp.Body.Close()

	htmlBytes, _ := io.ReadAll(resp.Body)
	html := string(htmlBytes)
	utils.Debugf("[YDOCS] response status=%d finalURL=%s body=%dB",
		resp.StatusCode, resp.Request.URL.String(), len(html))

	var cookies []string
	for _, c := range resp.Cookies() {
		cookies = append(cookies, fmt.Sprintf("%s=%s", c.Name, c.Value))
	}

	re := regexp.MustCompile(`<script[^>]*id="client-config"[^>]*>(.*?)</script>`)
	matches := re.FindStringSubmatch(html)
	if len(matches) < 2 {
		hint := "no client-config script"
		if strings.Contains(html, "passport") || strings.Contains(strings.ToLower(html), "login") {
			hint = "looks like a login page (doc not public?)"
		}
		return YandexDocsInfo{}, fmt.Errorf("config not found: %s (status %d, final %s)",
			hint, resp.StatusCode, resp.Request.URL.String())
	}

	var config map[string]interface{}
	if err := json.Unmarshal([]byte(matches[1]), &config); err != nil {
		return YandexDocsInfo{}, fmt.Errorf("client-config is not valid JSON: %w", err)
	}
	officeAction, ok := config["officeActionData"].(map[string]interface{})
	if !ok || officeAction == nil {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData missing")
	}

	editorConfigRaw, ok := officeAction["editor_config"].(map[string]interface{})
	if !ok || editorConfigRaw == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config nil - will reconnect")
	}

	balancerURL, ok := officeAction["balancer_url"].(string)
	if !ok || balancerURL == "" {
		return YandexDocsInfo{}, fmt.Errorf("officeActionData.balancer_url missing")
	}
	host := strings.TrimPrefix(balancerURL, "https://")
	build, err := fetchOnlyOfficeBuild(client, balancerURL)
	if err != nil {
		return YandexDocsInfo{}, fmt.Errorf("OnlyOffice build detection failed: %w", err)
	}
	utils.Debugf("[YDOCS] Detected OnlyOffice build: %s", build)

	document, ok := editorConfigRaw["document"].(map[string]interface{})
	if !ok || document == nil {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document missing")
	}

	token, ok := editorConfigRaw["token"].(string)
	if !ok || token == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.token missing")
	}

	docKey, ok := document["key"].(string)
	if !ok || docKey == "" {
		return YandexDocsInfo{}, fmt.Errorf("editor_config.document.key missing")
	}

	runtimeEditorConfig, ok := editorConfigRaw["editorConfig"].(map[string]interface{})
	if !ok || runtimeEditorConfig == nil {
		return YandexDocsInfo{}, fmt.Errorf("editorConfig missing")
	}

	runtimeUser, ok := runtimeEditorConfig["user"].(map[string]interface{})
	if !ok || runtimeUser == nil {
		return YandexDocsInfo{}, fmt.Errorf("editorConfig.user missing")
	}

	yandexUserID, _ := runtimeUser["id"].(string)
	if yandexUserID == "" {
		return YandexDocsInfo{}, fmt.Errorf("editorConfig.user.id missing")
	}

	perms, _ := document["permissions"].(map[string]interface{})
	if perms == nil {
		perms = make(map[string]interface{})
	}

	return YandexDocsInfo{
		CookieStr:   strings.Join(cookies, "; "),
		Token:       token,
		DocID:       docKey,
		UserID:      yandexUserID,
		Origin:      balancerURL,
		Host:        host,
		WsURL:       fmt.Sprintf("wss://%s/%s/doc/%s/c/?EIO=4&transport=websocket", host, build, docKey),
		Permissions: perms,
		OpenCmd: map[string]interface{}{
			"c":      "open",
			"id":     docKey,
			"userid": yandexUserID,
			"format": document["fileType"],
			"url":    document["url"],
			"title":  document["title"],
			"lcid":   25,
		},
	}, nil
}

func fetchOnlyOfficeBuild(client *http.Client, balancerURL string) (string, error) {
	apiURL := strings.TrimRight(balancerURL, "/") + "/web-apps/apps/api/documents/api.js"

	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("api.js HTTP status %d", resp.StatusCode)
	}

	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	re := regexp.MustCompile(`\?_dc=(\d+\.\d+\.\d+-\d+)`)
	m := re.FindSubmatch(b)
	if len(m) != 2 {
		return "", fmt.Errorf("OnlyOffice build not found in api.js")
	}

	return string(m[1]), nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}

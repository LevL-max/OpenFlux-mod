// Package mailru tunnels packets through Mail.ru's cloud document editor
// (docs.datacloudmail.ru, R7-Office), the same coauthoring backend family as
// Yandex Docs. Both peers open one public document anonymously and carry
// packets in the "cursor" field of the collaborative editing protocol.
//
// Ported from upstream OpenFlux 0.5.1 (transport/mailru) with these changes:
// the cursor declares the packet's real length (Yandex drops a mismatch since
// 2026-10-09, see transport/yandex), the fake saveChanges edits are off unless
// asked for, and there is no interface binding or cookie exchange: the
// document is opened as an anonymous visitor.
package mailru

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/utils"
)

// Mail.ru rejects an old Chrome UA on the WebSocket upgrade with HTTP 409 on
// some edge nodes; a current Firefox UA is accepted for both the API and the
// WebSocket (upstream issue #164).
const mailruUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:155.0) Gecko/20100101 Firefox/155.0"

var cursorPayloadRe = regexp.MustCompile(`"cursor":"[^;]+;([^"]+)"`)

// saveChangesMessageTemplate is a saveChanges message as Mail.ru's web editor
// sends it, with the participant's UserId and UserShortId substituted. It makes
// a real edit to the document, so it is sent only when editor activity is on.
const saveChangesMessageTemplate = `42["message",{"type":"saveChanges","changes":"[\"76;AgAAADEA//8BAOwbfF7pEAAALQEAAAMAAAAAAAAAAAAAAAAAAAAAAAAA9v///xoAAAAyADAAMgA2AC4AMgAuADEALgAyADIANgA4AA==\",\"35;BgAAADYAMgA3AAEAHAABAAAAAAAAAAEAAABhAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAQAAAAEAAABzAAAAAAMAAAA=\",\"35;BgAAADYAMgA3AAEAHAABAAAAAgAAAAEAAABkAAAAAAMAAAA=\"]","startSaveChanges":true,"endSaveChanges":true,"isCoAuthoring":false,"isExcel":false,"deleteIndex":null,"excelAdditionalInfo":"{\"UserId\":\"%s\",\"UserShortId\":\"%s\",\"CursorInfo\":\"14;BgAAADYAMgA3AAMAAAA=\"}","unlock":false,"releaseLocks":false}]`

type MailruDocsInfo struct {
	Token        string
	DocKey       string
	WsURL        string
	FileType     string
	DocURL       string
	DocTitle     string
	Permissions  map[string]interface{}
	CallbackURL  string
	EditorUserID string
}

type DocSession struct {
	Info             MailruDocsInfo
	Conn             *websocket.Conn
	WriteQueue       chan queuedPacket
	UserID           string
	writeMu          sync.Mutex
	heartbeatTimeout time.Duration
}

type queuedPacket struct {
	data []byte
	at   time.Time
}

const maxQueuedBytes = 8 << 20
const maxQueuedAge = 3 * time.Second

func (s *DocSession) safeWrite(messageType int, data []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	// A write into a half-open connection (NAT dropped it, the network
	// changed) would otherwise block until the kernel gives up, minutes.
	_ = s.Conn.SetWriteDeadline(time.Now().Add(docWriteTimeout))
	return s.Conn.WriteMessage(messageType, data)
}

// docWriteTimeout bounds one WebSocket write to the document.
const docWriteTimeout = 20 * time.Second

const mailruSocketIOHandshakeTimeout = 15 * time.Second

// waitMailruSocketIO completes the Engine.IO / Socket.IO handshake before
// editor authentication is sent: Mail.ru requires the Engine.IO open frame to
// be consumed before the Socket.IO connect packet.
func waitMailruSocketIO(session *DocSession, token string) error {
	if session == nil || session.Conn == nil {
		return fmt.Errorf("mailru: websocket session is nil")
	}
	conn := session.Conn
	if err := conn.SetReadDeadline(time.Now().Add(mailruSocketIOHandshakeTimeout)); err != nil {
		return fmt.Errorf("mailru: set handshake deadline: %w", err)
	}
	defer conn.SetReadDeadline(time.Time{})

	waitFor := func(match func(string) bool) error {
		for {
			messageType, payload, err := conn.ReadMessage()
			if err != nil {
				return err
			}
			if messageType != websocket.TextMessage {
				continue
			}
			text := string(payload)
			// Engine.IO heartbeat may arrive while handshaking.
			if text == "2" {
				if err := session.safeWrite(websocket.TextMessage, []byte("3")); err != nil {
					return fmt.Errorf("mailru: send Engine.IO pong: %w", err)
				}
				continue
			}
			if match(text) {
				return nil
			}
			utils.Debugf("[M-DOCS] handshake: ignoring server frame %q", text)
		}
	}

	// Engine.IO open: 0{"sid":"...", ...}
	if err := waitFor(func(text string) bool {
		if !strings.HasPrefix(text, "0{") {
			return false
		}
		var hello struct {
			PingInterval int `json:"pingInterval"`
			PingTimeout  int `json:"pingTimeout"`
		}
		if json.Unmarshal([]byte(text[1:]), &hello) == nil && hello.PingInterval > 0 && hello.PingTimeout > 0 {
			session.heartbeatTimeout = time.Duration(hello.PingInterval+hello.PingTimeout)*time.Millisecond + 5*time.Second
		}
		return true
	}); err != nil {
		return fmt.Errorf("mailru: wait for Engine.IO open: %w", err)
	}
	utils.Debugf("[M-DOCS] Engine.IO open")

	if err := session.safeWrite(websocket.TextMessage, []byte(fmt.Sprintf(`40{"token":"%s"}`, token))); err != nil {
		return fmt.Errorf("mailru: send Socket.IO connect: %w", err)
	}
	// Socket.IO acknowledgement: 40{"sid":"..."}
	if err := waitFor(func(text string) bool { return strings.HasPrefix(text, "40") }); err != nil {
		return fmt.Errorf("mailru: wait for Socket.IO connect: %w", err)
	}
	utils.Debugf("[M-DOCS] Socket.IO connected")
	return nil
}

type MailruDocsTransport struct {
	*transport.BaseTransport

	weblink string
	session *DocSession
	opening *DocSession
	ctx     context.Context
	cancel  context.CancelFunc

	userCounter atomic.Int32
	baseUserID  string

	cookieJar *cookiejar.Jar

	// editorActivity sends fake saveChanges edits as a live editor would.
	editorActivity bool

	// reconnecting is set while a scheduled reconnect waits out its backoff,
	// so that two failures never open two sessions to the document.
	reconnecting atomic.Bool

	// refusals counts, in a row, the times Mail.ru answered but did not let
	// the peer into the document (an HTTP error from its API or WebSocket).
	refusals atomic.Int32

	statusMu    sync.Mutex
	statusLast  map[string]time.Time
	done        chan struct{}
	stopOnce    sync.Once
	queuedBytes atomic.Int64
	queueDrops  atomic.Uint64
	writeErrors atomic.Uint64
	wsWrites    atomic.Uint64
	wsBytes     atomic.Uint64
}

// NewMailruDocsTransport accepts either a bare weblink ("AbCd/EfGh") or a full
// public URL ("https://cloud.mail.ru/public/AbCd/EfGh").
func NewMailruDocsTransport(weblink string, config transport.TransportConfig) *MailruDocsTransport {
	jar, _ := cookiejar.New(nil)
	ctx, cancel := context.WithCancel(context.Background())
	return &MailruDocsTransport{
		BaseTransport: transport.NewBaseTransport(config),
		weblink:       normalizeWeblink(weblink),
		baseUserID:    randUserID(),
		cookieJar:     jar,
		statusLast:    map[string]time.Time{},
		done:          make(chan struct{}),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// SetEditorActivity turns the fake saveChanges edits on or off. Call it before
// Start. They change the document, so they are off by default.
func (t *MailruDocsTransport) SetEditorActivity(enabled bool) { t.editorActivity = enabled }

func normalizeWeblink(weblink string) string {
	weblink = strings.TrimSpace(weblink)
	for _, prefix := range []string{
		"https://cloud.mail.ru/public/",
		"http://cloud.mail.ru/public/",
		"https://cloud.mail.ru/",
		"http://cloud.mail.ru/",
	} {
		if strings.HasPrefix(weblink, prefix) {
			return strings.Trim(strings.TrimPrefix(weblink, prefix), "/")
		}
	}
	return weblink
}

// statusThrottled logs a status line at most once per interval for a key.
func (t *MailruDocsTransport) statusThrottled(key string, every time.Duration, format string, args ...interface{}) {
	t.statusMu.Lock()
	if last, ok := t.statusLast[key]; ok && time.Since(last) < every {
		t.statusMu.Unlock()
		return
	}
	t.statusLast[key] = time.Now()
	t.statusMu.Unlock()
	utils.Statusf(format, args...)
}

func (t *MailruDocsTransport) Start() error {
	select {
	case <-t.done:
		return fmt.Errorf("transport already stopped")
	default:
	}
	if err := t.BaseTransport.Start(); err != nil {
		return err
	}
	t.baseUserID = randUserID()
	utils.SafeGo("mailru.keepAlive", t.keepAliveLoop)
	utils.SafeGo("mailru.stats", t.perfStatsLoop)
	utils.SafeGo("mailru.documentKey", func() { t.documentKeyLoop(30 * time.Second) })
	if t.editorActivity {
		utils.SafeGo("mailru.editorActivity", t.editorActivityLoop)
	}
	t.connectToDoc(0)
	return nil
}

func (t *MailruDocsTransport) Send(data []byte) error {
	if !t.IsRunning() {
		return fmt.Errorf("transport stopped")
	}
	t.Mu.RLock()
	session := t.session
	t.Mu.RUnlock()
	if session == nil {
		return fmt.Errorf("no active session")
	}
	if t.queuedBytes.Add(int64(len(data))) > maxQueuedBytes {
		t.queuedBytes.Add(-int64(len(data)))
		t.queueDrops.Add(1)
		return fmt.Errorf("write queue byte limit")
	}
	packet := queuedPacket{data: append([]byte(nil), data...), at: time.Now()}
	select {
	case session.WriteQueue <- packet:
		t.RecordSend(len(data))
		return nil
	default:
		t.queuedBytes.Add(-int64(len(data)))
		t.queueDrops.Add(1)
		return fmt.Errorf("write queue full")
	}
}

func (t *MailruDocsTransport) connectToDoc(attempt int) {
	if !t.IsRunning() {
		return
	}
	utils.Debugf("[M-DOCS] connectToDoc attempt %d", attempt)

	utils.SafeGo("mailru.connect", func() {
		t.Mu.Lock()
		existingSession := t.session
		t.Mu.Unlock()

		var userID string
		if existingSession != nil {
			userID = existingSession.UserID
		} else {
			userID = t.baseUserID + fmt.Sprintf("%03d", t.userCounter.Add(1)%1000)
		}

		info, err := t.fetchDocInfo(t.weblink)
		if err != nil {
			if !isNetworkFailure(err) {
				t.refusals.Add(1)
			}
			utils.Debugf("[M-DOCS] fetchDocInfo failed: %v", err)
			t.statusThrottled("fetch", time.Minute, "[M-DOCS] cannot open the document: %v; retrying", err)
			t.scheduleReconnect(attempt)
			return
		}

		dialer := websocket.Dialer{
			// Mail.ru's nginx answers 409 Conflict to an upgrade that does not
			// offer permessage-deflate, and gorilla/websocket offers it only
			// with compression enabled (upstream issue #163).
			EnableCompression: true,
			HandshakeTimeout:  15 * time.Second,
			NetDialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		}
		headers := http.Header{}
		headers.Set("User-Agent", mailruUserAgent)
		// A real browser always sends Accept-Language; some edge nodes answer
		// 409 without it (upstream issue #164).
		headers.Set("Accept-Language", "en-US,en;q=0.9")
		headers.Set("Origin", "https://docs.datacloudmail.ru")

		utils.Debugf("[M-DOCS] WebSocket dial %s", info.WsURL)
		conn, resp, err := dialer.DialContext(t.ctx, info.WsURL, headers)
		if err != nil {
			status := 0
			if resp != nil {
				status = resp.StatusCode
				t.refusals.Add(1)
			}
			utils.Debugf("[M-DOCS] WebSocket dial failed (http %d): %v", status, err)
			if status != 0 {
				t.statusThrottled("dial", time.Minute, "[M-DOCS] document server refused the connection (http %d); retrying", status)
			} else {
				t.statusThrottled("dial", time.Minute, "[M-DOCS] cannot reach the document server; retrying")
			}
			t.scheduleReconnect(attempt)
			return
		}

		writeQueue := make(chan queuedPacket, t.GetConfig().MaxQueueSize)
		if existingSession != nil {
			writeQueue = existingSession.WriteQueue
		}
		session := &DocSession{Info: info, Conn: conn, WriteQueue: writeQueue, UserID: userID}
		t.Mu.Lock()
		if !t.IsRunning() {
			t.Mu.Unlock()
			_ = conn.Close()
			return
		}
		t.opening = session
		t.Mu.Unlock()
		defer func() {
			t.Mu.Lock()
			if t.opening == session {
				t.opening = nil
			}
			t.Mu.Unlock()
		}()

		if err := waitMailruSocketIO(session, info.Token); err != nil {
			utils.Debugf("[M-DOCS] Socket.IO handshake failed: %v", err)
			t.statusThrottled("handshake", time.Minute, "[M-DOCS] document server did not finish the handshake; retrying")
			_ = conn.Close()
			t.scheduleReconnect(attempt)
			return
		}

		authMsg := map[string]interface{}{
			"type":                "auth",
			"docid":               info.DocKey,
			"documentCallbackUrl": info.CallbackURL,
			"token":               "fghhfgsjdgfjs",
			"user": map[string]interface{}{
				"id":        info.EditorUserID,
				"username":  userID,
				"indexUser": -1,
			},
			"editorType":         0,
			"lastOtherSaveTime":  -1,
			"block":              []interface{}{},
			"documentFormatSave": 65,
			"view":               false,
			"isCloseCoAuthoring": false,
			"openCmd": map[string]interface{}{
				"c":               "open",
				"id":              info.DocKey,
				"userid":          info.EditorUserID,
				"format":          info.FileType,
				"url":             info.DocURL,
				"title":           info.DocTitle,
				"lcid":            25,
				"nobase64":        true,
				"convertToOrigin": ".pdf.xps.oxps.djvu",
			},
			"lang":                  "ru",
			"mode":                  "edit",
			"permissions":           info.Permissions,
			"IsAnonymousUser":       false,
			"timezoneOffset":        -180,
			"coEditingMode":         "fast",
			"jwtOpen":               info.Token,
			"time":                  1000,
			"supportAuthChangesAck": true,
		}
		messagePart, _ := json.Marshal([]interface{}{"message", authMsg})
		if err := session.safeWrite(websocket.TextMessage, []byte("42"+string(messagePart))); err == nil {
			err = waitMailruEditorAuth(session)
			if err != nil {
				_ = conn.Close()
				if errors.Is(err, errEditorAuthRejected) {
					t.refusals.Add(1)
				}
				t.statusThrottled("auth", time.Minute, "[M-DOCS] document authorization failed; retrying")
				t.scheduleReconnect(attempt)
				return
			}
		} else {
			_ = conn.Close()
			t.scheduleReconnect(attempt)
			return
		}
		t.Mu.Lock()
		if !t.IsRunning() {
			t.Mu.Unlock()
			_ = conn.Close()
			return
		}
		t.session = session
		t.opening = nil
		t.SetConnected(true)
		t.Mu.Unlock()
		t.refusals.Store(0)
		if existingSession == nil {
			utils.SafeGo("mailru.writer", t.writerLoop)
		}
		utils.Statusf("[M-DOCS] connected to the document")

		connectedAt := time.Now()
		for t.IsRunning() {
			_, message, err := conn.ReadMessage()
			if err != nil {
				utils.Debugf("[M-DOCS] Read error: %v", err)
				t.statusThrottled("drop", time.Minute, "[M-DOCS] connection to the document dropped: %v; reconnecting", err)
				t.dropConnection(session)
				next := attempt
				if time.Since(connectedAt) > 15*time.Second {
					next = -1
				}
				t.scheduleReconnect(next)
				return
			}
			t.handleMessage(session, message)
		}
	})
}

func (t *MailruDocsTransport) writerLoop() {
	var queue chan queuedPacket
	for t.IsRunning() && queue == nil {
		t.Mu.Lock()
		if t.session != nil {
			queue = t.session.WriteQueue
		}
		t.Mu.Unlock()
		if queue == nil {
			time.Sleep(5 * time.Millisecond)
		}
	}
	if queue == nil {
		return
	}

	var pending *queuedPacket
	defer func() {
		if pending != nil {
			t.queuedBytes.Add(-int64(len(pending.data)))
		}
	}()
	for t.IsRunning() {
		if pending == nil {
			select {
			case packet := <-queue:
				pending = &packet
			case <-t.done:
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}
		if time.Since(pending.at) > maxQueuedAge {
			t.queuedBytes.Add(-int64(len(pending.data)))
			t.queueDrops.Add(1)
			pending = nil
			continue
		}
		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil || !t.IsConnected() {
			time.Sleep(15 * time.Millisecond)
			continue
		}
		// The cursor declares the packet's real length before Base64, as the
		// web editor does.
		msg := fmt.Sprintf(`42["message",{"type":"cursor","cursor":"%d;%s"}]`, len(pending.data), base64.StdEncoding.EncodeToString(pending.data))
		if err := session.safeWrite(websocket.TextMessage, []byte(msg)); err != nil {
			t.writeErrors.Add(1)
			utils.Debugf("[M-DOCS] Write error: %v", err)
			t.dropConnection(session)
			continue
		}
		t.wsWrites.Add(1)
		t.wsBytes.Add(uint64(len(msg)))
		t.queuedBytes.Add(-int64(len(pending.data)))
		pending = nil
	}
}

func (t *MailruDocsTransport) keepAliveLoop() {
	ticker := time.NewTicker(t.GetConfig().KeepAliveInterval)
	defer ticker.Stop()
	keepAliveMsg := `42["message",{"type":"cursor","cursor":"18;---KA---"}]`
	for t.IsRunning() {
		select {
		case <-ticker.C:
		case <-t.done:
			return
		}
		t.Mu.Lock()
		session := t.session
		t.Mu.Unlock()
		if session != nil && session.Conn != nil {
			if err := session.safeWrite(websocket.TextMessage, []byte(keepAliveMsg)); err != nil {
				utils.Debugf("[M-DOCS] Keep-alive failed, closing the connection to reconnect: %v", err)
				t.dropConnection(session)
			}
		}
	}
}

// editorActivityLoop sends a saveChanges edit every 0.5-5 s, as a live editor
// would. Off by default (SetEditorActivity): each one changes the document.
func (t *MailruDocsTransport) editorActivityLoop() {
	const (
		minDelay = 500 * time.Millisecond
		maxDelay = 5 * time.Second
	)
	for t.IsRunning() {
		select {
		case <-time.After(minDelay + time.Duration(rand.Int63n(int64(maxDelay-minDelay)+1))):
		case <-t.done:
			return
		}
		t.Mu.RLock()
		session := t.session
		t.Mu.RUnlock()
		if session == nil || session.Conn == nil {
			continue
		}
		// A read-only link may not save: the server closes the connection on
		// the first saveChanges, over and over.
		if !canEdit(session.Info.Permissions) {
			continue
		}
		if err := session.safeWrite(websocket.TextMessage, buildSaveChanges(session)); err != nil {
			utils.Debugf("[M-DOCS] saveChanges write error: %v", err)
		}
	}
}

// canEdit says whether the document may be edited through the joined link. A
// document that does not say is treated as editable.
func canEdit(permissions map[string]interface{}) bool {
	edit, ok := permissions["edit"].(bool)
	return !ok || edit
}

func buildSaveChanges(session *DocSession) []byte {
	userID := session.Info.EditorUserID
	if userID == "" {
		userID = session.UserID
	}
	short := userID
	if len(short) > 1 {
		short = short[:len(short)-1]
	}
	return []byte(fmt.Sprintf(saveChangesMessageTemplate, userID, short))
}

func (t *MailruDocsTransport) handleMessage(session *DocSession, data []byte) {
	text := string(data)
	if handled, err := handleMailruControl(session, data); handled {
		if err != nil {
			t.dropConnection(session)
		}
		return
	}
	if text == "3" {
		return
	}
	if strings.Contains(text, `"type":"auth"`) && strings.Contains(text, `"result":1`) {
		utils.Debugf("[M-DOCS] Auth OK for user %s", session.UserID)
		return
	}
	if strings.Contains(text, "cursor") {
		for _, base64Str := range cursorPayloads(text) {
			decoded, err := base64.StdEncoding.DecodeString(base64Str)
			if err != nil {
				utils.Debugf("[M-DOCS] Base64 decode error: %v", err)
				continue
			}
			t.RecordReceive(len(decoded))
			t.CallReceive(decoded)
		}
	}
}

// cursorPayloads returns the Base64 payload of every cursor entry in a server
// message, in order, without the keep-alives. Under load the server batches
// several entries into one message.
func cursorPayloads(text string) []string {
	var out []string
	for _, m := range cursorPayloadRe.FindAllStringSubmatch(text, -1) {
		if len(m) > 1 && m[1] != "---KA---" {
			out = append(out, m[1])
		}
	}
	return out
}

func (t *MailruDocsTransport) scheduleReconnect(attempt int) {
	next := attempt + 1
	if !t.IsRunning() || next >= t.GetConfig().MaxReconnectAttempts {
		return
	}
	if !t.reconnecting.CompareAndSwap(false, true) {
		return // one is already waiting
	}
	d := reconnectBackoff(next)
	if r := refusalBackoff(int(t.refusals.Load())); r > d {
		d = r
	}
	utils.Debugf("[M-DOCS] reconnecting in %v (attempt %d)", d, next)
	select {
	case <-time.After(d):
	case <-t.done:
		return
	}
	t.reconnecting.Store(false) // a failed connect schedules the next one itself
	if !t.IsRunning() {
		return
	}
	t.RecordReconnect()
	t.connectToDoc(next)
}

// reconnectBackoff returns an exponential backoff with jitter, capped at 15 s.
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
	return d + time.Duration(rand.Int63n(int64(d/2)+1))
}

// refusalBackoff spaces retries after Mail.ru itself turned the peer away (an
// HTTP error for the document or its WebSocket) rather than the network
// failing, as transport/yandex does: a removed document or a closed link is not
// fixed by asking every 15 s around the clock. After three quick tries the
// pause doubles from 30 s up to 10 min.
func refusalBackoff(refusals int) time.Duration {
	if refusals <= 3 {
		return 0
	}
	d := 30 * time.Second << min(refusals-4, 5)
	if d > 10*time.Minute {
		d = 10 * time.Minute
	}
	return d + time.Duration(rand.Int63n(int64(d/4)+1))
}

// isNetworkFailure reports a failure to reach Mail.ru at all: DNS, connect,
// reset or timeout. Anything else means Mail.ru answered but did not open the
// document.
func isNetworkFailure(err error) bool {
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	var op *net.OpError
	var dns *net.DNSError
	var ne net.Error
	switch {
	case errors.As(err, &op), errors.As(err, &dns):
		return true
	case errors.As(err, &ne) && ne.Timeout():
		return true
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return true
	}
	return false
}

// editAPI is Mail.ru's public-document editor API (a variable for tests).
var editAPI = "https://cloud.mail.ru/api/v4/r7/edit"

// fetchDocInfo POSTs to Mail.ru's public-document editor API as an anonymous
// visitor and returns what is needed to open the collaborative WebSocket.
func (t *MailruDocsTransport) fetchDocInfo(weblink string) (MailruDocsInfo, error) {
	client := &http.Client{Jar: t.cookieJar, Timeout: 15 * time.Second}
	jsonData, _ := json.Marshal(map[string]string{
		"x-email":  "anonym",
		"public":   "/" + weblink,
		"platform": "desktop_web",
	})
	req, err := http.NewRequestWithContext(t.ctx, "POST", editAPI, bytes.NewBuffer(jsonData))
	if err != nil {
		return MailruDocsInfo{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	req.Header.Set("User-Agent", mailruUserAgent)
	req.Header.Set("X-Api-Version", "4")
	req.Header.Set("Referer", fmt.Sprintf("https://cloud.mail.ru/public/%s?weblink=%s", weblink, weblink))

	resp, err := client.Do(req)
	if err != nil {
		return MailruDocsInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return MailruDocsInfo{}, fmt.Errorf("API returned status %d", resp.StatusCode)
	}
	bodyBytes, _ := io.ReadAll(resp.Body)
	var res map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &res); err != nil {
		return MailruDocsInfo{}, fmt.Errorf("failed to parse JSON: %w", err)
	}

	apiBase, _ := res["api"].(string)
	token, _ := res["token"].(string)
	document, ok := res["document"].(map[string]interface{})
	if !ok || document == nil {
		return MailruDocsInfo{}, fmt.Errorf("document object missing")
	}
	docKey, _ := document["key"].(string)
	fileType, _ := document["fileType"].(string)
	docURL, _ := document["url"].(string)
	docTitle, _ := document["title"].(string)
	permissions, _ := document["permissions"].(map[string]interface{})
	if permissions == nil {
		permissions = make(map[string]interface{})
	}
	editorConfig, ok := res["editorConfig"].(map[string]interface{})
	if !ok || editorConfig == nil {
		return MailruDocsInfo{}, fmt.Errorf("editorConfig object missing")
	}
	callbackURL, _ := editorConfig["callbackUrl"].(string)
	var editorUserID string
	if userObj, _ := editorConfig["user"].(map[string]interface{}); userObj != nil {
		editorUserID, _ = userObj["id"].(string)
	}

	wsBase := strings.Replace(apiBase, "https://", "wss://", 1)
	return MailruDocsInfo{
		Token:        token,
		DocKey:       docKey,
		WsURL:        fmt.Sprintf("%s/doc/%s/c/?EIO=4&transport=websocket", wsBase, docKey),
		FileType:     fileType,
		DocURL:       docURL,
		DocTitle:     docTitle,
		Permissions:  permissions,
		CallbackURL:  callbackURL,
		EditorUserID: editorUserID,
	}, nil
}

func randUserID() string {
	return fmt.Sprintf("%010d", rand.New(rand.NewSource(time.Now().UnixNano())).Intn(1000000000))
}

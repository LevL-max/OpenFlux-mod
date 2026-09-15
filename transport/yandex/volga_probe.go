package yandex

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// VolgaProbeResult intentionally contains no token, sign, cookie, session ID,
// request path, or document identifier values. It is safe to print in logs.
type VolgaProbeResult struct {
	Stage          string `json:"stage"`
	HTTPStatus     int    `json:"http_status,omitempty"`
	FinalHost      string `json:"final_host,omitempty"`
	Layout         string `json:"layout,omitempty"`
	Challenge      string `json:"challenge,omitempty"`
	HasActionURL   bool   `json:"has_action_url,omitempty"`
	HasAccessToken bool   `json:"has_access_token,omitempty"`
	HasTTL         bool   `json:"has_ttl,omitempty"`
	TTL            string `json:"ttl,omitempty"`
	HasToken       bool   `json:"has_token,omitempty"`
	HasRequestPath bool   `json:"has_request_path,omitempty"`
	HasSessionID   bool   `json:"has_session_id,omitempty"`
	HasXivaUser    bool   `json:"has_xiva_user,omitempty"`
	HasXivaSign    bool   `json:"has_xiva_sign,omitempty"`
	HasXivaTS      bool   `json:"has_xiva_ts,omitempty"`
	PushConnected  bool   `json:"push_connected,omitempty"`
	Error          string `json:"error,omitempty"`
}

func classifyChallenge(status int, finalURL string, body []byte) string {
	lower := strings.ToLower(string(body))
	u := strings.ToLower(finalURL)
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case status == http.StatusForbidden:
		return "forbidden"
	case strings.Contains(u, "passport.yandex") || strings.Contains(lower, "passport") || strings.Contains(lower, "login"):
		return "login"
	case strings.Contains(lower, "captcha") || strings.Contains(lower, "smartcaptcha"):
		return "captcha"
	case strings.Contains(u, "/document/error/"):
		return "document_error"
	default:
		return "none"
	}
}

func fetchVolgaClientConfig(docURL string) (map[string]interface{}, *http.Client, string, int, []byte, error) {
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			MaxIdleConns:        20,
			MaxIdleConnsPerHost: 20,
			IdleConnTimeout:     30 * time.Second,
		},
		Timeout: 20 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	req, err := http.NewRequest(http.MethodGet, docURL, nil)
	if err != nil {
		return nil, client, "", 0, nil, err
	}
	req.Header.Set("User-Agent", volgaUserAgent)
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	resp, err := client.Do(req)
	if err != nil {
		return nil, client, "", 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, client, resp.Request.URL.String(), resp.StatusCode, nil, err
	}
	finalURL := resp.Request.URL.String()
	m := reClientConfig.FindSubmatch(body)
	if len(m) < 2 {
		return nil, client, finalURL, resp.StatusCode, body, fmt.Errorf("client-config not found")
	}
	var cfg map[string]interface{}
	dec := json.NewDecoder(bytes.NewReader(m[1]))
	dec.UseNumber()
	if err := dec.Decode(&cfg); err != nil {
		return nil, client, finalURL, resp.StatusCode, body, fmt.Errorf("parse client-config: %w", err)
	}
	return cfg, client, finalURL, resp.StatusCode, body, nil
}

// ProbeVolgaClassify performs only public-page discovery and client-config
// classification. It does not submit the Volga auth form or open a WebSocket.
func ProbeVolgaClassify(docURL string) VolgaProbeResult {
	result := VolgaProbeResult{Stage: "classify", Layout: "unknown", Challenge: "none"}
	cfg, _, finalURL, status, body, err := fetchVolgaClientConfig(docURL)
	result.HTTPStatus = status
	if u, parseErr := url.Parse(finalURL); parseErr == nil {
		result.FinalHost = u.Host
	}
	result.Challenge = classifyChallenge(status, finalURL, body)
	if err != nil {
		result.Error = err.Error()
		return result
	}

	if officeType := getStr(cfg, "officeType"); officeType != "" {
		result.Layout = officeType
	}
	office, _ := cfg["officeActionData"].(map[string]interface{})
	if office == nil {
		result.Error = "officeActionData missing"
		return result
	}
	result.HasActionURL = getStr(office, "action_url") != ""
	result.HasAccessToken = getStr(office, "access_token") != ""
	if ttl, ok := office["access_token_ttl"]; ok && ttl != nil {
		result.HasTTL = true
		result.TTL = formatTTL(ttl)
	}
	if result.Layout == "unknown" {
		switch {
		case result.HasActionURL && result.HasAccessToken:
			result.Layout = "volga"
		case office["editor_config"] != nil || getStr(office, "balancer_url") != "":
			result.Layout = "only_office"
		}
	}
	return result
}

// ProbeVolgaAuth executes the same authorize() path used by the transport and
// returns only presence/health metadata. Secret values are never returned.
func ProbeVolgaAuth(docURL string) VolgaProbeResult {
	result := ProbeVolgaClassify(docURL)
	result.Stage = "auth"
	if result.Challenge != "none" || result.Error != "" || result.Layout != "volga" {
		if result.Error == "" && result.Layout != "volga" {
			result.Error = "document is not a Volga layout"
		}
		return result
	}
	auth, err := authorize(docURL)
	if err != nil {
		result.Error = err.Error()
		if strings.Contains(strings.ToLower(result.Error), "document/error") {
			result.Challenge = "document_error"
		}
		return result
	}
	result.HasToken = auth.Token != ""
	result.HasRequestPath = auth.RequestPath != ""
	result.HasSessionID = auth.SessionID != ""
	result.HasXivaUser = auth.UserIDStr != ""
	result.HasXivaSign = auth.Sign != ""
	result.HasXivaTS = auth.TS != ""
	return result
}

// ProbeVolgaWS executes auth, opens the same Xiva push WebSocket endpoint used
// by Volga, waits for one server frame, and closes it. It sends no tunnel data.
func ProbeVolgaWS(docURL string) VolgaProbeResult {
	result := ProbeVolgaAuth(docURL)
	result.Stage = "ws"
	if result.Error != "" {
		return result
	}
	auth, err := authorize(docURL)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	wsURL := "wss://push.yandex.ru/v2/subscribe/websocket?" +
		"service=volga" +
		"&user=" + url.QueryEscape(auth.UserIDStr) +
		"&sign=" + url.QueryEscape(auth.Sign) +
		"&ts=" + url.QueryEscape(auth.TS) +
		"&client=web" +
		"&session=" + url.QueryEscape(auth.SessionID) +
		"&fetch_history=" + url.QueryEscape(auth.UserIDStr+":volga:0:1") +
		"&x_request_attempt=0"

	header := http.Header{}
	header.Set("User-Agent", volgaUserAgent)
	header.Set("Origin", "https://volga.yandex.ru")
	var cookieParts []string
	for _, c := range auth.Cookies {
		cookieParts = append(cookieParts, c.Name+"="+c.Value)
	}
	if len(cookieParts) > 0 {
		header.Set("Cookie", strings.Join(cookieParts, "; "))
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	conn, resp, err := dialer.Dial(wsURL, header)
	if err != nil {
		if resp != nil {
			result.HTTPStatus = resp.StatusCode
		}
		result.Error = fmt.Sprintf("push websocket: %v", err)
		return result
	}
	defer conn.Close()
	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := conn.ReadMessage(); err != nil {
		result.Error = fmt.Sprintf("push websocket read: %v", err)
		return result
	}
	result.PushConnected = true
	return result
}

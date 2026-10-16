package mailru

// Private file storage for the opt-in hybrid carrier. Browser credentials never
// enter chat frames, filenames, redirects to download hosts, or error messages.
import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

const cloudBase = "https://cloud.mail.ru"
const hybridBlockLimit = 512 << 10

var cloudHash = regexp.MustCompile(`^[A-Fa-f0-9]{40}(?:[;\s]+[0-9]+)?$`)
var hybridObjectID = regexp.MustCompile(`^[a-f0-9]{32}-[a-f0-9]{16}$`)

type blockStore interface {
	Put(context.Context, string, []byte) error
	Get(context.Context, string, int) ([]byte, error)
	Delete(context.Context, string) error
}
type cloudState struct {
	expires                        time.Time
	client                         *http.Client
	token, email, upload, download string
	profile                        [32]byte
}
type cloudStore struct {
	profilePath, folder string
	mu                  sync.Mutex
	state               *cloudState
}
type cloudFailure struct{ status int }

func (e cloudFailure) Error() string { return fmt.Sprintf("Mail.ru file API status %d", e.status) }

func newCloudStore(profile, document string) *cloudStore {
	sum := sha256.Sum256([]byte(normalizeWeblink(document)))
	return &cloudStore{profilePath: profile, folder: "/OpenFlux-hybrid-" + hex.EncodeToString(sum[:6])}
}
func cloudURL(raw string, download bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" {
		return false
	}
	h := u.Hostname()
	return h == "cloud.mail.ru" || strings.HasSuffix(h, ".cloud.mail.ru") || (download && strings.HasSuffix(h, ".datacloudmail.ru"))
}
func (s *cloudStore) session(ctx context.Context) (*cloudState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.profilePath)
	if err != nil || len(raw) > browserProfileLimit {
		return nil, errors.New("Mail.ru browser profile unavailable")
	}
	sum := sha256.Sum256(raw)
	if s.state != nil && s.state.profile == sum && time.Now().Before(s.state.expires) {
		return s.state, nil
	}
	var p browserProfile
	if json.Unmarshal(raw, &p) != nil || len(p.Cookies) == 0 {
		return nil, errors.New("Mail.ru browser profile invalid")
	}
	jar, _ := cookiejar.New(nil)
	base, _ := url.Parse(cloudBase)
	for k, v := range p.Cookies {
		if !cookieName.MatchString(k) || strings.ContainsAny(v, "\r\n\x00;") {
			return nil, errors.New("invalid browser cookie")
		}
		jar.SetCookies(base, []*http.Cookie{{Name: k, Value: v, Path: "/", Secure: true}})
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 12
	tr.MaxConnsPerHost = 16
	st := &cloudState{profile: sum, client: &http.Client{Jar: jar, Transport: tr, Timeout: 20 * time.Second}}
	st.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 || !cloudURL(req.URL.String(), true) {
			return errors.New("file redirect rejected")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("X-Csrf-Token")
			req.Header.Del("Cookie")
			req.Header.Del("Authorization")
		}
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", cloudBase+"/home/", nil)
	req.Header.Set("User-Agent", p.Headers["user-agent"])
	resp, err := st.client.Do(req)
	if err != nil {
		return nil, errors.New("Mail.ru file bootstrap unavailable")
	}
	page, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	resp.Body.Close()
	if readErr != nil || resp.StatusCode != 200 {
		return nil, cloudFailure{resp.StatusCode}
	}
	for _, loc := range bootstrapTokens.FindAllIndex(page, 8) {
		var tokens struct {
			CSRF string `json:"csrf"`
		}
		if json.NewDecoder(bytes.NewReader(page[loc[1]:])).Decode(&tokens) == nil && tokens.CSRF != "" {
			st.token = tokens.CSRF
			break
		}
	}
	if st.token == "" {
		return nil, errors.New("Mail.ru file API requires fresh browser cookies")
	}
	var folder struct {
		Email string `json:"email"`
	}
	if err = st.api(ctx, "folder", url.Values{"home": {"/"}, "limit": {"0"}}, false, &folder); err != nil {
		return nil, err
	}
	st.email = folder.Email
	if st.email == "" {
		return nil, errors.New("Mail.ru account unavailable")
	}
	var dispatch struct {
		Body map[string][]struct {
			URL string `json:"url"`
		} `json:"body"`
	}
	if err = st.api(ctx, "dispatcher", nil, false, &dispatch); err != nil {
		return nil, err
	}
	if len(dispatch.Body["upload"]) == 0 || len(dispatch.Body["get"]) == 0 {
		return nil, errors.New("file servers unavailable")
	}
	st.upload = dispatch.Body["upload"][0].URL
	st.download = dispatch.Body["get"][0].URL
	if !cloudURL(st.upload, false) || !cloudURL(st.download, true) {
		return nil, errors.New("file server rejected")
	}
	// Copy the authenticated Cloud session only to the dispatcher-selected
	// Cloud hosts. Never seed credentials on datacloudmail.ru redirect hosts.
	for _, target := range []string{st.upload, st.download} {
		seedCloudCookies(jar, base, target)
	}
	if err = st.api(ctx, "folder", url.Values{"home": {s.folder}, "limit": {"0"}}, false, nil); err != nil {
		var failure cloudFailure
		if !errors.As(err, &failure) || failure.status != 404 {
			return nil, err
		}
		if err = st.api(ctx, "folder/add", url.Values{"home": {s.folder}, "conflict": {"strict"}}, true, nil); err != nil {
			if err = st.api(ctx, "folder", url.Values{"home": {s.folder}, "limit": {"0"}}, false, nil); err != nil {
				return nil, err
			}
		}
	}
	if s.state != nil {
		s.state.client.CloseIdleConnections()
	}
	st.expires = time.Now().Add(5 * time.Minute)
	s.state = st
	return st, nil
}
func seedCloudCookies(jar http.CookieJar, base *url.URL, target string) {
	if !cloudURL(target, false) {
		return
	}
	u, _ := url.Parse(target)
	for _, c := range jar.Cookies(base) {
		jar.SetCookies(u, []*http.Cookie{{Name: c.Name, Value: c.Value, Path: "/", Secure: true}})
	}
}
func (st *cloudState) api(ctx context.Context, endpoint string, params url.Values, post bool, dst any) error {
	if params == nil {
		params = url.Values{}
	}
	params.Set("token", st.token)
	params.Set("api", "2")
	target := cloudBase + "/api/v2/" + endpoint
	method := "GET"
	var body io.Reader
	if post {
		method = "POST"
		body = strings.NewReader(params.Encode())
	} else {
		target += "?" + params.Encode()
	}
	req, _ := http.NewRequestWithContext(ctx, method, target, body)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-Csrf-Token", st.token)
	req.Header.Set("Origin", cloudBase)
	req.Header.Set("Referer", cloudBase+"/home/")
	resp, err := st.client.Do(req)
	if err != nil {
		return errors.New("Mail.ru file API request failed")
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return errors.New("file API response unavailable")
	}
	var status struct {
		Status int `json:"status"`
	}
	if json.Unmarshal(raw, &status) != nil {
		return errors.New("invalid file API response")
	}
	if resp.StatusCode != 200 {
		return cloudFailure{resp.StatusCode}
	}
	if status.Status != 200 {
		return cloudFailure{status.Status}
	}
	if dst != nil && json.Unmarshal(raw, dst) != nil {
		return errors.New("invalid file API response")
	}
	return nil
}
func (s *cloudStore) invalidate(st *cloudState, err error) {
	var e cloudFailure
	if errors.As(err, &e) && (e.status == 401 || e.status == 403) {
		s.mu.Lock()
		if s.state == st {
			s.state = nil
			st.client.CloseIdleConnections()
		}
		s.mu.Unlock()
	}
}
func (s *cloudStore) path(id string) (string, error) {
	if !hybridObjectID.MatchString(id) {
		return "", errors.New("invalid hybrid object")
	}
	return s.folder + "/ofh1-" + id + ".bin", nil
}
func (s *cloudStore) Put(ctx context.Context, id string, data []byte) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	if len(data) > hybridBlockLimit {
		return errors.New("block too large")
	}
	st, err := s.session(ctx)
	if err != nil {
		return err
	}
	u, _ := url.Parse(st.upload)
	q := u.Query()
	q.Set("cloud_domain", "2")
	q.Set("x-email", st.email)
	u.RawQuery = q.Encode()
	req, _ := http.NewRequestWithContext(ctx, "PUT", u.String(), bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := st.client.Do(req)
	if err != nil {
		return errors.New("file upload failed")
	}
	raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 1024))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		err = cloudFailure{resp.StatusCode}
		s.invalidate(st, err)
		return err
	}
	if readErr != nil {
		return errors.New("upload response unavailable")
	}
	text := strings.TrimSpace(string(raw))
	if !cloudHash.MatchString(text) {
		return errors.New("invalid upload response")
	}
	hash := text[:40]
	err = st.api(ctx, "file/add", url.Values{"home": {path}, "hash": {hash}, "size": {fmt.Sprint(len(data))}, "conflict": {"strict"}}, true, nil)
	s.invalidate(st, err)
	return err
}
func (s *cloudStore) Get(ctx context.Context, id string, size int) ([]byte, error) {
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	if size <= 0 || size > hybridBlockLimit {
		return nil, errors.New("invalid block size")
	}
	st, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(st.download, "/") + "/" + strings.TrimPrefix(path, "/")
	req, _ := http.NewRequestWithContext(ctx, "GET", target, nil)
	resp, err := st.client.Do(req)
	if err != nil {
		return nil, errors.New("file download failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		err = cloudFailure{resp.StatusCode}
		s.invalidate(st, err)
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(size)+1))
	if err != nil || len(data) != size {
		return nil, errors.New("file size mismatch")
	}
	return data, nil
}
func (s *cloudStore) Delete(ctx context.Context, id string) error {
	path, err := s.path(id)
	if err != nil {
		return err
	}
	st, err := s.session(ctx)
	if err != nil {
		return err
	}
	err = st.api(ctx, "file/remove", url.Values{"home": {path}}, true, nil)
	s.invalidate(st, err)
	return err
}

// Remove only recognizable protocol objects in this document's dedicated folder
// after an hour. Active objects expire after two minutes, so another process's
// live uploads are never swept. No user-created folder or unrelated file is removed.
func (s *cloudStore) Sweep(ctx context.Context) error {
	st, err := s.session(ctx)
	if err != nil {
		return err
	}
	var listing struct {
		Body struct {
			List []struct {
				Home  string `json:"home"`
				Type  string `json:"type"`
				Mtime int64  `json:"mtime"`
			} `json:"list"`
		} `json:"body"`
	}
	if err = st.api(ctx, "folder", url.Values{"home": {s.folder}, "limit": {"1000"}}, false, &listing); err != nil {
		s.invalidate(st, err)
		return err
	}
	removed := 0
	for _, entry := range listing.Body.List {
		if entry.Type != "file" || !strings.HasPrefix(entry.Home, s.folder+"/ofh1-") || !strings.HasSuffix(entry.Home, ".bin") || entry.Mtime < 946684800 || entry.Mtime >= time.Now().Add(-time.Hour).Unix() {
			continue
		}
		id := strings.TrimSuffix(strings.TrimPrefix(entry.Home, s.folder+"/ofh1-"), ".bin")
		if !hybridObjectID.MatchString(id) {
			continue
		}
		if err = s.Delete(ctx, id); err != nil {
			return err
		}
		removed++
		if removed >= 16 {
			break
		}
	}
	return nil
}

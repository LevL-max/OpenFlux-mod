//go:build volga

package yandex

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestVolgaV6HTTPProtocolSelection(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	for mode, want := range map[string]int{"http1": 1, "http2": 2} {
		t.Run(mode, func(t *testing.T) {
			cfg := defaultVolgaV6YandexConfig()
			cfg.HTTPProtocol = mode
			tr := volgaV6HTTPTransport(cfg)
			tr.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
			defer tr.CloseIdleConnections()
			client := &http.Client{Transport: tr}
			resp, err := client.Get(server.URL)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			_, _ = io.Copy(io.Discard, resp.Body)
			if resp.ProtoMajor != want {
				t.Fatalf("negotiated %s for mode %s", resp.Proto, mode)
			}
		})
	}
}

func TestVolgaV6HTTPStatusAccounting(t *testing.T) {
	c := newOfflineVolgaV6YandexCarrier(t, 1, 8000)
	status := 204
	c.http = &http.Client{Transport: volgaV6LocalRoundTripper(func(*http.Request) (*http.Response, error) {
		if status == 0 {
			return nil, errors.New("test network failure")
		}
		return &http.Response{StatusCode: status, ProtoMajor: 1, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header)}, nil
	})}
	for _, code := range []int{204, 429, 503, 0} {
		status = code
		err := c.postRelayBody([]byte("{}"))
		if (err == nil) != (code == 204) {
			t.Fatalf("status=%d err=%v", code, err)
		}
	}
	s := c.Snapshot()
	if s.Posts != 4 || s.PostFailures != 3 || s.HTTPTransportErrors != 1 || s.HTTP1Posts != 3 || s.HTTPStatuses[204] != 1 || s.HTTPStatuses[429] != 1 || s.HTTPStatuses[503] != 1 {
		t.Fatalf("wrong HTTP accounting: %+v", s)
	}
}

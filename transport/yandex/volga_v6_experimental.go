//go:build volga

package yandex

// Explicit opt-in for the standalone lab executable. No release CLI, default,
// configuration migration, or Legacy transport calls this constructor.
import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"sync/atomic"
	"time"

	"universal-bypass-tool/transport"
)

type VolgaV6ExperimentalOptions struct {
	Documents      []string
	CookieStore    string
	BrowserProfile string

	// PostsPerSecond overrides the relay pacing rate. 0 keeps the validated
	// 360/s default. In shared mode it is the aggregate ceiling for all lanes;
	// in per-lane mode it is the budget for each lane, so the aggregate becomes
	// PostsPerSecond * len(Documents).
	PostsPerSecond float64
	// PerLaneBudget gives every lane its own pacing gate (and its own 429
	// cooldown) instead of one shared gate. This is the A/B lever for pushing
	// aggregate throughput past the shared 360/s ceiling; it must be validated
	// live for HTTP 429 before use, because the shared gate is the known-safe
	// configuration. Default false preserves the shared gate.
	PerLaneBudget bool
	// SendWorkers overrides the number of concurrent relay send workers. 0 keeps
	// the default. It exists so a controlled run can rule out send concurrency
	// as a local bottleneck when interpreting an aggregate-rate result.
	SendWorkers int
	// Quiet makes a server wait silently for a client: no repairs and only a
	// keepalive carrier recycle while no client frame arrives. A client keeps
	// fast recovery.
	Quiet bool
	// QuietRecycleInterval overrides the quiet server's safety renewal (0 keeps
	// the default). SessionPingInterval overrides the editor session ping (0
	// keeps the default, negative disables it). Both exist so a live run can
	// fall back to frequent renewals without a rebuild.
	QuietRecycleInterval time.Duration
	SessionPingInterval  time.Duration
}

func NewVolgaV6Experimental(o VolgaV6ExperimentalOptions) (*YandexVolgaV6Transport, error) {
	if len(o.Documents) < 1 || len(o.Documents) > 4 {
		return nil, errors.New("Volga lab requires 1-4 document lanes")
	}
	rate := o.PostsPerSecond
	if rate == 0 {
		rate = 360
	}
	if rate < 1 || rate > 2000 {
		return nil, errors.New("Volga lab: posts_per_second must be 1-2000")
	}
	for _, doc := range o.Documents {
		if !volgaV6AllowedURL(doc, true) {
			return nil, errors.New("Volga lab: invalid document URL")
		}
	}
	p := newVolgaV6AuthProvider(o.CookieStore)
	if o.BrowserProfile != "" {
		f, err := os.Open(o.BrowserProfile)
		if err != nil {
			return nil, errors.New("Volga lab: cannot open browser profile")
		}
		defer f.Close()
		b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		var profile struct {
			Headers map[string]string `json:"headers"`
			Editors map[string]string `json:"editors"`
		}
		if err != nil || len(b) > 1<<20 || json.Unmarshal(b, &profile) != nil {
			return nil, errors.New("Volga lab: invalid browser profile")
		}
		p.browserHeaders = make(http.Header)
		for k, v := range profile.Headers {
			p.browserHeaders.Set(k, v)
		}
		for _, doc := range o.Documents {
			if editor := profile.Editors[doc]; editor != "" {
				if !volgaV6AllowedURL(editor, true) {
					return nil, errors.New("Volga lab: invalid editor URL")
				}
				p.document(doc).seed.Editor = editor
			}
		}
	}
	if o.SendWorkers != 0 && (o.SendWorkers < 1 || o.SendWorkers > 256) {
		return nil, errors.New("Volga lab: send_workers must be 1-256")
	}
	if o.QuietRecycleInterval != 0 && (o.QuietRecycleInterval < time.Minute || o.QuietRecycleInterval > time.Hour) {
		return nil, errors.New("Volga lab: quiet_recycle_seconds must be 60-3600")
	}
	if o.SessionPingInterval > 0 && (o.SessionPingInterval < 15*time.Second || o.SessionPingInterval > 5*time.Minute) {
		return nil, errors.New("Volga lab: session_ping_seconds must be 15-300, or negative to disable")
	}
	cfg := DefaultVolgaV6TransportConfig(o.Documents)
	cfg.Telemetry = false
	cfg.QueueSize, cfg.SendQueueSize, cfg.SendWorkers = 512, 64, 64
	if o.SendWorkers != 0 {
		cfg.SendWorkers = o.SendWorkers
	}
	cfg.Runtime.CarrierStartTimeout = 30 * time.Second
	cfg.Runtime.Quiet = o.Quiet
	cfg.Runtime.credentialsChanged = p.blockedCookiesChanged
	if o.QuietRecycleInterval != 0 {
		cfg.Runtime.QuietRecycleInterval = o.QuietRecycleInterval
	}
	if o.SessionPingInterval != 0 {
		cfg.Yandex.SessionPingInterval = o.SessionPingInterval
	}
	cfg.Yandex.HTTPProtocol = "http1"
	cfg.Yandex.RelayEnvelope = "minimal"
	cfg.Yandex.RelayPostsPerSecond = rate
	cfg.Yandex.authorize = p.authorize
	// One gate per lane index, created once and reused across handoff
	// generations so a lane keeps its own Retry-After cooldown. In shared mode
	// every lane index points at the same gate, reproducing the 360/s aggregate
	// ceiling exactly.
	laneGates := make([]*volgaV6RelayGate, len(o.Documents))
	shared := &volgaV6RelayGate{rate: rate}
	for i := range laneGates {
		if o.PerLaneBudget {
			laneGates[i] = &volgaV6RelayGate{rate: rate}
		} else {
			laneGates[i] = shared
		}
	}
	factory := func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		pool := &experimentalV6Pool{generation: generation}
		for i, doc := range o.Documents {
			laneCfg := cfg.Yandex
			laneCfg.relayGate = laneGates[i]
			pool.lanes = append(pool.lanes, newVolgaV6YandexCarrier(generation, doc, laneCfg, onFrame))
		}
		return pool, nil
	}
	t := newYandexVolgaV6TransportWithFactory(transport.DefaultConfig(), cfg, factory)
	t.authBlocked = p.anyBlocked
	return t, nil
}

// AuthBlocked exposes only a boolean; document cookies and tokens stay private.
func (t *YandexVolgaV6Transport) AuthBlocked() bool {
	return t.authBlocked != nil && t.authBlocked()
}

// SendContext applies bounded queue backpressure without polling or dropping.
func (t *YandexVolgaV6Transport) SendContext(ctx context.Context, data []byte) error {
	if !t.started.Load() || t.stopped.Load() {
		return errVolgaV6TransportNotStarted
	}
	if len(data) == 0 {
		return nil
	}
	if len(data) > volgaV6MaxRecordBytes {
		return errVolgaV6InvalidPayload
	}
	cp := append([]byte(nil), data...)
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.ctx.Done():
		return errVolgaV6TransportNotStarted
	case t.queue <- cp:
		return nil
	}
}

type experimentalV6Pool struct {
	generation uint64
	lanes      []*volgaV6YandexCarrier
	next       atomic.Uint64
}

func (p *experimentalV6Pool) Generation() uint64 { return p.generation }
func (p *experimentalV6Pool) Start(ctx context.Context) error {
	for _, c := range p.lanes {
		if err := c.Start(ctx); err != nil {
			p.Stop()
			return err
		}
	}
	return nil
}
func (p *experimentalV6Pool) Stop() error {
	var first error
	for _, c := range p.lanes {
		if err := c.Stop(); err != nil && first == nil {
			first = err
		}
	}
	return first
}
func (p *experimentalV6Pool) SendVolgaV6(f volgaV6WireFrame) error {
	return p.lanes[(p.next.Add(1)-1)%uint64(len(p.lanes))].SendVolgaV6(f)
}
func (p *experimentalV6Pool) VolgaV6PhysicalHealth(now time.Time) volgaV6PhysicalHealth {
	out := volgaV6PhysicalHealth{Known: true, Generation: p.generation, Connected: true, HTTPStatuses: make(map[int]uint64)}
	for _, c := range p.lanes {
		s := c.VolgaV6PhysicalHealth(now)
		out.Connected = out.Connected && s.Connected
		out.Posts += s.Posts
		out.PostFailures += s.PostFailures
		out.PostBytes += s.PostBytes
		out.PostMicros += s.PostMicros
		out.MaxPostMicros = max(out.MaxPostMicros, s.MaxPostMicros)
		out.WSReconnects += s.WSReconnects
		out.HTTP1Posts += s.HTTP1Posts
		out.HTTP2Posts += s.HTTP2Posts
		out.WSDataFrames += s.WSDataFrames
		out.WSAckFrames += s.WSAckFrames
		out.WSDecodeErrors += s.WSDecodeErrors
		out.WSRawMessages += s.WSRawMessages
		out.WSIgnoredMessages += s.WSIgnoredMessages
		out.WSJSONErrors += s.WSJSONErrors
		out.HTTPTransportErrors += s.HTTPTransportErrors
		out.SessionPings += s.SessionPings
		out.SessionPingFailures += s.SessionPingFailures
		out.SessionPingRejected += s.SessionPingRejected
		out.RelayPostsPerSecond = s.RelayPostsPerSecond
		if s.RelayRetryAt.After(out.RelayRetryAt) {
			out.RelayRetryAt = s.RelayRetryAt
		}
		for code, n := range s.HTTPStatuses {
			out.HTTPStatuses[code] += n
		}
	}
	return out
}

package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"openflux-volga-lab/internal/recordconn"
	"openflux-volga-lab/internal/tunnel"
	"universal-bypass-tool/transport/yandex"
)

const version = "0.1.3-review1-experimental"

type config struct {
	Protocol       string   `json:"protocol"`
	Role           string   `json:"role"`
	Documents      []string `json:"documents"`
	CookieStore    string   `json:"cookie_store"`
	BrowserProfile string   `json:"browser_profile"`
	SharedKey      string   `json:"shared_key"`
	Listen         string   `json:"listen"`
	AllowedTargets []string `json:"allowed_targets"`
	IdleSeconds    int      `json:"idle_seconds"`
	// Optional throughput A/B knobs. Absent keeps the validated shared 360/s.
	PostsPerSecond float64 `json:"posts_per_second"`
	PerLaneBudget  bool    `json:"per_lane_budget"`
	// Optional stream-shape knobs. Absent keeps the validated defaults. These
	// exist so a controlled run can tell a provider limit apart from a local
	// bottleneck (record size, sender credit window, send concurrency).
	RecordChunkBytes  int `json:"record_chunk_bytes"`
	RecordWindowBytes int `json:"record_window_bytes"`
	FlushMillis       int `json:"flush_millis"` // <0 disables coalescing
	SendWorkers       int `json:"send_workers"`
}

var outputMu sync.Mutex

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func event(kind string, data any) {
	outputMu.Lock()
	defer outputMu.Unlock()
	json.NewEncoder(os.Stdout).Encode(map[string]any{"time": time.Now().UTC(), "event": kind, "data": data})
}
func load(path string) (config, []byte, error) {
	var c config
	f, e := os.Open(path)
	if e != nil {
		return c, nil, errors.New("cannot read private configuration")
	}
	defer f.Close()
	d := json.NewDecoder(io.LimitReader(f, 1<<20))
	d.DisallowUnknownFields()
	if d.Decode(&c) != nil {
		return c, nil, errors.New("invalid configuration JSON")
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return c, nil, errors.New("trailing configuration data")
	}
	if c.Protocol != "volga-stream-v1" || (c.Role != "client" && c.Role != "server") {
		return c, nil, errors.New("protocol must be volga-stream-v1; role must be client or server")
	}
	key, e := hex.DecodeString(c.SharedKey)
	if e != nil || len(key) != 32 {
		return c, nil, errors.New("shared_key must contain 32 random bytes encoded as 64 hex characters")
	}
	if c.IdleSeconds == 0 {
		c.IdleSeconds = 60
	}
	if c.IdleSeconds < 5 || c.IdleSeconds > 60 {
		return c, nil, errors.New("idle_seconds must be 5-60")
	}
	if c.Role == "client" && c.Listen == "" {
		c.Listen = "127.0.0.1:1088"
	}
	if c.PostsPerSecond != 0 && (c.PostsPerSecond < 1 || c.PostsPerSecond > 2000) {
		return c, nil, errors.New("posts_per_second must be 1-2000 when set")
	}
	if c.RecordChunkBytes != 0 && (c.RecordChunkBytes < 256 || c.RecordChunkBytes > recordconn.MaxChunk) {
		return c, nil, errors.New("record_chunk_bytes must be 256-16384 when set")
	}
	if c.RecordWindowBytes != 0 && (c.RecordWindowBytes < 4*maxInt(c.RecordChunkBytes, recordconn.Chunk) || c.RecordWindowBytes > recordconn.MaxWindow) {
		return c, nil, errors.New("record_window_bytes must be >=4 chunks and <=8 MiB when set")
	}
	if c.SendWorkers != 0 && (c.SendWorkers < 1 || c.SendWorkers > 256) {
		return c, nil, errors.New("send_workers must be 1-256 when set")
	}
	if c.Role == "server" && len(c.AllowedTargets) == 0 {
		return c, nil, errors.New("experimental server requires allowed_targets")
	}
	for _, s := range c.AllowedTargets {
		if _, _, e = net.SplitHostPort(s); e != nil {
			return c, nil, errors.New("invalid allowed target")
		}
	}
	return c, recordconn.DeriveKey(key), nil
}
func run() error {
	path := flag.String("config", "", "private JSON configuration")
	showVersion := flag.Bool("version", false, "show experimental build version")
	flag.Parse()
	if *showVersion {
		fmt.Println("openflux-volga-lab", version)
		return nil
	}
	if *path == "" {
		return errors.New("-config is required")
	}
	c, key, e := load(*path)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	tr, e := yandex.NewVolgaV6Experimental(yandex.VolgaV6ExperimentalOptions{Documents: c.Documents, CookieStore: c.CookieStore, BrowserProfile: c.BrowserProfile, PostsPerSecond: c.PostsPerSecond, PerLaneBudget: c.PerLaneBudget, SendWorkers: c.SendWorkers})
	if e != nil {
		return e
	}
	defer tr.Stop()
	ep := tunnel.NewEndpoint(c.Role == "client", key, tr.SendContext)
	ep.Idle = time.Duration(c.IdleSeconds) * time.Second
	flush := time.Duration(0)
	if c.FlushMillis < 0 {
		flush = -1
	} else if c.FlushMillis > 0 {
		flush = time.Duration(c.FlushMillis) * time.Millisecond
	}
	ep.Opts = recordconn.Options{Chunk: c.RecordChunkBytes, Window: c.RecordWindowBytes, FlushDelay: flush}
	ep.Allowed = make(map[string]bool)
	for _, s := range c.AllowedTargets {
		ep.Allowed[s] = true
	}
	ep.Event = func(s string) { event(s, nil) }
	tr.Receive(ep.Receive)
	if e = tr.Start(); e != nil {
		return e
	}
	postRate := c.PostsPerSecond
	if postRate == 0 {
		postRate = 360
	}
	aggregate := postRate
	if c.PerLaneBudget {
		aggregate = postRate * float64(len(c.Documents))
	}
	effChunk := c.RecordChunkBytes
	if effChunk == 0 {
		effChunk = recordconn.Chunk
	}
	effWindow := c.RecordWindowBytes
	if effWindow == 0 {
		effWindow = recordconn.Window
	}
	effWorkers := c.SendWorkers
	if effWorkers == 0 {
		effWorkers = 64
	}
	effFlushMillis := c.FlushMillis
	if effFlushMillis == 0 {
		effFlushMillis = int(recordconn.FlushDelay / time.Millisecond)
	} else if effFlushMillis < 0 {
		effFlushMillis = 0
	}
	event("carrier_started", map[string]any{"role": c.Role, "version": version, "protocol": c.Protocol,
		"lanes": len(c.Documents), "posts_per_second": postRate, "per_lane_budget": c.PerLaneBudget, "aggregate_posts_per_second": aggregate,
		"record_chunk_bytes": effChunk, "record_window_bytes": effWindow, "flush_millis": effFlushMillis, "send_workers": effWorkers})
	done := make(chan error, 2)
	go func() { done <- ep.Run(ctx) }()
	workers := 1
	if c.Role == "client" {
		ln, e := net.Listen("tcp", c.Listen)
		if e != nil {
			cancel()
			<-done
			return errors.New("cannot open SOCKS listener")
		}
		defer ln.Close()
		workers++
		go func() { done <- tunnel.ServeSOCKS(ctx, ln, ep.Open, ep.Idle) }()
		event("socks_listening", ln.Addr().String())
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	var result error
	running := true
	for running {
		select {
		case <-ctx.Done():
			running = false
		case result = <-done:
			workers--
			running = false
		case now := <-tick.C:
			s := tr.Snapshot(now)
			event("status", map[string]any{"stream": ep.Stats(), "post_failures": s.Carrier.ActiveHealth.PostFailures, "http_statuses": s.Carrier.ActiveHealth.HTTPStatuses, "repairs": s.RepairsSent, "ws_reconnects": s.Carrier.ActiveHealth.WSReconnects, "handoffs": s.Carrier.Handoffs})
		}
	}
	cancel()
	for i := 0; i < workers; i++ {
		<-done
	}
	if errors.Is(result, context.Canceled) || errors.Is(result, net.ErrClosed) {
		return nil
	}
	return result
}
func main() {
	if e := run(); e != nil {
		event("stopped", e.Error())
		os.Exit(1)
	}
}

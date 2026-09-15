#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# Applied AFTER:
#   V1..V6 central batcher transforms
#   volga_benchmark_harness.py
#   volga_body_fragment_v1.py
#   volga_adaptive_fragment_v2.py
#
# Goal: keep HTTP/2 enabled while opening multiple independent HTTP transports.
# Each transport owns its own HTTP/2 connection pool. Requests are distributed
# round-robin across shards, avoiding a single multiplexed connection becoming
# the only flow-control / scheduling point under high relay concurrency.

# ---------------------------------------------------------------------------
# transport.TransportConfig: runtime shard count.
# ---------------------------------------------------------------------------
p = Path("transport/transport.go")
s = p.read_text()
s = replace_once(
    s,
    "\tVolgaHTTPBodyLimit     int\n\tVolgaTelemetry         bool\n",
    "\tVolgaHTTPBodyLimit     int\n"
    "\tVolgaHTTPShards        int\n"
    "\tVolgaTelemetry         bool\n",
    "TransportConfig HTTP shards",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# main.go: expose --volga-http-shards=1|2|4|8.
# ---------------------------------------------------------------------------
p = Path("main.go")
s = p.read_text()
s = replace_once(
    s,
    "\tvolgaWorkers := flag.Int(\"volga-workers\", 64, \"Volga HTTP relay workers\")\n",
    "\tvolgaWorkers := flag.Int(\"volga-workers\", 64, \"Volga HTTP relay workers\")\n"
    "\tvolgaHTTPShards := flag.Int(\"volga-http-shards\", 1, \"Independent Volga HTTP/2 transport shards: 1,2,4,8\")\n",
    "main HTTP shard flag",
)
s = replace_once(
    s,
    "\tif *volgaWorkers < 1 || *volgaWorkers > 512 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-workers: %d\", *volgaWorkers)\n"
    "\t}\n",
    "\tif *volgaWorkers < 1 || *volgaWorkers > 512 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-workers: %d\", *volgaWorkers)\n"
    "\t}\n"
    "\tif *volgaHTTPShards != 1 && *volgaHTTPShards != 2 && *volgaHTTPShards != 4 && *volgaHTTPShards != 8 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-http-shards: %d (allowed: 1,2,4,8)\", *volgaHTTPShards)\n"
    "\t}\n",
    "main HTTP shard validation",
)
s = replace_once(
    s,
    "\tconfig.VolgaWorkerCount = *volgaWorkers\n",
    "\tconfig.VolgaWorkerCount = *volgaWorkers\n"
    "\tconfig.VolgaHTTPShards = *volgaHTTPShards\n",
    "main HTTP shard config assignment",
)
s = replace_once(
    s,
    "\t\tlog.Printf(\"Volga benchmark: workers=%d queue=%d batch_packets=%d batch_bytes=%d http_body_limit=%d timeout=%dus normalize_mtu=%d telemetry=%t\",\n"
    "\t\t\t*volgaWorkers, *volgaQueueSize, *volgaBatchPackets, *volgaBatchBytes, *volgaHTTPBodyLimit, *volgaBatchTimeoutUs, *volgaNormalizeMTU, *volgaTelemetry)\n",
    "\t\tlog.Printf(\"Volga benchmark: workers=%d http_shards=%d queue=%d batch_packets=%d batch_bytes=%d http_body_limit=%d timeout=%dus normalize_mtu=%d telemetry=%t\",\n"
    "\t\t\t*volgaWorkers, *volgaHTTPShards, *volgaQueueSize, *volgaBatchPackets, *volgaBatchBytes, *volgaHTTPBodyLimit, *volgaBatchTimeoutUs, *volgaNormalizeMTU, *volgaTelemetry)\n",
    "main HTTP shard benchmark log",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# vyandex.go: N independent HTTP transports, round-robin request placement.
# ---------------------------------------------------------------------------
p = Path("transport/yandex/vyandex.go")
s = p.read_text()

s = replace_once(
    s,
    "\tWorkerCount int\n\tQueueSize   int\n",
    "\tWorkerCount int\n\tHTTPShards  int\n\tQueueSize   int\n",
    "VolgaConfig HTTP shards",
)
s = replace_once(
    s,
    "\t\tWorkerCount: 64,\n\t\tQueueSize:   1000000,\n",
    "\t\tWorkerCount: 64,\n\t\tHTTPShards:  1,\n\t\tQueueSize:   1000000,\n",
    "VolgaConfig HTTP shard default",
)
s = replace_once(
    s,
    "\tif cfg.VolgaWorkerCount > 0 {\n"
    "\t\tvolgaCfg.WorkerCount = cfg.VolgaWorkerCount\n"
    "\t}\n",
    "\tif cfg.VolgaWorkerCount > 0 {\n"
    "\t\tvolgaCfg.WorkerCount = cfg.VolgaWorkerCount\n"
    "\t}\n"
    "\tif cfg.VolgaHTTPShards > 0 {\n"
    "\t\tvolgaCfg.HTTPShards = cfg.VolgaHTTPShards\n"
    "\t}\n",
    "Volga runtime HTTP shard override",
)

s = replace_once(
    s,
    "\thttpClient  *http.Client\n",
    "\thttpClients  []*http.Client\n\thttpShardSeq atomic.Uint64\n",
    "relay HTTP client shards field",
)

# Replace constructor as one unit so each shard gets a distinct Transport.
start = s.index("func newRelayClient(auth *volgaAuth, cfg VolgaConfig, stats *VolgaStats) *relayClient {")
end = s.index("func (r *relayClient) Start() {")
constructor = r'''func newRelayClient(auth *volgaAuth, cfg VolgaConfig, stats *VolgaStats) *relayClient {
	shardCount := cfg.HTTPShards
	if shardCount < 1 {
		shardCount = 1
	}

	httpClients := make([]*http.Client, shardCount)
	for i := 0; i < shardCount; i++ {
		tr := &http.Transport{
			MaxIdleConns:        cfg.MaxIdleConns,
			MaxIdleConnsPerHost: cfg.MaxIdleConnsPerHost,
			IdleConnTimeout:     cfg.IdleConnTimeout,
			DisableCompression:  true,
			ForceAttemptHTTP2:   true,
		}
		httpClients[i] = &http.Client{
			Transport: tr,
			Timeout:   cfg.RelayTimeout,
			Jar:       auth.Session.Jar,
		}
	}

	ctx, cancel := context.WithCancel(context.Background())

	batchDivisor := cfg.BatchSize
	if batchDivisor < 1 {
		batchDivisor = 1
	}
	batchQueueSize := cfg.QueueSize / batchDivisor
	if batchQueueSize < 1 {
		batchQueueSize = 1
	}

	return &relayClient{
		auth:        auth,
		config:      cfg,
		stats:       stats,
		httpClients: httpClients,
		workers:     cfg.WorkerCount,
		packetQueue: make(chan []byte, cfg.QueueSize),
		batchQueue:  make(chan [][]byte, batchQueueSize),
		ctx:         ctx,
		cancel:      cancel,
	}
}

'''
s = s[:start] + constructor + s[end:]

s = replace_once(
    s,
    "\tutils.Debugf(\"[VOLGA] relay pool started: central batcher -> %d HTTP workers, batch=%d timeout=%v\",\n"
    "\t\tr.workers, r.config.BatchSize, r.config.BatchTimeout)\n",
    "\tutils.Debugf(\"[VOLGA] relay pool started: central batcher -> %d HTTP workers, shards=%d, batch=%d timeout=%v\",\n"
    "\t\tr.workers, len(r.httpClients), r.config.BatchSize, r.config.BatchTimeout)\n",
    "relay shard startup log",
)

s = replace_once(
    s,
    "\thttpStart := time.Now()\n\tresp, err := r.httpClient.Do(req)\n",
    "\tshard := int((r.httpShardSeq.Add(1) - 1) % uint64(len(r.httpClients)))\n"
    "\thttpStart := time.Now()\n"
    "\tresp, err := r.httpClients[shard].Do(req)\n",
    "round-robin HTTP shard request",
)

p.write_text(s)

# ---------------------------------------------------------------------------
# Unit test: constructor really creates distinct transports for each shard.
# ---------------------------------------------------------------------------
Path("transport/yandex/volga_http_shards_test.go").write_text(r'''package yandex

import (
	"net/http"
	"net/http/cookiejar"
	"testing"
)

func TestVolgaHTTPShardsUseDistinctTransports(t *testing.T) {
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := &volgaAuth{Session: &http.Client{Jar: jar}}
	cfg := DefaultVolgaConfig()
	cfg.HTTPShards = 4
	cfg.QueueSize = 1000
	cfg.BatchSize = 20

	r := newRelayClient(auth, cfg, &VolgaStats{})
	if got := len(r.httpClients); got != 4 {
		t.Fatalf("HTTP shard count = %d, want 4", got)
	}

	seen := map[http.RoundTripper]bool{}
	for i, c := range r.httpClients {
		if c == nil || c.Transport == nil {
			t.Fatalf("shard %d has nil client/transport", i)
		}
		if seen[c.Transport] {
			t.Fatalf("shard %d reuses another shard transport", i)
		}
		seen[c.Transport] = true
	}
}
''')

print("Volga HTTP/2 sharding v3 transform applied")

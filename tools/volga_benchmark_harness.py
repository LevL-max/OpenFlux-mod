#!/usr/bin/env python3
from pathlib import Path


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


# This transformer is applied AFTER the proven V1..V6 transforms.
# It keeps the V6 operating point as the default while making the important
# parameters runtime-tunable from one benchmark binary.

# ---------------------------------------------------------------------------
# transport.TransportConfig: carry Volga-specific runtime knobs from main.
# ---------------------------------------------------------------------------
p = Path("transport/transport.go")
s = p.read_text()
s = replace_once(
    s,
    "\tKeepAliveInterval    time.Duration\n",
    "\tKeepAliveInterval    time.Duration\n\n"
    "\t// Volga benchmark knobs. Zero values preserve transport defaults.\n"
    "\tVolgaWorkerCount       int\n"
    "\tVolgaQueueSize         int\n"
    "\tVolgaBatchSize         int\n"
    "\tVolgaBatchTimeout      time.Duration\n"
    "\tVolgaBatchMaxBytes     int\n"
    "\tVolgaTelemetry         bool\n",
    "TransportConfig Volga benchmark fields",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# main.go: command-line knobs and packet-normalizer wrapper.
# ---------------------------------------------------------------------------
p = Path("main.go")
s = p.read_text()
s = replace_once(
    s,
    "\tbatchPackets := flag.Int(\"batch-packets\", 1, \"IP packets per Yandex transport batch; 1 disables batching\")\n"
    "\tbatchDelayUs := flag.Int(\"batch-delay-us\", 1000, \"Maximum batch flush delay in microseconds\")\n",
    "\tbatchPackets := flag.Int(\"batch-packets\", 1, \"IP packets per Yandex transport batch; 1 disables batching\")\n"
    "\tbatchDelayUs := flag.Int(\"batch-delay-us\", 1000, \"Maximum batch flush delay in microseconds\")\n"
    "\tvolgaWorkers := flag.Int(\"volga-workers\", 64, \"Volga HTTP relay workers\")\n"
    "\tvolgaQueueSize := flag.Int(\"volga-queue-size\", 1000000, \"Volga packet queue capacity\")\n"
    "\tvolgaBatchPackets := flag.Int(\"volga-batch-packets\", 20, \"Maximum packets per Volga internal batch\")\n"
    "\tvolgaBatchBytes := flag.Int(\"volga-batch-bytes\", 5000, \"Maximum framed bytes per Volga internal batch\")\n"
    "\tvolgaBatchTimeoutUs := flag.Int(\"volga-batch-timeout-us\", 2000, \"Volga internal batch flush timeout in microseconds\")\n"
    "\tvolgaNormalizeMTU := flag.Int(\"volga-normalize-mtu\", 1500, \"Split oversized IPv4/TCP packets to this MTU before compression; 0 disables\")\n"
    "\tvolgaTelemetry := flag.Bool(\"volga-telemetry\", true, \"Enable aggregate Volga benchmark telemetry\")\n",
    "main benchmark flags",
)
s = replace_once(
    s,
    "\tif *batchDelayUs < 50 || *batchDelayUs > 100000 {\n"
    "\t\tlog.Fatalf(\"Invalid --batch-delay-us: %d\", *batchDelayUs)\n"
    "\t}\n"
    "\tvar trans transport.Transport\n",
    "\tif *batchDelayUs < 50 || *batchDelayUs > 100000 {\n"
    "\t\tlog.Fatalf(\"Invalid --batch-delay-us: %d\", *batchDelayUs)\n"
    "\t}\n"
    "\tif *volgaWorkers < 1 || *volgaWorkers > 512 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-workers: %d\", *volgaWorkers)\n"
    "\t}\n"
    "\tif *volgaQueueSize < 1024 || *volgaQueueSize > 2000000 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-queue-size: %d\", *volgaQueueSize)\n"
    "\t}\n"
    "\tif *volgaBatchPackets < 1 || *volgaBatchPackets > 64 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-batch-packets: %d\", *volgaBatchPackets)\n"
    "\t}\n"
    "\tif *volgaBatchBytes < 512 || *volgaBatchBytes > 16000 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-batch-bytes: %d\", *volgaBatchBytes)\n"
    "\t}\n"
    "\tif *volgaBatchTimeoutUs < 100 || *volgaBatchTimeoutUs > 100000 {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-batch-timeout-us: %d\", *volgaBatchTimeoutUs)\n"
    "\t}\n"
    "\tif *volgaNormalizeMTU != 0 && (*volgaNormalizeMTU < 576 || *volgaNormalizeMTU > 9000) {\n"
    "\t\tlog.Fatalf(\"Invalid --volga-normalize-mtu: %d\", *volgaNormalizeMTU)\n"
    "\t}\n"
    "\tvar trans transport.Transport\n",
    "main benchmark validation",
)
s = replace_once(
    s,
    "\tconfig.MaxQueueSize = *yandexQueueSize\n",
    "\tconfig.MaxQueueSize = *yandexQueueSize\n"
    "\tconfig.VolgaWorkerCount = *volgaWorkers\n"
    "\tconfig.VolgaQueueSize = *volgaQueueSize\n"
    "\tconfig.VolgaBatchSize = *volgaBatchPackets\n"
    "\tconfig.VolgaBatchMaxBytes = *volgaBatchBytes\n"
    "\tconfig.VolgaBatchTimeout = time.Duration(*volgaBatchTimeoutUs) * time.Microsecond\n"
    "\tconfig.VolgaTelemetry = *volgaTelemetry\n",
    "main benchmark config assignment",
)
s = replace_once(
    s,
    "\tcase \"vyandex\":\n"
    "\t\tvolgaTransport := yandex.NewYandexVolgaTransport(globalDocUrl, config)\n"
    "\t\tif *compressionEnabled {\n"
    "\t\t\ttrans = transport.NewCompressedTransport(volgaTransport)\n"
    "\t\t} else {\n"
    "\t\t\ttrans = volgaTransport\n"
    "\t\t}\n"
    "\t\tlog.Printf(\"Volga transport: upstream defaults (internal batch=20 timeout=2ms)\")\n"
    "\t\tlog.Printf(\"Compression: %t\", *compressionEnabled)\n",
    "\tcase \"vyandex\":\n"
    "\t\tvolgaTransport := yandex.NewYandexVolgaTransport(globalDocUrl, config)\n"
    "\t\tif *compressionEnabled {\n"
    "\t\t\ttrans = transport.NewCompressedTransport(volgaTransport)\n"
    "\t\t} else {\n"
    "\t\t\ttrans = volgaTransport\n"
    "\t\t}\n"
    "\t\tif *volgaNormalizeMTU > 0 {\n"
    "\t\t\ttrans = transport.NewTCPPacketNormalizer(trans, *volgaNormalizeMTU, *volgaTelemetry)\n"
    "\t\t}\n"
    "\t\tlog.Printf(\"Volga benchmark: workers=%d queue=%d batch_packets=%d batch_bytes=%d timeout=%dus normalize_mtu=%d telemetry=%t\",\n"
    "\t\t\t*volgaWorkers, *volgaQueueSize, *volgaBatchPackets, *volgaBatchBytes, *volgaBatchTimeoutUs, *volgaNormalizeMTU, *volgaTelemetry)\n"
    "\t\tlog.Printf(\"Compression: %t\", *compressionEnabled)\n",
    "main vyandex benchmark wrapper",
)
p.write_text(s)

# ---------------------------------------------------------------------------
# vyandex.go: apply runtime overrides and add aggregate benchmark telemetry.
# ---------------------------------------------------------------------------
p = Path("transport/yandex/vyandex.go")
s = p.read_text()
s = replace_once(
    s,
    "\tKeepAliveInterval  time.Duration\n",
    "\tKeepAliveInterval  time.Duration\n\tTelemetry          bool\n",
    "VolgaConfig telemetry field",
)

s = replace_once(
    s,
    "\tMaxHTTP413SinglePacketBytes atomic.Uint64\n\tWSReconnects               atomic.Uint64\n",
    "\tMaxHTTP413SinglePacketBytes atomic.Uint64\n"
    "\tHTTPAttempts                atomic.Uint64\n"
    "\tHTTPDurationMicros          atomic.Uint64\n"
    "\tHTTPMaxDurationMicros       atomic.Uint64\n"
    "\tHTTPBodyBytes               atomic.Uint64\n"
    "\tHTTP401s                    atomic.Uint64\n"
    "\tHTTP504s                    atomic.Uint64\n"
    "\tFrontierAdvances            atomic.Uint64\n"
    "\tSameFrontierSends           atomic.Uint64\n"
    "\tWSReconnects                atomic.Uint64\n",
    "VolgaStats benchmark fields",
)

s = replace_once(
    s,
    "\tmu       sync.Mutex\n\tfrontier string\n",
    "\tmu                         sync.Mutex\n"
    "\tfrontier                   string\n"
    "\tfrontierGeneration         uint64\n"
    "\tlastSendFrontierGeneration uint64\n"
    "\tlastSendFrontierSet        bool\n",
    "relay frontier benchmark fields",
)

s = replace_once(
    s,
    "func NewYandexVolgaTransport(docURL string, cfg transport.TransportConfig) *YandexVolgaTransport {\n"
    "\treturn &YandexVolgaTransport{\n"
    "\t\tBaseTransport: transport.NewBaseTransport(cfg),\n"
    "\t\tdocURL:        docURL,\n"
    "\t\tconfig:        DefaultVolgaConfig(),\n"
    "\t\tstats:         &VolgaStats{},\n"
    "\t\tkeepAliveStop: make(chan struct{}),\n"
    "\t}\n"
    "}\n",
    "func NewYandexVolgaTransport(docURL string, cfg transport.TransportConfig) *YandexVolgaTransport {\n"
    "\tvolgaCfg := DefaultVolgaConfig()\n"
    "\tif cfg.VolgaWorkerCount > 0 {\n"
    "\t\tvolgaCfg.WorkerCount = cfg.VolgaWorkerCount\n"
    "\t}\n"
    "\tif cfg.VolgaQueueSize > 0 {\n"
    "\t\tvolgaCfg.QueueSize = cfg.VolgaQueueSize\n"
    "\t}\n"
    "\tif cfg.VolgaBatchSize > 0 {\n"
    "\t\tvolgaCfg.BatchSize = cfg.VolgaBatchSize\n"
    "\t}\n"
    "\tif cfg.VolgaBatchTimeout > 0 {\n"
    "\t\tvolgaCfg.BatchTimeout = cfg.VolgaBatchTimeout\n"
    "\t}\n"
    "\tif cfg.VolgaBatchMaxBytes > 0 {\n"
    "\t\tvolgaCfg.BatchMaxBytes = cfg.VolgaBatchMaxBytes\n"
    "\t}\n"
    "\tvolgaCfg.Telemetry = cfg.VolgaTelemetry\n\n"
    "\treturn &YandexVolgaTransport{\n"
    "\t\tBaseTransport: transport.NewBaseTransport(cfg),\n"
    "\t\tdocURL:        docURL,\n"
    "\t\tconfig:        volgaCfg,\n"
    "\t\tstats:         &VolgaStats{},\n"
    "\t\tkeepAliveStop: make(chan struct{}),\n"
    "\t}\n"
    "}\n",
    "Volga runtime config override",
)

s = replace_once(
    s,
    "\tgo t.keepAliveLoop()\n\tgo t.statsLoop()\n\tt.SetConnected(true)\n",
    "\tgo t.keepAliveLoop()\n"
    "\tif t.config.Telemetry {\n"
    "\t\tgo t.statsLoop()\n"
    "\t}\n"
    "\tt.SetConnected(true)\n",
    "conditional benchmark stats loop",
)

s = replace_once(
    s,
    "\tresp, err := r.httpClient.Do(req)\n"
    "\tif err != nil {\n"
    "\t\treturn err\n"
    "\t}\n",
    "\thttpStart := time.Now()\n"
    "\tresp, err := r.httpClient.Do(req)\n"
    "\telapsedMicros := uint64(time.Since(httpStart).Microseconds())\n"
    "\tif elapsedMicros == 0 {\n"
    "\t\telapsedMicros = 1\n"
    "\t}\n"
    "\tr.stats.HTTPAttempts.Add(1)\n"
    "\tr.stats.HTTPDurationMicros.Add(elapsedMicros)\n"
    "\tatomicMax(&r.stats.HTTPMaxDurationMicros, elapsedMicros)\n"
    "\tif err != nil {\n"
    "\t\treturn err\n"
    "\t}\n",
    "HTTP latency telemetry",
)

s = replace_once(
    s,
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n",
    "\tif resp.StatusCode == http.StatusUnauthorized {\n"
    "\t\tr.stats.HTTP401s.Add(1)\n"
    "\t}\n"
    "\tif resp.StatusCode == http.StatusGatewayTimeout {\n"
    "\t\tr.stats.HTTP504s.Add(1)\n"
    "\t}\n"
    "\tif resp.StatusCode == http.StatusRequestEntityTooLarge {\n",
    "HTTP status telemetry",
)

s = replace_once(
    s,
    "\tatomicMax(&r.stats.MaxHTTPSuccessBytes, bodyBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessRawBytes, rawBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessPackets, packetCount)\n\n"
    "\tr.stats.PacketsSent.Add(uint64(len(batch)))\n",
    "\tatomicMax(&r.stats.MaxHTTPSuccessBytes, bodyBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessRawBytes, rawBytes)\n"
    "\tatomicMax(&r.stats.MaxSuccessPackets, packetCount)\n"
    "\tr.stats.HTTPBodyBytes.Add(bodyBytes)\n\n"
    "\tr.stats.PacketsSent.Add(uint64(len(batch)))\n",
    "successful HTTP body telemetry",
)

s = replace_once(
    s,
    "func (r *relayClient) SetFrontier(opID string) {\n"
    "\tr.mu.Lock()\n"
    "\tr.frontier = opID\n"
    "\tr.mu.Unlock()\n"
    "}\n\n"
    "func (r *relayClient) getFrontier() []interface{} {\n"
    "\tr.mu.Lock()\n"
    "\tdefer r.mu.Unlock()\n"
    "\tif r.frontier == \"\" {\n"
    "\t\treturn []interface{}{}\n"
    "\t}\n"
    "\treturn []interface{}{r.frontier}\n"
    "}\n",
    "func (r *relayClient) SetFrontier(opID string) {\n"
    "\tr.mu.Lock()\n"
    "\tif opID != \"\" && opID != r.frontier {\n"
    "\t\tr.frontier = opID\n"
    "\t\tr.frontierGeneration++\n"
    "\t\tr.stats.FrontierAdvances.Add(1)\n"
    "\t}\n"
    "\tr.mu.Unlock()\n"
    "}\n\n"
    "func (r *relayClient) getFrontier() []interface{} {\n"
    "\tr.mu.Lock()\n"
    "\tdefer r.mu.Unlock()\n"
    "\tgen := r.frontierGeneration\n"
    "\tif r.lastSendFrontierSet && r.lastSendFrontierGeneration == gen {\n"
    "\t\tr.stats.SameFrontierSends.Add(1)\n"
    "\t}\n"
    "\tr.lastSendFrontierGeneration = gen\n"
    "\tr.lastSendFrontierSet = true\n"
    "\tif r.frontier == \"\" {\n"
    "\t\treturn []interface{}{}\n"
    "\t}\n"
    "\treturn []interface{}{r.frontier}\n"
    "}\n",
    "frontier generation telemetry",
)

s = replace_once(
    s,
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, last413, lastOversize, last413Single, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n",
    "\tvar lastSent, lastBytes, lastHTTP, lastFailed, last413, lastOversize, last413Single, lastRecv, lastRecvBytes, lastBatches, lastBatched uint64\n"
    "\tvar lastAttempts, lastHTTPMicros, lastHTTPBody, last401, last504, lastFrontier, lastSameFrontier uint64\n",
    "benchmark stats last counters",
)

s = replace_once(
    s,
    "\t\t\thttp413Single := t.stats.HTTP413SinglePackets.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "\t\t\thttp413Single := t.stats.HTTP413SinglePackets.Load()\n"
    "\t\t\tattempts := t.stats.HTTPAttempts.Load()\n"
    "\t\t\thttpMicros := t.stats.HTTPDurationMicros.Load()\n"
    "\t\t\thttpBody := t.stats.HTTPBodyBytes.Load()\n"
    "\t\t\thttp401 := t.stats.HTTP401s.Load()\n"
    "\t\t\thttp504 := t.stats.HTTP504s.Load()\n"
    "\t\t\tfrontier := t.stats.FrontierAdvances.Load()\n"
    "\t\t\tsameFrontier := t.stats.SameFrontierSends.Load()\n"
    "\t\t\trecv := t.stats.PacketsRecv.Load()\n",
    "benchmark stats loads",
)

s = replace_once(
    s,
    "\t\t\tutils.Debugf(\"[VOLGA-PKT] over-cap %d/5s cap=%d max-pkt=%d | 413-single %d/5s total=%d range=%d..%d\",\n"
    "\t\t\t\toversize-lastOversize, t.config.BatchMaxBytes, t.stats.MaxPacketBytes.Load(),\n"
    "\t\t\t\thttp413Single-last413Single, http413Single,\n"
    "\t\t\t\tt.stats.MinHTTP413SinglePacketBytes.Load(), t.stats.MaxHTTP413SinglePacketBytes.Load())\n\n"
    "\t\t\tlastSent, lastBytes = sent, bytes\n",
    "\t\t\tutils.Debugf(\"[VOLGA-PKT] over-cap %d/5s cap=%d max-pkt=%d | 413-single %d/5s total=%d range=%d..%d\",\n"
    "\t\t\t\toversize-lastOversize, t.config.BatchMaxBytes, t.stats.MaxPacketBytes.Load(),\n"
    "\t\t\t\thttp413Single-last413Single, http413Single,\n"
    "\t\t\t\tt.stats.MinHTTP413SinglePacketBytes.Load(), t.stats.MaxHTTP413SinglePacketBytes.Load())\n\n"
    "\t\t\tattemptDelta := attempts - lastAttempts\n"
    "\t\t\tsuccessDelta := httpReqs - lastHTTP\n"
    "\t\t\tavgHTTPms := float64(httpMicros-lastHTTPMicros) / float64(maxU64(attemptDelta, 1)) / 1000.0\n"
    "\t\t\tavgBody := (httpBody - lastHTTPBody) / maxU64(successDelta, 1)\n"
    "\t\t\tavgRaw := (bytes - lastBytes) / maxU64(successDelta, 1)\n"
    "\t\t\tutils.Debugf(\"[VOLGA-BENCH] age=%v | attempts %d/5s avg=%.1fms max=%dms | status 401=%d 504=%d | req-avg body=%d raw=%d | frontier +%d same-gen=%d\",\n"
    "\t\t\t\tt.BaseTransport.Stats().Uptime.Round(time.Second), attemptDelta, avgHTTPms, t.stats.HTTPMaxDurationMicros.Load()/1000,\n"
    "\t\t\t\thttp401-last401, http504-last504, avgBody, avgRaw, frontier-lastFrontier, sameFrontier-lastSameFrontier)\n\n"
    "\t\t\tlastAttempts, lastHTTPMicros, lastHTTPBody = attempts, httpMicros, httpBody\n"
    "\t\t\tlast401, last504, lastFrontier, lastSameFrontier = http401, http504, frontier, sameFrontier\n"
    "\t\t\tlastSent, lastBytes = sent, bytes\n",
    "benchmark stats output",
)

p.write_text(s)
print("Volga benchmark harness transform applied")

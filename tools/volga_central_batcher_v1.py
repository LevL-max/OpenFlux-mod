from pathlib import Path

path = Path("transport/yandex/vyandex.go")
s = path.read_text()


def replace_once(text: str, old: str, new: str, label: str) -> str:
    count = text.count(old)
    if count != 1:
        raise SystemExit(f"{label}: expected exactly one match, got {count}")
    return text.replace(old, new, 1)


s = replace_once(
    s,
    "\thttpClient *http.Client\n\tworkers    int\n\tqueue      chan []byte\n\tbatchQueue chan []byte\n",
    "\thttpClient  *http.Client\n\tworkers     int\n\tpacketQueue chan []byte\n\tbatchQueue  chan [][]byte\n",
    "relayClient queues",
)

s = replace_once(
    s,
    "\tctx, cancel := context.WithCancel(context.Background())\n\n\treturn &relayClient{",
    "\tctx, cancel := context.WithCancel(context.Background())\n\n"
    "\tbatchDivisor := cfg.BatchSize\n"
    "\tif batchDivisor < 1 {\n"
    "\t\tbatchDivisor = 1\n"
    "\t}\n"
    "\tbatchQueueSize := cfg.QueueSize / batchDivisor\n"
    "\tif batchQueueSize < 1 {\n"
    "\t\tbatchQueueSize = 1\n"
    "\t}\n\n"
    "\treturn &relayClient{",
    "batch queue sizing",
)

s = replace_once(
    s,
    "\t\tworkers:    cfg.WorkerCount,\n"
    "\t\tqueue:      make(chan []byte, cfg.QueueSize),\n"
    "\t\tbatchQueue: make(chan []byte, cfg.QueueSize),\n"
    "\t\tctx:        ctx,\n"
    "\t\tcancel:     cancel,\n",
    "\t\tworkers:     cfg.WorkerCount,\n"
    "\t\tpacketQueue: make(chan []byte, cfg.QueueSize),\n"
    "\t\tbatchQueue:  make(chan [][]byte, batchQueueSize),\n"
    "\t\tctx:         ctx,\n"
    "\t\tcancel:      cancel,\n",
    "relayClient constructor queues",
)

s = replace_once(
    s,
    "func (r *relayClient) Start() {\n"
    "\tfor i := 0; i < r.workers; i++ {\n"
    "\t\tr.wg.Add(1)\n"
    "\t\tgo r.worker(i)\n"
    "\t}\n"
    "\tutils.Debugf(\"[VOLGA] relay pool started: %d workers, batch=%d timeout=%v\",\n"
    "\t\tr.workers, r.config.BatchSize, r.config.BatchTimeout)\n"
    "}\n\n"
    "func (r *relayClient) Stop() {\n"
    "\tr.cancel()\n"
    "\tclose(r.queue)\n"
    "\tclose(r.batchQueue)\n"
    "\tr.wg.Wait()\n"
    "}\n",
    "func (r *relayClient) Start() {\n"
    "\tr.wg.Add(1)\n"
    "\tgo r.batcher()\n\n"
    "\tfor i := 0; i < r.workers; i++ {\n"
    "\t\tr.wg.Add(1)\n"
    "\t\tgo r.worker(i)\n"
    "\t}\n"
    "\tutils.Debugf(\"[VOLGA] relay pool started: central batcher -> %d HTTP workers, batch=%d timeout=%v\",\n"
    "\t\tr.workers, r.config.BatchSize, r.config.BatchTimeout)\n"
    "}\n\n"
    "func (r *relayClient) Stop() {\n"
    "\tr.cancel()\n"
    "\tr.wg.Wait()\n"
    "}\n",
    "Start/Stop",
)

s = replace_once(
    s,
    "\tselect {\n"
    "\tcase r.batchQueue <- cp:\n"
    "\t\treturn nil\n"
    "\tdefault:\n"
    "\t\tr.stats.QueueDrops.Add(1)\n"
    "\t\treturn fmt.Errorf(\"queue full\")\n"
    "\t}\n"
    "}\n\n"
    "func (r *relayClient) worker(id int) {",
    "\tselect {\n"
    "\tcase <-r.ctx.Done():\n"
    "\t\treturn fmt.Errorf(\"relay stopped\")\n"
    "\tdefault:\n"
    "\t}\n\n"
    "\tselect {\n"
    "\tcase r.packetQueue <- cp:\n"
    "\t\treturn nil\n"
    "\tdefault:\n"
    "\t\tr.stats.QueueDrops.Add(1)\n"
    "\t\treturn fmt.Errorf(\"queue full\")\n"
    "\t}\n"
    "}\n\n"
    "func (r *relayClient) worker(id int) {",
    "Send queue",
)

start = s.index("func (r *relayClient) worker(id int) {")
end = s.index("func (r *relayClient) sendBatch(batch [][]byte) error {")

replacement = r'''func (r *relayClient) batcher() {
	defer r.wg.Done()

	batchCap := r.config.BatchSize
	if batchCap < 1 {
		batchCap = 1
	}

	batch := make([][]byte, 0, batchCap)
	totalBytes := 0
	timer := time.NewTimer(r.config.BatchTimeout)
	if !timer.Stop() {
		<-timer.C
	}
	defer timer.Stop()
	timerArmed := false

	stopTimer := func() {
		if !timerArmed {
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timerArmed = false
	}

	armTimer := func() {
		stopTimer()
		timer.Reset(r.config.BatchTimeout)
		timerArmed = true
	}

	flush := func() bool {
		if len(batch) == 0 {
			stopTimer()
			return true
		}

		stopTimer()
		ready := batch
		batch = make([][]byte, 0, batchCap)
		totalBytes = 0

		select {
		case r.batchQueue <- ready:
			return true
		case <-r.ctx.Done():
			return false
		}
	}

	for {
		var timerC <-chan time.Time
		if timerArmed {
			timerC = timer.C
		}

		select {
		case <-r.ctx.Done():
			return

		case pkt := <-r.packetQueue:
			batch = append(batch, pkt)
			totalBytes += len(pkt)

			if len(batch) >= batchCap || totalBytes >= r.config.BatchMaxBytes {
				if !flush() {
					return
				}
			} else if len(batch) == 1 {
				armTimer()
			}

		case <-timerC:
			timerArmed = false
			if !flush() {
				return
			}
		}
	}
}

func (r *relayClient) worker(id int) {
	defer r.wg.Done()

	for {
		select {
		case <-r.ctx.Done():
			return

		case batch := <-r.batchQueue:
			if len(batch) == 0 {
				continue
			}

			r.stats.WorkerBusy.Add(1)
			err := r.sendBatch(batch)
			if err != nil {
				r.stats.HTTPReqsFailed.Add(1)
				utils.Debugf("[VOLGA] batch send failed: %v", err)
			} else {
				r.stats.HTTPReqsSent.Add(1)
				r.stats.BatchesSent.Add(1)
			}
			r.stats.WorkerBusy.Add(-1)
		}
	}
}

'''

s = s[:start] + replacement + s[end:]

s = replace_once(
    s,
    "\t\t\tutils.Debugf(\"[VOLGA-STATS] send %d pkt/s (%d KB/s) | http %d req/s fail %d | batch %d (avg %.1f pkt) | recv %d pkt/s (%d KB/s) | busy %d/%d\",\n"
    "\t\t\t\t(sent-lastSent)/5, (bytes-lastBytes)/5/1024,\n"
    "\t\t\t\t(httpReqs-lastHTTP)/5, failed-lastFailed,\n"
    "\t\t\t\t(batches-lastBatches)/5,\n"
    "\t\t\t\tfloat64(batched-lastBatched)/float64(maxU64(batches-lastBatches, 1)),\n"
    "\t\t\t\t(recv-lastRecv)/5, (recvBytes-lastRecvBytes)/5/1024,\n"
    "\t\t\t\tt.stats.WorkerBusy.Load(), t.config.WorkerCount)\n",
    "\t\t\tutils.Debugf(\"[VOLGA-STATS] send %d pkt/s (%d KB/s) | http %d req/s fail %d | batch %d (avg %.1f pkt) | recv %d pkt/s (%d KB/s) | busy %d/%d | q %d/%d batchq %d/%d\",\n"
    "\t\t\t\t(sent-lastSent)/5, (bytes-lastBytes)/5/1024,\n"
    "\t\t\t\t(httpReqs-lastHTTP)/5, failed-lastFailed,\n"
    "\t\t\t\t(batches-lastBatches)/5,\n"
    "\t\t\t\tfloat64(batched-lastBatched)/float64(maxU64(batches-lastBatches, 1)),\n"
    "\t\t\t\t(recv-lastRecv)/5, (recvBytes-lastRecvBytes)/5/1024,\n"
    "\t\t\t\tt.stats.WorkerBusy.Load(), t.config.WorkerCount,\n"
    "\t\t\t\tlen(t.relay.packetQueue), cap(t.relay.packetQueue),\n"
    "\t\t\t\tlen(t.relay.batchQueue), cap(t.relay.batchQueue))\n",
    "stats telemetry",
)

path.write_text(s)
print("Volga central batcher v1 transform applied")

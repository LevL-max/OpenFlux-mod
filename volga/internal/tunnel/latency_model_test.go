//go:build volga_lab_model

package tunnel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"openflux-volga-lab/internal/recordconn"
	"universal-bypass-tool/transport/yandex"
)

func TestLatencyModel(t *testing.T) {
	for _, tc := range []struct {
		name      string
		window    uint32
		writeSize int
	}{
		{"direct_32k", 0, 32768}, {"mux512_32k", 512 << 10, 32768}, {"mux1024_32k", 1024 << 10, 32768}, {"mux512_4k", 512 << 10, 4096},
		{"mux2048_32k", 2048 << 10, 32768}, {"mux2048_4k", 2048 << 10, 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a, b, ca, _ := yandex.NewVolgaLabModelPair(ctx, nil)
			if e := a.Start(); e != nil {
				t.Fatal(e)
			}
			if e := b.Start(); e != nil {
				t.Fatal(e)
			}
			key := sha256.Sum256([]byte("local paced comparison"))
			x, e := recordconn.New(ctx, key[:], true, a.SendContext)
			if e != nil {
				t.Fatal(e)
			}
			y, e := recordconn.New(ctx, key[:], false, b.SendContext)
			if e != nil {
				t.Fatal(e)
			}
			a.Receive(x.Receive)
			b.Receive(y.Receive)
			defer func() {
				cancel()
				x.Close()
				y.Close()
				a.Stop()
				b.Stop()
				<-x.Done()
				<-y.Done()
			}()
			if e = x.Handshake(ctx); e != nil {
				t.Fatal(e)
			}
			if e = y.Handshake(ctx); e != nil {
				t.Fatal(e)
			}
			var writer io.Writer = x
			var reader io.Reader = y
			if tc.window != 0 {
				cfg := MuxConfig()
				cfg.MaxStreamWindowSize = tc.window
				mx, e := yamux.Client(x, cfg)
				if e != nil {
					t.Fatal(e)
				}
				defer mx.Close()
				my, e := yamux.Server(y, cfg)
				if e != nil {
					t.Fatal(e)
				}
				defer my.Close()
				xs, e := mx.OpenStream()
				if e != nil {
					t.Fatal(e)
				}
				ys, e := my.AcceptStream()
				if e != nil {
					t.Fatal(e)
				}
				writer, reader = xs, ys
			}
			const size = 4 << 20
			seed := sha256.Sum256([]byte("latency model payload"))
			source := rand.NewChaCha8(seed)
			expected := sha256.New()
			io.CopyN(expected, rand.NewChaCha8(seed), size)
			done := make(chan error, 1)
			h := sha256.New()
			before := ca.Snapshot()
			start := time.Now()
			go func() { _, e := io.CopyN(h, reader, size); done <- e }()
			buf := make([]byte, tc.writeSize)
			for remain := size; remain > 0; {
				n := min(remain, len(buf))
				io.ReadFull(source, buf[:n])
				if e = WriteFull(writer, buf[:n]); e != nil {
					t.Fatal(e)
				}
				remain -= n
			}
			if e = <-done; e != nil {
				t.Fatal(e)
			}
			seconds := time.Since(start).Seconds()
			stats := ca.Snapshot()
			got := hex.EncodeToString(h.Sum(nil))
			if got != hex.EncodeToString(expected.Sum(nil)) {
				t.Fatal("integrity")
			}
			data := map[string]any{"case": tc.name, "bytes": size, "seconds": seconds, "mbps": float64(size) * 8 / seconds / 1e6, "sha256": got,
				"posts": stats.Posts - before.Posts, "batches": stats.UniqueBatches - before.UniqueBatches,
				"mean_batch_bytes":    float64(stats.UniqueBatchBytes-before.UniqueBatchBytes) / float64(stats.UniqueBatches-before.UniqueBatches),
				"mean_record_payload": float64(stats.DataBytes-before.DataBytes) / float64(stats.DataRecords-before.DataRecords),
				"small_records":       stats.SmallDataRecords - before.SmallDataRecords, "repairs": a.Snapshot(time.Now()).RepairsSent}
			p, _ := json.Marshal(data)
			fmt.Println("MODEL", string(p))
		})
	}
}

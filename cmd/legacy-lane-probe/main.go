// legacy-lane-probe compares one and two independent Legacy senders on one
// document. It is a carrier diagnostic, not a TCP tunnel or a deployment tool.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"universal-bypass-tool/transport"
	"universal-bypass-tool/transport/yandex"
)

const payloadSize = 1800

func main() {
	doc := flag.String("url", "", "Existing Legacy document URL")
	cookies := flag.String("cookies", "", "Local cookie store (copied, never printed)")
	role := flag.Int("role", 0, "1=AWS, 2=mini-PC")
	start := flag.Int64("start", 0, "Common UTC Unix start time on both peers")
	seconds := flag.Int("seconds", 45, "Per-phase measurement seconds")
	flag.Parse()
	if *doc == "" || *cookies == "" || (*role != 1 && *role != 2) || *seconds < 10 || *seconds > 120 || *start <= time.Now().Unix() {
		fmt.Fprintln(os.Stderr, "invalid probe configuration")
		os.Exit(2)
	}
	tmp, err := os.MkdirTemp("", "openflux-lane-probe-")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)
	seed, err := os.ReadFile(*cookies)
	if err != nil {
		panic("cannot read cookie store")
	}
	var mu sync.Mutex
	seen := make([]map[uint64]bool, 4)
	for i := range seen {
		seen[i] = make(map[uint64]bool)
	}
	invalid := 0
	transports := make([]*yandex.YandexDocsTransport, 2)
	for i := range transports {
		path := filepath.Join(tmp, fmt.Sprintf("cookies-%d.json", i))
		if err := os.WriteFile(path, seed, 0600); err != nil {
			panic(err)
		}
		cfg := transport.DefaultConfig()
		cfg.MaxQueueSize = 64
		t := yandex.NewYandexDocsTransport(*doc, cfg)
		t.SetCursorSafety(2600, 100*time.Millisecond)
		if err := t.SetCookieStore(path); err != nil {
			panic("cannot load cookie store")
		}
		t.Receive(func(data []byte) {
			if len(data) != payloadSize || !bytes.Equal(data[:8], []byte("OFPROBE1")) || int(data[8]) == *role {
				return
			}
			phase, lane := int(data[9]), int(data[10])
			if phase >= 4 || lane >= 2 || binary.BigEndian.Uint64(data[16:24]) != uint64(*start) {
				return
			}
			sum := sha256.Sum256(data[:payloadSize-32])
			mu.Lock()
			defer mu.Unlock()
			if !bytes.Equal(sum[:], data[payloadSize-32:]) {
				invalid++
				return
			}
			key := uint64(lane)<<32 | uint64(binary.BigEndian.Uint32(data[12:16]))
			seen[phase][key] = true // both receivers see the broadcast; count it once
		})
		transports[i] = t
		if err := t.Start(); err != nil {
			panic(err)
		}
		defer t.Stop()
	}
	for time.Now().Unix() < *start {
		time.Sleep(100 * time.Millisecond)
	}
	for phase := 0; phase < 4; phase++ {
		phaseStart := time.Unix(*start, 0).Add(time.Duration(phase*(*seconds+5)) * time.Second)
		if delay := time.Until(phaseStart); delay > 0 {
			time.Sleep(delay)
		}
		senderRole, lanes := 1, 1+phase%2
		if phase >= 2 {
			senderRole = 2
		}
		accepted, errors := 0, 0
		before := []uint64{transports[0].Stats().Reconnects, transports[1].Stats().Reconnects}
		ticker := time.NewTicker(125 * time.Millisecond) // eight data messages/s/lane
		deadline := phaseStart.Add(time.Duration(*seconds) * time.Second)
		seq := uint32(0)
		for tick := range ticker.C {
			if !tick.Before(deadline) {
				break
			}
			if *role != senderRole {
				continue
			}
			seq++
			for lane := 0; lane < lanes; lane++ {
				data := make([]byte, payloadSize)
				copy(data, "OFPROBE1")
				data[8], data[9], data[10] = byte(*role), byte(phase), byte(lane)
				binary.BigEndian.PutUint32(data[12:16], seq)
				binary.BigEndian.PutUint64(data[16:24], uint64(*start))
				for j := 24; j < payloadSize-32; j++ {
					data[j] = byte(j*31 + int(seq)*17 + lane)
				}
				sum := sha256.Sum256(data[:payloadSize-32])
				copy(data[payloadSize-32:], sum[:])
				if transports[lane].Send(data) == nil {
					accepted++
				} else {
					errors++
				}
			}
		}
		ticker.Stop()
		time.Sleep(5 * time.Second)
		up, reconnects := 0, uint64(0)
		for i, t := range transports {
			if t.IsConnected() {
				up++
			}
			reconnects += t.Stats().Reconnects - before[i]
		}
		mu.Lock()
		received := len(seen[phase])
		json.NewEncoder(os.Stdout).Encode(map[string]interface{}{
			"phase": phase, "role": *role, "sender_role": senderRole, "send_lanes": lanes,
			"seconds": *seconds, "accepted": accepted, "send_errors": errors,
			"unique_received": received, "invalid_hashes": invalid, "lanes_up": up,
			"reconnects": reconnects, "received_mbps": float64(received*payloadSize*8) / float64(*seconds) / 1e6,
			"measurement_start": phaseStart.Unix(), "drain_seconds": 5,
		})
		mu.Unlock()
	}
}

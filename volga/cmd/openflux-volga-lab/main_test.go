package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimePolicyAndLimitConfig(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		allowed    []string
		limit      int
		ok         bool
	}{
		{"public without exceptions", "public", nil, 64, true},
		{"origin exception", "public", []string{"127.0.0.1:18765"}, 32, true},
		{"old allowlist default", "", []string{"origin.example:80"}, 0, true},
		{"missing allowlist", "", nil, 64, false},
		{"invalid policy", "open", nil, 64, false},
		{"hostname exception", "public", []string{"origin.example:80"}, 64, false},
		{"invalid max", "public", nil, 65, false},
		{"negative max", "public", nil, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := config{Protocol: "volga-stream-v1", Role: "server", SharedKey: strings.Repeat("ab", 32), EgressPolicy: tc.mode, AllowedTargets: tc.allowed, MaxStreams: tc.limit,
				PostsPerSecond: 480, RecordWindowBytes: 1048576, RecordChunkBytes: 5600, FlushMillis: 1, SendWorkers: 64}
			b, err := json.Marshal(input)
			if err != nil {
				t.Fatal(err)
			}
			p := filepath.Join(t.TempDir(), "lab.json")
			if err = os.WriteFile(p, b, 0600); err != nil {
				t.Fatal(err)
			}
			got, _, err := load(p)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v error=%v", tc.ok, err)
			}
			if err == nil && (got.MaxStreams < 1 || got.MaxStreams > 64 || got.PostsPerSecond != 480 || got.RecordWindowBytes != 1048576 || got.RecordChunkBytes != 5600 || got.FlushMillis != 1 || got.SendWorkers != 64 || got.PerLaneBudget) {
				t.Fatal("frozen speed profile changed", got)
			}
		})
	}
}

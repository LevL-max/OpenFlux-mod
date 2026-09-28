//go:build !volga

package main

// The Volga runtime uses the V6 carrier files in transport/yandex, which are
// excluded from the Legacy build. Build it with: go build -tags volga ./cmd/openflux-volga-lab
var _ = build_with_tags_volga

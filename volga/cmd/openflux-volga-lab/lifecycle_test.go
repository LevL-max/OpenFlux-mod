package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"universal-bypass-tool/transport/yandex"
)

func TestBlockedStartupWaitsLocallyAndResumesAfterCredentialsChange(t *testing.T) {
	for _, rejected := range []error{yandex.ErrCaptchaRequired, yandex.ErrLoginRequired} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		var generation, attempts atomic.Int32
		blocked := make(chan struct{})
		done := make(chan error, 1)
		go func() {
			done <- supervise(ctx, func() [32]byte { return [32]byte{byte(generation.Load())} }, func(context.Context) error {
				if attempts.Add(1) == 1 {
					close(blocked)
					return rejected
				}
				return nil
			}, time.Millisecond)
		}()
		<-blocked
		time.Sleep(15 * time.Millisecond)
		if attempts.Load() != 1 {
			t.Fatal("blocked startup made more remote attempts")
		}
		generation.Store(1)
		if err := <-done; err != nil || attempts.Load() != 2 {
			t.Fatalf("did not recover: %v, attempts %d", err, attempts.Load())
		}
		cancel()
	}
}

func TestBlockedStartupCancelsAndOtherFailuresReturn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	err := supervise(ctx, func() [32]byte { return [32]byte{} }, func(context.Context) error { cancel(); return yandex.ErrCaptchaRequired }, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	sentinel := errors.New("network")
	if err = supervise(context.Background(), func() [32]byte { return [32]byte{} }, func(context.Context) error { return sentinel }, time.Millisecond); err != sentinel {
		t.Fatal(err)
	}
}

func TestCookiesSavedByEarlierLaneDoNotRetryBlockedLane(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var generation, attempts atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- supervise(ctx, func() [32]byte { return [32]byte{byte(generation.Load())} }, func(context.Context) error {
			attempts.Add(1)
			generation.Add(1)
			return yandex.ErrCaptchaRequired
		}, time.Millisecond)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := <-done; err != nil || attempts.Load() != 1 {
		t.Fatalf("automatic cookie writes retried CAPTCHA: %v, %d", err, attempts.Load())
	}
}

func TestCredentialFingerprintTracksContentsOfBothFiles(t *testing.T) {
	a, b := filepath.Join(t.TempDir(), "cookies"), filepath.Join(t.TempDir(), "browser")
	before := credentialFingerprint(a, b)
	if err := os.WriteFile(a, []byte("cookies"), 0600); err != nil {
		t.Fatal(err)
	}
	after := credentialFingerprint(a, b)
	if before == after {
		t.Fatal("cookie import missed")
	}
	if err := os.WriteFile(b, []byte("profile"), 0600); err != nil {
		t.Fatal(err)
	}
	if after == credentialFingerprint(a, b) {
		t.Fatal("browser profile import missed")
	}
}

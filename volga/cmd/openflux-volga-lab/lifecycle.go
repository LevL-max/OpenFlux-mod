package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"time"

	"universal-bypass-tool/transport/yandex"
)

var errBrowserProfileChanged = errors.New("browser profile changed")

// Fingerprints are private and never emitted to logs. Atomic browser imports
// wake startup without restarting the process or retrying a CAPTCHA remotely.
func credentialFingerprint(paths ...string) [32]byte {
	h := sha256.New()
	for _, path := range paths {
		io.WriteString(h, path)
		h.Write([]byte{0})
		f, err := os.Open(path)
		if err != nil {
			h.Write([]byte("unavailable"))
		} else {
			io.Copy(h, io.LimitReader(f, (1<<20)+1))
			f.Close()
		}
		h.Write([]byte{0})
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func supervise(ctx context.Context, fingerprint func() [32]byte, attempt func(context.Context) error, poll time.Duration) error {
	for ctx.Err() == nil {
		err := attempt(ctx)
		if ctx.Err() != nil {
			return nil
		}
		if errors.Is(err, errBrowserProfileChanged) {
			event("connecting", nil)
			continue
		}
		if !errors.Is(err, yandex.ErrCaptchaRequired) && !errors.Is(err, yandex.ErrLoginRequired) {
			return err
		}
		event("auth_blocked", nil)
		// Successful earlier lanes may have persisted Set-Cookie during Start.
		// Those writes must not trigger a loop against a different blocked lane.
		before := fingerprint()
		tick := time.NewTicker(poll)
		for fingerprint() == before && ctx.Err() == nil {
			select {
			case <-ctx.Done():
			case <-tick.C:
			}
		}
		tick.Stop()
		if ctx.Err() == nil {
			event("connecting", nil)
		}
	}
	return nil
}

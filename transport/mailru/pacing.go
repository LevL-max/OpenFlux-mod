package mailru

import (
	"errors"
	"time"
)

// SetChatPacing must be called before Start. The sole chat writer owns the
// schedule, which survives socket replacement. Heartbeat/auth writes are not
// queued behind it. Bytes count the complete encoded application message.
func (t *MailruDocsTransport) SetChatPacing(interval time.Duration, bytesPerSecond int) error {
	if t.IsRunning() || interval < 0 || interval > time.Second || bytesPerSecond < 0 || bytesPerSecond > 10<<20 || bytesPerSecond > 0 && bytesPerSecond < 4096 {
		return errors.New("invalid Mail.ru chat pacing: interval 0..1000 ms, byte budget 0 or 4096..10485760; configure before start")
	}
	t.sendInterval, t.sendBytesPerSecond = interval, bytesPerSecond
	return nil
}

func (t *MailruDocsTransport) chatSpacing(bytes int) time.Duration {
	spacing := t.sendInterval
	if t.sendBytesPerSecond > 0 {
		spacing = max(spacing, time.Duration(bytes)*time.Second/time.Duration(t.sendBytesPerSecond))
	}
	return spacing
}

func (t *MailruDocsTransport) waitChatSlot(next time.Time) bool {
	if !time.Now().Before(next) {
		return t.IsRunning()
	}
	timer := time.NewTimer(time.Until(next))
	defer timer.Stop()
	select {
	case <-timer.C:
		return t.IsRunning()
	case <-t.done:
		return false
	}
}

package yandex

// diagnosticHead returns the oldest outstanding logical DATA sequence and the
// number of retries already attempted for that head entry. It is intentionally
// read-only and used only by V6 telemetry so live stalls can be distinguished
// from ordinary out-of-order delivery.
func (s *volgaV6ReliableSession) diagnosticHead() (uint64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var headSeq uint64
	headRetries := 0
	for seq, entry := range s.replay {
		if headSeq == 0 || seq < headSeq {
			headSeq = seq
			headRetries = entry.retries
		}
	}
	return headSeq, headRetries
}

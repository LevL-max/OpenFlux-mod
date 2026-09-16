package yandex

import (
	"fmt"
	"strings"
)

// newVolgaV6SingleDocumentCarrierFactory is the only concrete Yandex factory
// used by the first live V6 transport. Progress-based recycle is unilateral:
// the sender may decide to fresh-authorize without the peer recycling at the
// same instant. Reusing the same shared document therefore keeps both peers on
// one collaboration channel while the physical sender authorization changes.
//
// Multi-document rotation requires a separate peer-coordination/listener-pool
// design and is deliberately rejected until that protocol exists.
func newVolgaV6SingleDocumentCarrierFactory(documents []string, cfg volgaV6YandexConfig) volgaV6CarrierFactory {
	urls := append([]string(nil), documents...)
	return func(generation uint64, onFrame func(volgaV6WireFrame)) (volgaV6PhysicalCarrier, error) {
		if len(urls) != 1 {
			return nil, fmt.Errorf("volga v6 live transport requires exactly one Yandex document; got %d (multi-document handoff is not coordinated yet)", len(urls))
		}
		docURL := strings.TrimSpace(urls[0])
		if docURL == "" {
			return nil, fmt.Errorf("volga v6 Yandex document URL is empty")
		}
		return newVolgaV6YandexCarrier(generation, docURL, cfg, onFrame), nil
	}
}

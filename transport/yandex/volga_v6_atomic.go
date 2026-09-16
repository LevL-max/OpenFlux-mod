package yandex

import "sync/atomic"

func atomicMax(dst *atomic.Uint64, value uint64) {
	for {
		current := dst.Load()
		if value <= current {
			return
		}
		if dst.CompareAndSwap(current, value) {
			return
		}
	}
}

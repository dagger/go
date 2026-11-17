package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A module proxy's downloads run at most downloadConcurrency at once, and
// every one runs.
func TestEachBounded(t *testing.T) {
	const n = 200
	var running, peak atomic.Int64
	var mu sync.Mutex
	ran := map[int]bool{}
	eachBounded(n, downloadConcurrency, func(i int) {
		now := running.Add(1)
		for {
			p := peak.Load()
			if now <= p || peak.CompareAndSwap(p, now) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		running.Add(-1)
		mu.Lock()
		ran[i] = true
		mu.Unlock()
	})
	if got := peak.Load(); got > downloadConcurrency || got < downloadConcurrency/2 {
		t.Errorf("%d calls ran at once, want at most %d (and the bound used)", got, downloadConcurrency)
	}
	if len(ran) != n {
		t.Errorf("%d of %d calls ran", len(ran), n)
	}
}

package sfu

import "time"

// layerSelector picks the simulcast layer for one player from its bandwidth estimate. Layers are
// indexed largest first, and the player's preference is a ceiling: index 0 allows the largest.
//
// Google congestion control will not raise its estimate much above 1.5 times what is actually being
// sent, and the SFU sends no padding to probe with. So a player that has dropped to a small layer
// would never see an estimate big enough to climb back. The selector therefore tries the next layer
// up speculatively after a backoff, and keeps it if the estimate follows. A failed attempt doubles
// the backoff.
type layerSelector struct {
	ceiling int
	current int

	low       int // consecutive samples below the current layer's bitrate
	probing   bool
	probedAt  time.Time
	nextProbe time.Time
	backoff   time.Duration
}

const (
	probeBackoffMin = 8 * time.Second
	probeBackoffMax = 64 * time.Second
	probeHold       = 10 * time.Second // a probe that survives this long succeeded
	downSamples     = 2                // consecutive low samples before stepping down
)

func newLayerSelector(ceiling int) *layerSelector {
	return &layerSelector{ceiling: ceiling, current: ceiling, backoff: probeBackoffMin}
}

// clampLayer limits a requested layer index to the layers that exist.
func clampLayer(i, n int) int {
	if n <= 0 || i < 0 {
		return 0
	}
	return min(i, n-1)
}

// next returns the layer to use, given each layer's bitrate (largest first, 0 if unknown) and the
// current estimate in bits per second. estimate <= 0 means there is none: use the ceiling.
func (s *layerSelector) next(now time.Time, rates []int64, estimate int) int {
	n := len(rates)
	s.ceiling = clampLayer(s.ceiling, n)
	if n == 0 || estimate <= 0 {
		s.current = s.ceiling
		return s.current
	}
	s.current = clampLayer(max(s.current, s.ceiling), n)

	// The largest layer the estimate covers. An unmeasured layer is assumed to fit.
	fits := n - 1
	for i := s.ceiling; i < n; i++ {
		if rates[i] <= int64(estimate) {
			fits = i
			break
		}
	}

	switch {
	case fits > s.current: // the estimate no longer covers this layer
		s.low++
		if s.low >= downSamples {
			s.low = 0
			s.current = fits
			if s.probing {
				s.probing = false
				s.backoff = min(s.backoff*2, probeBackoffMax)
			}
			s.nextProbe = now.Add(s.backoff)
		}
	case fits < s.current: // the estimate already covers a larger layer
		s.low = 0
		s.current = fits
	default:
		s.low = 0
		if s.probing && now.Sub(s.probedAt) >= probeHold {
			s.probing, s.backoff, s.nextProbe = false, probeBackoffMin, now.Add(probeBackoffMin)
		}
		if !s.probing && s.current > s.ceiling && !now.Before(s.nextProbe) {
			s.current--
			s.probing, s.probedAt = true, now
		}
	}
	return s.current
}

// Package dispatch implements the loop that drains the queue and feeds
// destination adapters.
package dispatch

import (
	"math/rand"
	"time"
)

// BackoffConfig parameterizes the exponential-with-jitter retry curve.
//
// The mean per-attempt delay is initial * 2^(attempts-1), bounded by
// max. With jitter enabled, the actual delay is uniformly distributed on
// [delay/2, delay]. AC-Q2 requires that empirical means stay within ±15%
// of the documented mean over many trials.
type BackoffConfig struct {
	InitialMS  int
	MaxMS      int
	MaxRetries int
	Jitter     bool
}

// DefaultBackoff returns the §12.4 reference curve.
func DefaultBackoff() BackoffConfig {
	return BackoffConfig{InitialMS: 500, MaxMS: 60_000, MaxRetries: 8, Jitter: true}
}

// Compute returns the next attempt delay for a given attempt count.
// `attempts` is the number of failed attempts so far (0-based).
func Compute(cfg BackoffConfig, attempts int, rng *rand.Rand) time.Duration {
	if attempts < 0 {
		attempts = 0
	}
	delayMS := int64(cfg.InitialMS) << attempts
	if delayMS <= 0 || delayMS > int64(cfg.MaxMS) {
		delayMS = int64(cfg.MaxMS)
	}
	if cfg.Jitter && rng != nil {
		// Uniform on [delay/2, delay].
		half := delayMS / 2
		jitter := half + rng.Int63n(half+1)
		delayMS = jitter
	}
	return time.Duration(delayMS) * time.Millisecond
}

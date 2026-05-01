package dispatch

import (
	"math"
	"math/rand"
	"testing"
)

func TestComputeMonotonicWithoutJitter(t *testing.T) {
	cfg := BackoffConfig{InitialMS: 100, MaxMS: 60000, MaxRetries: 8}
	last := -1
	for i := 0; i < 8; i++ {
		got := int(Compute(cfg, i, nil).Milliseconds())
		if got <= last && got != cfg.MaxMS {
			t.Fatalf("attempt %d: %dms not greater than previous %dms", i, got, last)
		}
		last = got
	}
}

func TestComputeCappedAtMax(t *testing.T) {
	cfg := BackoffConfig{InitialMS: 1000, MaxMS: 4000}
	if got := Compute(cfg, 20, nil); got.Milliseconds() != 4000 {
		t.Fatalf("attempt 20: %dms, want 4000", got.Milliseconds())
	}
}

func TestComputeJitterMeanWithin15Percent(t *testing.T) {
	cfg := BackoffConfig{InitialMS: 100, MaxMS: 60000, Jitter: true}
	rng := rand.New(rand.NewSource(0xC0FFEE))
	for attempt := 0; attempt < 6; attempt++ {
		const trials = 200
		var sum float64
		for i := 0; i < trials; i++ {
			sum += float64(Compute(cfg, attempt, rng).Milliseconds())
		}
		got := sum / trials
		// With jitter on [d/2, d], mean is 3d/4.
		expected := 0.75 * float64(int64(cfg.InitialMS)<<attempt)
		if expected > 0.75*float64(cfg.MaxMS) {
			expected = 0.75 * float64(cfg.MaxMS)
		}
		diff := math.Abs(got-expected) / expected
		if diff > 0.15 {
			t.Errorf("attempt %d: empirical mean %.2f, expected %.2f (diff %.1f%%)", attempt, got, expected, diff*100)
		}
	}
}

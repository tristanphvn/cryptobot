package ingestion

import (
	"testing"
	"time"
)

func TestComputeBackfillN_NoGap(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	last := now.Add(-90 * time.Second) // 1.5 intervals for 1m
	got := ComputeBackfillN(last, now, time.Minute, 24*time.Hour)
	if got != 0 {
		t.Fatalf("expected 0 for sub-2-interval elapsed, got %d", got)
	}
}

func TestComputeBackfillN_ExactBoundary(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	last := now.Add(-2 * time.Minute) // exactly 2 intervals
	got := ComputeBackfillN(last, now, time.Minute, 24*time.Hour)
	if got < 2 {
		t.Fatalf("expected >=2 candles at 2-interval gap, got %d", got)
	}
}

func TestComputeBackfillN_ModerateGap1m(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	last := now.Add(-30 * time.Minute)
	got := ComputeBackfillN(last, now, time.Minute, 24*time.Hour)
	// Expect roughly 30 + 1 buffer
	if got < 30 || got > 32 {
		t.Fatalf("expected ~31 candles for 30m gap on 1m interval, got %d", got)
	}
}

func TestComputeBackfillN_CappedByMaxLookback(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	last := now.Add(-72 * time.Hour) // 3 days
	got := ComputeBackfillN(last, now, time.Minute, 24*time.Hour)
	// 24h / 1m = 1440 but Binance limit is 1000.
	if got != 1000 {
		t.Fatalf("expected 1000 (binance limit) for >24h gap on 1m, got %d", got)
	}
}

func TestComputeBackfillN_CappedByBinanceLimit_1hInterval(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	last := now.Add(-24 * time.Hour)
	// 24h / 1h = 24 candles, well under 1000 cap and at-cap of 24h lookback.
	got := ComputeBackfillN(last, now, time.Hour, 24*time.Hour)
	if got != 25 {
		t.Fatalf("expected 25 for 24h gap on 1h interval, got %d", got)
	}
}

func TestComputeBackfillN_RejectsZeroInterval(t *testing.T) {
	now := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	if got := ComputeBackfillN(now.Add(-time.Hour), now, 0, 24*time.Hour); got != 0 {
		t.Fatalf("zero interval must yield 0, got %d", got)
	}
}

func TestParseInterval_Known(t *testing.T) {
	cases := map[string]time.Duration{
		"1m":  time.Minute,
		"5m":  5 * time.Minute,
		"15m": 15 * time.Minute,
		"1h":  time.Hour,
		"4h":  4 * time.Hour,
		"1d":  24 * time.Hour,
	}
	for in, want := range cases {
		got, err := ParseInterval(in)
		if err != nil {
			t.Errorf("ParseInterval(%q) err=%v", in, err)
			continue
		}
		if got != want {
			t.Errorf("ParseInterval(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestParseInterval_Unknown(t *testing.T) {
	if _, err := ParseInterval("7m"); err == nil {
		t.Fatal("expected error for unsupported interval 7m")
	}
}

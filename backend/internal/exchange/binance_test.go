package exchange

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// makeKlineRow builds one row of Binance's tuple-array wire shape. Only the
// fields the parser reads (open/close time, OHLCV) carry meaningful values;
// the rest are zeroed placeholders to satisfy the 12-element length check.
func makeKlineRow(openTimeMs int64, intervalMs int64, price string) []any {
	return []any{
		openTimeMs,
		price, price, price, price, // OHLC all = price for simplicity
		"1.0",                          // volume
		openTimeMs + intervalMs - 1,    // closeTime
		"0", 0, "0", "0", "0",          // quoteVol, trades, takerBuyVol, ignored, ignored
	}
}

// klineServer is a fake Binance /klines endpoint that paginates from a
// fixed in-memory series. Each call returns up to `pageLimit` rows starting
// from the request's startTime, simulating real venue behaviour.
func klineServer(t *testing.T, intervalMs int64, totalCandles int, pageLimit int) (*httptest.Server, *int) {
	t.Helper()
	calls := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v3/klines", func(w http.ResponseWriter, r *http.Request) {
		calls++
		q := r.URL.Query()
		startMs, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		endMs, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if limit == 0 || limit > pageLimit {
			limit = pageLimit
		}

		rows := []any{}
		// The series starts at openTime=0 and goes for totalCandles buckets.
		for i := 0; i < totalCandles; i++ {
			openMs := int64(i) * intervalMs
			if openMs < startMs {
				continue
			}
			if endMs > 0 && openMs > endMs {
				break
			}
			rows = append(rows, makeKlineRow(openMs, intervalMs, fmt.Sprintf("%d.00", 100+i)))
			if len(rows) >= limit {
				break
			}
		}
		_ = json.NewEncoder(w).Encode(rows)
	})
	return httptest.NewServer(mux), &calls
}

func TestFetchHistoricalCandlesRange_Paginates(t *testing.T) {
	// Force the parser to need 3 pages (1000 + 1000 + 200) of 1h candles.
	const intervalMs int64 = 60 * 60 * 1000
	const total = 2200
	srv, calls := klineServer(t, intervalMs, total, 1000)
	defer srv.Close()

	// Shrink the inter-call sleep so the test runs fast without sacrificing
	// the assertion that we do sleep between calls.
	prevSleep := rangeSleep
	rangeSleep = 1 * time.Millisecond
	defer func() { rangeSleep = prevSleep }()

	start := time.UnixMilli(0)
	end := time.UnixMilli(int64(total) * intervalMs) // exclusive sentinel past last
	got, err := FetchHistoricalCandlesRange(srv.URL, "BTCUSDT", "1h", start, end)
	if err != nil {
		t.Fatalf("FetchHistoricalCandlesRange: %v", err)
	}
	if len(got) != total {
		t.Fatalf("candle count: got %d want %d", len(got), total)
	}
	// 2200 candles at 1000/page = 3 calls (1000, 1000, 200).
	if *calls != 3 {
		t.Errorf("HTTP calls: got %d want 3 (verify cursor advance + early-exit on short page)", *calls)
	}
	// Chronological + no dupes.
	for i := 1; i < len(got); i++ {
		if !got[i].OpenTime.After(got[i-1].OpenTime) {
			t.Fatalf("candles not strictly increasing at i=%d: %s vs %s",
				i, got[i-1].OpenTime, got[i].OpenTime)
		}
	}
	if got[0].Symbol != "BTCUSDT" || got[0].Interval != "1h" {
		t.Errorf("symbol/interval not propagated: %+v", got[0])
	}
}

func TestFetchHistoricalCandlesRange_EmptyResponseStops(t *testing.T) {
	const intervalMs int64 = 60 * 60 * 1000
	srv, calls := klineServer(t, intervalMs, 0, 1000) // no candles in series
	defer srv.Close()

	rangeSleep = 1 * time.Millisecond
	defer func() { rangeSleep = 200 * time.Millisecond }()

	got, err := FetchHistoricalCandlesRange(srv.URL, "BTCUSDT", "1h",
		time.UnixMilli(0), time.UnixMilli(intervalMs*100))
	if err != nil {
		t.Fatalf("FetchHistoricalCandlesRange: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d candles, want 0", len(got))
	}
	if *calls != 1 {
		t.Errorf("want exactly one HTTP call for empty result, got %d", *calls)
	}
}

func TestFetchHistoricalCandlesRange_RejectsBadInterval(t *testing.T) {
	_, err := FetchHistoricalCandlesRange("http://example.invalid", "BTCUSDT", "1M",
		time.UnixMilli(0), time.UnixMilli(1))
	if err == nil || !strings.Contains(err.Error(), "unsupported backfill interval") {
		t.Fatalf("want unsupported-interval error, got %v", err)
	}
}

func TestFetchHistoricalCandlesRange_RejectsInvertedRange(t *testing.T) {
	_, err := FetchHistoricalCandlesRange("http://example.invalid", "BTCUSDT", "1h",
		time.UnixMilli(100), time.UnixMilli(50))
	if err == nil || !strings.Contains(err.Error(), "range invalid") {
		t.Fatalf("want range-invalid error, got %v", err)
	}
}

func TestFetchHistoricalCandlesRange_SleepsBetweenCalls(t *testing.T) {
	const intervalMs int64 = 60 * 60 * 1000
	srv, _ := klineServer(t, intervalMs, 1500, 1000) // forces 2 pages
	defer srv.Close()

	prev := rangeSleep
	rangeSleep = 50 * time.Millisecond
	defer func() { rangeSleep = prev }()

	t0 := time.Now()
	if _, err := FetchHistoricalCandlesRange(srv.URL, "BTCUSDT", "1h",
		time.UnixMilli(0), time.UnixMilli(int64(1500)*intervalMs)); err != nil {
		t.Fatalf("FetchHistoricalCandlesRange: %v", err)
	}
	elapsed := time.Since(t0)
	// Two pages → one inter-call sleep of 50ms. Allow generous slack.
	if elapsed < 40*time.Millisecond {
		t.Errorf("expected ≥ ~50ms elapsed for inter-page sleep, got %s", elapsed)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("elapsed %s suspiciously long for a 2-page fetch", elapsed)
	}
}

func TestFetchHistoricalCandles_SinglePage_UnaffectedByRefactor(t *testing.T) {
	const intervalMs int64 = 60 * 60 * 1000
	srv, _ := klineServer(t, intervalMs, 50, 1000)
	defer srv.Close()

	got, err := FetchHistoricalCandles(srv.URL, "ETHUSDT", "1h", 50)
	if err != nil {
		t.Fatalf("FetchHistoricalCandles: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("got %d candles, want 50", len(got))
	}
}

// Sanity: confirm the test server actually advances by startTime so a buggy
// cursor would be caught (this is what FetchHistoricalCandlesRange relies on).
func TestKlineServer_RespectsStartTime(t *testing.T) {
	const intervalMs int64 = 60 * 60 * 1000
	srv, _ := klineServer(t, intervalMs, 100, 1000)
	defer srv.Close()

	u, _ := url.Parse(srv.URL + "/api/v3/klines")
	q := u.Query()
	q.Set("symbol", "BTCUSDT")
	q.Set("interval", "1h")
	q.Set("startTime", fmt.Sprintf("%d", 50*intervalMs))
	q.Set("endTime", fmt.Sprintf("%d", 99*intervalMs))
	q.Set("limit", "1000")
	u.RawQuery = q.Encode()

	got, err := fetchKlines(httpClient(), u.String(), "BTCUSDT", "1h")
	if err != nil {
		t.Fatalf("fetchKlines: %v", err)
	}
	if len(got) != 50 {
		t.Errorf("got %d candles from start=50, want 50", len(got))
	}
	if got[0].OpenTime.UnixMilli() != 50*intervalMs {
		t.Errorf("first openTime: got %d want %d", got[0].OpenTime.UnixMilli(), 50*intervalMs)
	}
}

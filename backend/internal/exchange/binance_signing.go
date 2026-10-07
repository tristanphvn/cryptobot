package exchange

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/url"
	"strconv"
	"time"
)

// SignQuery builds a Binance signed request payload from params, the
// API secret, and a recvWindow. It appends `timestamp` (ms) and
// `recvWindow`, computes HMAC-SHA256 over the URL-encoded canonical
// form, and appends `&signature=<hex>`.
//
// Standard, not a bandaid: deterministic ordering via url.Values.Encode
// (alphabetical) makes test golden values reproducible. The signature
// covers EVERY param including timestamp and recvWindow.
func SignQuery(params url.Values, secret string, recvWindowMs int64) string {
	if recvWindowMs <= 0 {
		recvWindowMs = 5000
	}
	params.Set("recvWindow", strconv.FormatInt(recvWindowMs, 10))
	params.Set("timestamp", strconv.FormatInt(time.Now().UnixMilli(), 10))
	return signWithPayload(params, secret)
}

// SignQueryWithTimestamp is the test seam — callers in production
// always go through SignQuery (which sets timestamp internally), but
// tests need a deterministic timestamp to compare against a golden
// signature.
func SignQueryWithTimestamp(params url.Values, secret string, recvWindowMs, timestampMs int64) string {
	if recvWindowMs <= 0 {
		recvWindowMs = 5000
	}
	params.Set("recvWindow", strconv.FormatInt(recvWindowMs, 10))
	params.Set("timestamp", strconv.FormatInt(timestampMs, 10))
	return signWithPayload(params, secret)
}

func signWithPayload(params url.Values, secret string) string {
	payload := params.Encode()
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(payload))
	return payload + "&signature=" + hex.EncodeToString(mac.Sum(nil))
}

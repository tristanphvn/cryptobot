package config

import (
	"reflect"
	"testing"
	"time"
)

func TestParseSymbolList(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "BTCUSDT", []string{"BTCUSDT"}},
		{"five spec symbols", "BTCUSDT,ETHUSDT,SOLUSDT,BNBUSDT,XRPUSDT",
			[]string{"BTCUSDT", "ETHUSDT", "SOLUSDT", "BNBUSDT", "XRPUSDT"}},
		{"trims whitespace + uppercases", " btcusdt ,  ethusdt",
			[]string{"BTCUSDT", "ETHUSDT"}},
		{"drops empty entries", "BTCUSDT,,ETHUSDT, ,SOLUSDT",
			[]string{"BTCUSDT", "ETHUSDT", "SOLUSDT"}},
		{"all empty → empty slice", ", ,  ,", []string{}},
		{"raw empty → empty slice", "", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseSymbolList(tt.raw)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("got %v want %v", got, tt.want)
			}
		})
	}
}

func TestParseDuration(t *testing.T) {
	good := []struct {
		raw  string
		want time.Duration
	}{
		{"5m", 5 * time.Minute},
		{"15m", 15 * time.Minute},
		{"1h30m", 90 * time.Minute},
		{"500ms", 500 * time.Millisecond},
	}
	for _, tt := range good {
		t.Run("ok/"+tt.raw, func(t *testing.T) {
			got, err := parseDuration(tt.raw)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %s want %s", got, tt.want)
			}
		})
	}

	bad := []string{"", "   ", "notaduration", "5"}
	for _, b := range bad {
		t.Run("reject/"+b, func(t *testing.T) {
			if _, err := parseDuration(b); err == nil {
				t.Errorf("expected error for %q, got nil", b)
			}
		})
	}
}

func TestGetEnvBool(t *testing.T) {
	const key = "TEST_CRY12_PPROF_BOOL"
	defer t.Setenv(key, "")

	truthy := []string{"1", "true", "TRUE", "True", "yes", "YES", " on "}
	for _, v := range truthy {
		t.Setenv(key, v)
		if got := getEnvBool(key, false); !got {
			t.Errorf("getEnvBool(%q) = false, want true", v)
		}
	}
	falsy := []string{"0", "false", "FALSE", "no", "off"}
	for _, v := range falsy {
		t.Setenv(key, v)
		if got := getEnvBool(key, true); got {
			t.Errorf("getEnvBool(%q) = true, want false", v)
		}
	}
	// Empty / unrecognised → use the fallback.
	t.Setenv(key, "")
	if got := getEnvBool(key, true); !got {
		t.Errorf("empty value should fall back to true, got false")
	}
	t.Setenv(key, "maybe")
	if got := getEnvBool(key, true); !got {
		t.Errorf("unrecognised value should fall back to true, got false")
	}
}

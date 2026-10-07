package indicators

// MACDResult holds one data point of MACD output.
type MACDResult struct {
	MACDLine   float64
	SignalLine float64
	Histogram  float64
}

// MACD calculates the Moving Average Convergence Divergence.
// Standard parameters: fast=12, slow=26, signal=9.
// Returns a slice aligned to the end of `closes`.
func MACD(closes []float64, fast, slow, signal int) []MACDResult {
	if len(closes) < slow {
		return nil
	}

	emaFast := ema(closes, fast)
	emaSlow := ema(closes, slow)

	// Align: both EMA slices have different lengths; align from the end.
	offset := len(emaFast) - len(emaSlow)
	macdLine := make([]float64, len(emaSlow))
	for i := range macdLine {
		macdLine[i] = emaFast[i+offset] - emaSlow[i]
	}

	// Signal line = EMA of MACD line
	signalLine := ema(macdLine, signal)

	// Align MACD to signal
	off := len(macdLine) - len(signalLine)
	results := make([]MACDResult, len(signalLine))
	for i := range results {
		m := macdLine[i+off]
		s := signalLine[i]
		results[i] = MACDResult{
			MACDLine:   m,
			SignalLine: s,
			Histogram:  m - s,
		}
	}

	return results
}

// ema computes the Exponential Moving Average.
func ema(data []float64, period int) []float64 {
	if len(data) < period {
		return nil
	}

	k := 2.0 / float64(period+1)
	result := make([]float64, 0, len(data)-period+1)

	// Seed: SMA of first `period` values.
	var sum float64
	for i := 0; i < period; i++ {
		sum += data[i]
	}
	prev := sum / float64(period)
	result = append(result, prev)

	for i := period; i < len(data); i++ {
		prev = (data[i]-prev)*k + prev
		result = append(result, prev)
	}

	return result
}

package indicators

// RSI calculates the Relative Strength Index over `period` closing prices.
// Returns a slice of RSI values aligned to the end of `closes`
// (length = len(closes) - period).
func RSI(closes []float64, period int) []float64 {
	if len(closes) <= period {
		return nil
	}

	rsi := make([]float64, 0, len(closes)-period)

	// Seed: average gain / loss over the first `period` candles.
	var gainSum, lossSum float64
	for i := 1; i <= period; i++ {
		delta := closes[i] - closes[i-1]
		if delta >= 0 {
			gainSum += delta
		} else {
			lossSum -= delta // make positive
		}
	}
	avgGain := gainSum / float64(period)
	avgLoss := lossSum / float64(period)

	rsi = append(rsi, rsiValue(avgGain, avgLoss))

	// Smoothed (Wilder) moving average for remaining bars.
	for i := period + 1; i < len(closes); i++ {
		delta := closes[i] - closes[i-1]
		var gain, loss float64
		if delta >= 0 {
			gain = delta
		} else {
			loss = -delta
		}
		avgGain = (avgGain*float64(period-1) + gain) / float64(period)
		avgLoss = (avgLoss*float64(period-1) + loss) / float64(period)
		rsi = append(rsi, rsiValue(avgGain, avgLoss))
	}

	return rsi
}

func rsiValue(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - (100 / (1 + rs))
}

package model

import (
	"math"
	"strings"
	"time"
	"trader-mux/exchange"
)

func (t *trader) updatePair(pair *exchange.Pair) {
	if pair == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	t.Price = pair.Price
	t.MarkPrice = pair.MarkPrice
	t.openInterest = pair.OpenInterest
	t.fundingRate = pair.FundingRate
}

func (t *trader) updatePrices(prices *[]exchange.Price) {
	if prices == nil || len(*prices) < 2 {
		return
	}

	bidPrice := (*prices)[0]
	askPrice := (*prices)[1]
	bid := bidPrice.Price
	ask := askPrice.Price
	if bid <= 0 || ask <= 0 || ask < bid {
		return
	}

	bidSize := bidPrice.Size
	askSize := askPrice.Size

	asksVol, bidsVol, volPct := t.calculateOBVolPct(ORDERBOOK_LEVEL)
	if asksVol == 0 && bidsVol == 0 {
		// Fallback when orderbook is unavailable: approximate volumes
		// from top-of-book price tick sizes.
		asksVol = askSize * ask
		bidsVol = bidSize * bid
		volPct = ((bidsVol - asksVol) / (bidsVol + asksVol)) * 100
	}

	t.Lock()
	defer t.Unlock()

	t.bestBid = bid
	t.bestAsk = ask
	t.asksVol = asksVol
	t.bidsVol = bidsVol
	t.volumePct = volPct
	mid := (bid + ask) / 2
	spreadPct := ((ask - bid) / mid) * 100
	priceTime := bidPrice.Time
	if priceTime.IsZero() {
		priceTime = askPrice.Time
	}
	if priceTime.IsZero() {
		priceTime = time.Now()
	}
	bidPrice.Time = priceTime
	askPrice.Time = priceTime
	insertWithLimitInPlace(&t.Prices, [2]exchange.Price{bidPrice, askPrice}, ARRAY_SIZE)
	insertWithLimitInPlace(&t.spreadPct, spreadPct, ARRAY_SIZE)
	rawVolatilityPct := calculateVolatilityPct(t.Prices, VOLATILITY_WINDOW)
	t.volatilityPct = EMA(rawVolatilityPct, t.volatilityPct, EMA_ALPHA)
	t.volatilityRegime = volatilityRegime(t.volatilityPct)
	rawLatencyBufferPct := calculateLatencyBufferPct(t.parent.Exchange.GetLatency(), spreadPct)
	t.latencyBufferPct = EMA(rawLatencyBufferPct, t.latencyBufferPct, EMA_ALPHA)
	t.updateSpreadAvg()
}

func calculateLatencyBufferPct(latencyMs int64, spreadPct float64) float64 {
	latencySeconds := float64(latencyMs) / 2 / 1000
	fixedBuffer := math.Max(latencySeconds*0.1, LATENCY_MIN_BUFFER_PCT)
	adaptiveBuffer := spreadPct * latencySeconds * 0.5
	return math.Max(fixedBuffer+adaptiveBuffer, LATENCY_MIN_BUFFER_PCT)
}

// calculateVolatilityPct returns realized mid-price volatility over a time
// window as a percent. The newest price pair is expected at index 0.
func calculateVolatilityPct(prices [][2]exchange.Price, window time.Duration) float64 {
	if window <= 0 || len(prices) < 2 {
		return 0
	}
	latestTime := pricePairTime(prices[0])
	if latestTime.IsZero() {
		return 0
	}
	cutoff := latestTime.Add(-window)

	var sumSq float64
	var count int
	for i := 1; i < len(prices); i++ {
		newerTime := pricePairTime(prices[i-1])
		if newerTime.IsZero() {
			continue
		}
		if newerTime.Before(cutoff) {
			break
		}

		newerMid := midPrice(prices[i-1])
		olderMid := midPrice(prices[i])
		if newerMid <= 0 || olderMid <= 0 {
			continue
		}

		logReturn := math.Log(newerMid / olderMid)
		sumSq += logReturn * logReturn
		count++
	}

	if count == 0 {
		return 0
	}
	return math.Sqrt(sumSq) * 100
}

func volatilityRegime(volatilityPct float64) string {
	switch {
	case volatilityPct >= VOLATILITY_EXTREME_PCT:
		return "extreme"
	case volatilityPct >= VOLATILITY_HIGH_PCT:
		return "high"
	case volatilityPct >= VOLATILITY_LOW_PCT:
		return "normal"
	default:
		return "low"
	}
}

func midPrice(pricePair [2]exchange.Price) float64 {
	bid := pricePair[0].Price
	ask := pricePair[1].Price
	if bid <= 0 || ask <= 0 || ask < bid {
		return 0
	}
	return (bid + ask) / 2
}

func pricePairTime(pricePair [2]exchange.Price) time.Time {
	if !pricePair[0].Time.IsZero() {
		return pricePair[0].Time
	}
	return pricePair[1].Time
}

// calculateOBVolPct sums top-N bid/ask sizes from the subscribed orderbook,
// then returns notional-ish volumes (depth × top-of-book price) and imbalance
// in percent: positive means more bid size, negative more ask size.
func (t *trader) calculateOBVolPct(levels int) (float64, float64, float64) {
	if t.parent == nil || t.parent.Exchange == nil || levels <= 0 {
		return 0, 0, 0
	}
	ob, err := t.parent.Exchange.GetOrderbook(t.Pair)
	if err != nil {
		return 0, 0, 0
	}
	if len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0, 0, 0
	}

	var bidDepth, askDepth float64
	for i := 0; i < levels && i < len(ob.Bids); i++ {
		bidDepth += ob.Bids[i].Size
	}
	for i := 0; i < levels && i < len(ob.Asks); i++ {
		askDepth += ob.Asks[i].Size
	}
	totalDepth := bidDepth + askDepth
	if totalDepth == 0 {
		return 0, 0, 0
	}

	asksVol := askDepth * ob.Asks[0].Price
	bidsVol := bidDepth * ob.Bids[0].Price
	imbalance := (bidDepth - askDepth) / totalDepth
	return asksVol, bidsVol, imbalance * 100.0
}

func (t *trader) updateTrade(trade *exchange.Trade) {
	if trade == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	tradedPrice := 0.0
	if trade.Order != nil && trade.Order.Price > 0 {
		tradedPrice = trade.Order.Price
	}
	if len(trade.Fills) > 0 {
		var weightedFillPrice float64
		var totalSize float64
		for _, fill := range trade.Fills {
			if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
				continue
			}
			totalSize += fill.Size
			weightedFillPrice += fill.Price * fill.Size
		}
		if totalSize > 0 {
			tradedPrice = weightedFillPrice / totalSize
		}
	}
	if tradedPrice > 0 {
		t.lastTradePrice = tradedPrice
	}

	insertWithLimitInPlace(&t.Trades, *trade, ARRAY_SIZE)
	t.tradePerMinute = t.calculateTradesInDuration(time.Minute)
	side := ""
	if trade.Order != nil {
		side = trade.Order.Side
	}
	t.calculateSlippage(trade.Fills, side)
}

// calculateTradesInDuration calculates the number of stored trades inside duration.
// The caller must hold t.Lock or t.RLock.
func (t *trader) calculateTradesInDuration(duration time.Duration) int {
	if len(t.Trades) == 0 {
		return 0
	}

	var mostRecentTime time.Time
	var oldestTime time.Time
	for _, trade := range t.Trades {
		if trade.Order != nil && !trade.Order.Time.IsZero() {
			if mostRecentTime.IsZero() {
				mostRecentTime = trade.Order.Time
			}
			oldestTime = trade.Order.Time
		}
	}
	if mostRecentTime.IsZero() {
		return 0
	}

	cutoffTime := mostRecentTime.Add(-duration)
	count := 0
	for _, trade := range t.Trades {
		if trade.Order != nil && !trade.Order.Time.Before(cutoffTime) {
			count++
		}
	}

	// If the bounded trade buffer doesn't span the full duration, estimate
	// per-minute rate from the observed trade span to avoid undercounting.
	if len(t.Trades) == cap(t.Trades) && !oldestTime.IsZero() && oldestTime.After(cutoffTime) {
		spanSeconds := mostRecentTime.Sub(oldestTime).Seconds()
		if spanSeconds > 0 {
			return int(math.Round(float64(len(t.Trades)) * duration.Seconds() / spanSeconds))
		}
	}

	return count
}

func (t *trader) updateBar(bar *exchange.Bar) {
	if bar == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	previousSMA := t.m1_SMA
	t.updateBars(*bar)

	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.m1_SMA = sma
		if previousSMA > 0 {
			t.m1_SMASlope = ((sma - previousSMA) / previousSMA) * 100
		}
	}
}

func (t *trader) updateBars(bar exchange.Bar) {
	if len(t.Bars) > 0 && t.Bars[0].Time.Equal(bar.Time) {
		t.Bars[0] = bar
		return
	}
	if len(t.Bars) > 0 && bar.Time.Before(t.Bars[0].Time) {
		return
	}

	insertWithLimitInPlace(&t.Bars, bar, ARRAY_SIZE)
}

// calculateSlippage records price impact versus mark price based on trade fills.
// The caller must hold t.Lock.
func (t *trader) calculateSlippage(fills []*exchange.Fill, side string) {
	if len(fills) == 0 || t.MarkPrice <= 0 {
		return
	}

	side = strings.ToLower(side)
	if side != "buy" && side != "sell" {
		return
	}

	var worstFillPrice float64
	for _, fill := range fills {
		if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
			continue
		}
		switch side {
		case "buy":
			if fill.Price > worstFillPrice {
				worstFillPrice = fill.Price
			}
		case "sell":
			if worstFillPrice == 0 || fill.Price < worstFillPrice {
				worstFillPrice = fill.Price
			}
		}
	}

	if worstFillPrice <= 0 {
		return
	}

	var slippagePct float64
	if side == "buy" {
		slippagePct = ((worstFillPrice - t.MarkPrice) / t.MarkPrice) * 100
	} else {
		slippagePct = -((t.MarkPrice - worstFillPrice) / t.MarkPrice) * 100
	}

	if math.Abs(slippagePct) < 0.01 {
		return
	}

	insertWithLimitInPlace(&t.slippagePct, slippagePct, ARRAY_SIZE)
	t.updateSlippageAvg()
}

// updateSlippageAvg recalculates weighted rolling slippage percentage.
// The caller must hold t.Lock.
func (t *trader) updateSlippageAvg() {
	n := len(t.slippagePct)
	if n == 0 {
		t.slippageAvg = 0
		return
	}
	if n > ARRAY_SIZE {
		n = ARRAY_SIZE
	}

	var weightedSum float64
	var totalWeight float64
	for i := 0; i < n; i++ {
		weight := math.Pow(0.9, float64(n-i-1))
		weightedSum += t.slippagePct[i] * weight
		totalWeight += weight
	}
	if totalWeight <= 0 {
		t.slippageAvg = 0
		return
	}
	t.slippageAvg = weightedSum / totalWeight
}

// updateSpreadAvg recalculates the rolling average bid/ask spread.
// The caller must hold t.Lock.
func (t *trader) updateSpreadAvg() {
	if len(t.spreadPct) == 0 {
		t.spreadAvg = 0
		return
	}

	var sum float64
	for _, spread := range t.spreadPct {
		sum += spread
	}
	t.spreadAvg = sum / float64(len(t.spreadPct))
}

func calculateSMA(bars []exchange.Bar, length int) float64 {
	if length <= 0 || len(bars) < length {
		return 0
	}

	var sum float64
	for i := 0; i < length; i++ {
		sum += bars[i].Close
	}
	return sum / float64(length)
}

func EMA(newValue, oldValue, alpha float64) float64 {
	if alpha <= 0 {
		return oldValue
	}
	if alpha >= 1 || oldValue == 0 {
		return newValue
	}
	return alpha*newValue + (1-alpha)*oldValue
}

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

	t.Lock()
	defer t.Unlock()

	t.bestBid = bid
	t.bestAsk = ask
	mid := (bid + ask) / 2
	spreadPct := ((ask - bid) / mid) * 100
	insertWithLimitInPlace(&t.Prices, [2]exchange.Price{bidPrice, askPrice}, ARRAY_SIZE)
	t.volatilityPct = EMA(calculateVolatilityPct(t.Prices, VOLATILITY_WINDOW), t.volatilityPct, VOLATILITY_EMA_ALPHA)
	t.volatilityRegime = volatilityRegime(t.volatilityPct)
	t.latencyBufferPct = EMA(calculateLatencyBufferPct(t.parent.Exchange.GetLatency(), spreadPct), t.latencyBufferPct, LATENCY_EMA_ALPHA)
	t.spreadAvg = EMA(spreadPct, t.spreadAvg, SPREAD_EMA_ALPHA)
	t.spreadRegime = spreadRegime(t.spreadAvg)
}

func calculateLatencyBufferPct(latencyMs int64, spreadPct float64) float64 {
	latencySeconds := float64(latencyMs) / 2 / 1000
	fixedBuffer := math.Max(latencySeconds*0.1, LATENCY_MIN_BUFFER_PCT)
	adaptiveBuffer := spreadPct * latencySeconds * 0.5
	return math.Max(fixedBuffer+adaptiveBuffer, LATENCY_MIN_BUFFER_PCT)
}

func calculateVolatilityPct(prices [][2]exchange.Price, window time.Duration) float64 {
	if len(prices) < 2 || window <= 0 {
		return 0
	}

	latestTime := pricePairTime(prices[0])
	if latestTime.IsZero() {
		return 0
	}
	cutoff := latestTime.Add(-window)

	var sumSq float64
	// We loop back from the newest price (index 0) to the cutoff
	for i := 1; i < len(prices); i++ {
		if pricePairTime(prices[i-1]).Before(cutoff) {
			break
		}

		newerMid := midPrice(prices[i-1])
		olderMid := midPrice(prices[i])

		if newerMid > 0 && olderMid > 0 {
			// Log returns capture the "energy" of each zig and zag
			logReturn := math.Log(newerMid / olderMid)
			sumSq += logReturn * logReturn
		}
	}

	// math.Sqrt(sumSq) aggregates the "energy" of all moves in the window.
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

func spreadRegime(spreadPct float64) string {
	switch {
	case spreadPct >= SPREAD_EXTREME_PCT:
		return "extreme"
	case spreadPct >= SPREAD_HIGH_PCT:
		return "high"
	case spreadPct >= SPREAD_LOW_PCT:
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

func (t *trader) updateVolumes(orderbook *exchange.Orderbook) {
	asksVol, bidsVol, volumePct, bidNearNotional, askNearNotional, vpoc := t.calculateOrderbook(orderbook, ORDERBOOK_LEVEL)

	t.Lock()
	defer t.Unlock()

	strongestNearNotional := math.Max(bidNearNotional, askNearNotional)
	t.nearVolumeAvg = EMA(strongestNearNotional, t.nearVolumeAvg, ORDERBOOK_NEAR_EMA_ALPHA)

	t.asksVol = asksVol
	t.bidsVol = bidsVol
	t.volumePct = volumePct
	t.nearVolumeStrength = calculateNearVolumeStrength(bidNearNotional, askNearNotional, t.nearVolumeAvg)
	t.vpoc = vpoc
}

func (t *trader) calculateOrderbook(ob *exchange.Orderbook, levels int) (float64, float64, float64, float64, float64, float64) {
	if ob == nil || levels <= 0 || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0, 0, 0, 0, 0, 0
	}

	mid := (ob.Bids[0].Price + ob.Asks[0].Price) / 2
	if mid <= 0 {
		return 0, 0, 0, 0, 0, 0
	}

	tickSize := 0.0
	if t.parent != nil && t.parent.Exchange != nil {
		if pair, err := t.parent.Exchange.Pair(t.Pair); err == nil {
			tickSize = pair.Base.TickSize
		}
	}

	// VPOC buckets group nearby price levels. A positive decay factor keeps a
	// decayed profile; zero or lower uses only the current orderbook snapshot.
	if t.vpocProfile.BucketSize <= 0 && tickSize > 0 {
		t.vpocProfile.BucketSize = math.Max(mid*(ORDERBOOK_VPOC_BUCKET_PCT/100), tickSize)
	}
	t.vpocProfile.DecayFactor = ORDERBOOK_VPOC_DECAY_FACTOR

	vpocEnabled := t.vpocProfile.BucketSize > 0
	var vpocBuckets map[int64]float64
	vpocBucketPrice := make(map[int64]exchange.Price)
	if vpocEnabled {
		if t.vpocProfile.DecayFactor > 0 {
			if t.vpocProfile.Buckets == nil {
				t.vpocProfile.Buckets = make(map[int64]float64)
			}
			for idx, volume := range t.vpocProfile.Buckets {
				t.vpocProfile.Buckets[idx] = volume * t.vpocProfile.DecayFactor
				if t.vpocProfile.Buckets[idx] <= 1e-8 {
					delete(t.vpocProfile.Buckets, idx)
				}
			}
			vpocBuckets = t.vpocProfile.Buckets
		} else {
			vpocBuckets = make(map[int64]float64)
		}
	}

	maxDistance := mid * (ORDERBOOK_DEPTH_PCT / 100)
	bidNearMinPrice := ob.Bids[0].Price * (1 - (ORDERBOOK_NEAR_DEPTH_PCT / 100))
	askNearMaxPrice := ob.Asks[0].Price * (1 + (ORDERBOOK_NEAR_DEPTH_PCT / 100))
	var weightedBidNotional, weightedAskNotional float64
	var actualBidNotional, actualAskNotional float64
	var bidNearNotional, askNearNotional float64

	process := func(bookLevels []exchange.Price, isBid bool) {
		for i := 0; i < levels && i < len(bookLevels); i++ {
			level := bookLevels[i]
			dist := math.Abs(level.Price - mid)

			if level.Price <= 0 || level.Size <= 0 || dist > maxDistance {
				if isBid && level.Price < (mid-maxDistance) {
					break
				}
				if !isBid && level.Price > (mid+maxDistance) {
					break
				}
				continue
			}

			weight := 1.0
			if ORDERBOOK_WEIGHT_FACTOR > 0 {
				weight = 1.0 / (1.0 + (dist/mid)*ORDERBOOK_WEIGHT_FACTOR*100)
			}

			// 3. Consistent Notional Imbalance
			notional := level.Price * level.Size
			weightedNotional := notional * weight
			if isBid {
				weightedBidNotional += weightedNotional
				actualBidNotional += notional
				if level.Price >= bidNearMinPrice {
					bidNearNotional += notional
				}
			} else {
				weightedAskNotional += weightedNotional
				actualAskNotional += notional
				if level.Price <= askNearMaxPrice {
					askNearNotional += notional
				}
			}

			if vpocEnabled {
				idx := int64(math.Floor(level.Price / t.vpocProfile.BucketSize))
				vpocBuckets[idx] += notional
				if levelNotional, exists := vpocBucketPrice[idx]; !exists || notional > levelNotional.Size {
					vpocBucketPrice[idx] = exchange.Price{Price: level.Price, Size: notional}
				}
			}
		}
	}

	process(ob.Bids, true)
	process(ob.Asks, false)

	totalWeightedNotional := weightedBidNotional + weightedAskNotional
	imbalance := 0.0
	if totalWeightedNotional > 0 {
		imbalance = ((weightedBidNotional - weightedAskNotional) / totalWeightedNotional) * 100.0
	}

	if !vpocEnabled {
		return actualAskNotional, actualBidNotional, imbalance, bidNearNotional, askNearNotional, 0
	}

	// VPOC is the highest-volume price level inside the single highest-volume bucket.
	var bestIdx int64
	var maxBucketVolume float64
	for idx, volume := range vpocBuckets {
		if volume <= 1e-6 {
			continue
		}
		if volume > maxBucketVolume {
			maxBucketVolume = volume
			bestIdx = idx
		}
	}

	if maxBucketVolume <= 0 {
		return actualAskNotional, actualBidNotional, imbalance, bidNearNotional, askNearNotional, 0
	}

	vpoc := vpocBucketPrice[bestIdx].Price
	if vpoc <= 0 {
		vpoc = (float64(bestIdx) * t.vpocProfile.BucketSize) + (t.vpocProfile.BucketSize / 2)
	}
	return actualAskNotional, actualBidNotional, imbalance, bidNearNotional, askNearNotional, vpoc
}

func calculateNearVolumeStrength(bidNearNotional, askNearNotional, averageNearNotional float64) float64 {
	if averageNearNotional <= 0 {
		return 0
	}
	strongest := math.Max(bidNearNotional, askNearNotional)
	strength := (strongest / averageNearNotional) * 100
	if askNearNotional > bidNearNotional {
		return -strength
	}
	if bidNearNotional > askNearNotional {
		return strength
	}
	return 0
}

type VPOCProfile struct {
	Buckets     map[int64]float64
	BucketSize  float64
	DecayFactor float64
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

func (t *trader) updateBar(bar *exchange.Bar) {
	if bar == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	previousSMA20 := t.m1_SMA20
	previousSMA200 := t.m1_SMA200
	t.updateBars(*bar)

	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.m1_SMA20 = sma
		if previousSMA20 > 0 {
			t.m1_SMA20Slope = ((sma - previousSMA20) / previousSMA20) * 100
		}
	}
	if sma := calculateSMA(t.Bars, 200); sma > 0 {
		t.m1_SMA200 = sma
		if previousSMA200 > 0 {
			t.m1_SMA200Slope = ((sma - previousSMA200) / previousSMA200) * 100
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

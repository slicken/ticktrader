package model

import (
	"context"
	"log"
	"math"
	"strings"
	"sync"
	"time"
	"trader-mux/config"
	"trader-mux/exchange"
	"trader-mux/exchange/lighter"
)

const (
	BARS_LENGTH   = 210
	TRADES_LENGTH = 5000
	ARRAY_LENGTH  = 200
	BAR_INTERVAL  = "1m"
)

// Marketmaker is the main engine that manages exchange connection and global config
type Marketmaker struct {
	Exchange exchange.I
	config   *config.ModelConfig
	traders  map[string]*trader
	process  chan struct{} // Channel semaphore to ensure only one instance runs at a time
}

// trader is a pair-specific tradingg instance with its own world/settings
type trader struct {
	parent         *Marketmaker
	Pair           string
	Bars           []exchange.Bar
	Trades         []exchange.Trade
	Prices         [][2]exchange.Price
	slippagePct    []float64
	slippageAvg    float64
	spreadPct      []float64
	spreadAvg      float64
	MarkPrice      float64
	Price          float64
	bestBid        float64
	bestAsk        float64
	asksVol        float64
	bidsVol        float64
	tradePerMinute int
	lastTradePrice float64
	openInterest   float64
	fundingRate    float64
	SMA20          float64
	SMA20SlopePct  float64
	sync.RWMutex
}

// Initialize creates the main engine and automatically adds pair traders for all pairs
func Initialize(exch exchange.I, cfg *config.ModelConfig) *Marketmaker {
	strat := &Marketmaker{
		Exchange: exch,
		config:   cfg,
		traders:  make(map[string]*trader),
		process:  make(chan struct{}, 1),
	}

	var pairs []string

	for _, pair := range exch.GetPairs() {
		if !pair.Enabled || !pair.IsPerp {
			continue
		}
		pairName := pair.Name

		// check if pairs are specified
		if len(cfg.Pairs) > 0 {
			found := false
			for _, p := range cfg.Pairs {
				if p == pairName {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}

		// check if disabled
		if len(cfg.DisabledPairs) > 0 {
			disabled := false
			for _, p := range cfg.DisabledPairs {
				if p == pairName {
					disabled = true
					break
				}
			}
			if disabled {
				continue
			}
		}

		pairs = append(pairs, pairName)
	}

	for _, pair := range pairs {
		strat.traders[pair] = Newtrader(strat, pair)
	}

	count := 0
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, pair := range pairs {
		wg.Add(1)

		go func(p string) {
			defer wg.Done()

			if err := exch.SubscribePair(p); err != nil {
				log.Printf("Error subscribing to pair %s: %v", p, err)
			} else {
				log.Printf("Subscribed to pair %s", p)
				mu.Lock()
				count++
				mu.Unlock()
			}
			if err := exch.SubscribeTrades(p); err != nil {
				log.Printf("Error subscribing to trades %s: %v", p, err)
			} else {
				log.Printf("Subscribed to trades %s", p)
				mu.Lock()
				count++
				mu.Unlock()
			}
			if err := exch.SubscribeBars(p, BAR_INTERVAL); err != nil {
				log.Printf("Error subscribing to %s bars %s: %v", p, BAR_INTERVAL, err)
			} else {
				log.Printf("Subscribed to %s bars %s", p, BAR_INTERVAL)
				mu.Lock()
				count++
				mu.Unlock()
			}
			if err := exch.SubscribePrices(p); err != nil {
				log.Printf("Error subscribing to prices %s: %v", p, err)
			} else {
				log.Printf("Subscribed to prices %s", p)
				mu.Lock()
				count++
				mu.Unlock()
			}
			if err := strat.getInitialBars(p); err != nil {
				log.Printf("Error loading initial %s bars %s: %v", p, BAR_INTERVAL, err)
			}
			// if err := exch.SubscribeOrderbook(p); err != nil {
			// 	log.Printf("Error subscribing to orderbook %s: %v", p, err)
			// } else {
			// 	log.Printf("Subscribed to orderbook %s", p)
			// 	mu.Lock()
			// 	count++
			// 	mu.Unlock()
			// }
		}(pair)
	}
	wg.Wait()

	log.Printf("Subscribed to %d pairs... (%d subscriptions)\n", len(pairs), count)
	log.Printf("%v\n", pairs)

	return strat
}

// Newtrader creates a new pair-specific trading instance
func Newtrader(parent *Marketmaker, pair string) *trader {
	return &trader{
		parent:         parent,
		Pair:           pair,
		Bars:           make([]exchange.Bar, 0, BARS_LENGTH),
		Trades:         make([]exchange.Trade, 0, TRADES_LENGTH),
		Prices:         make([][2]exchange.Price, 0, ARRAY_LENGTH),
		slippagePct:    make([]float64, 0, ARRAY_LENGTH),
		slippageAvg:    0,
		spreadPct:      make([]float64, 0, ARRAY_LENGTH),
		spreadAvg:      0,
		MarkPrice:      0,
		Price:          0,
		bestBid:        0,
		bestAsk:        0,
		asksVol:        0,
		bidsVol:        0,
		tradePerMinute: 0,
		lastTradePrice: 0,
		openInterest:   0,
		fundingRate:    0,
		SMA20:          0,
		SMA20SlopePct:  0,
	}
}

// Start the market maker and all its pair traders
func (strat *Marketmaker) Start(ctx context.Context) {
	log.Println("Model is running...")

	mainTicker := time.NewTicker(2 * time.Second)
	defer mainTicker.Stop()
	syncTicker := time.NewTicker(5 * time.Minute)
	defer syncTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-syncTicker.C:
			if exch, ok := strat.Exchange.(*lighter.Lighter); ok {
				if err := exch.UpdateOrders(); err != nil {
					log.Printf("Failed to update orders: %v", err)
				}
			}
		case bar := <-strat.Exchange.GetBarUpdates():
			if t := strat.traders[bar.Pair]; t != nil {
				t.updateBar(bar.Data)
			}
		case tu := <-strat.Exchange.GetTradeUpdates():
			if t := strat.traders[tu.Pair]; t != nil {
				t.updateTrade(tu.Data)
			}
		case pd := <-strat.Exchange.GetPairUpdates():
			if t := strat.traders[pd.Pair]; t != nil {
				t.updatePair(pd.Data)
			}
		case pr := <-strat.Exchange.GetPricesUpdates():
			if t := strat.traders[pr.Pair]; t != nil {
				t.updatePrices(pr.Data)
			}

		// main ticker - run evaluation using channel semaphore
		case <-mainTicker.C:
			go func() {
				select {
				case strat.process <- struct{}{}:

					strat.update()

					// Release the process
					<-strat.process
				default:
				}
			}()
		}
	}
}

// Stop cancels all open orders and closes all positions for enabled pairs.
// Pairs listed in config DisabledPairs are skipped (no order cancel/position close).
func (strat *Marketmaker) Stop() {
	// Build disabled set for quick lookup
	disabled := make(map[string]struct{}, len(strat.config.DisabledPairs))
	for _, p := range strat.config.DisabledPairs {
		disabled[p] = struct{}{}
	}

	// Cancel open orders for enabled pairs only
	existingOrders := strat.Exchange.GetOrders()
	ordersToCancel := make([]exchange.Order, 0, len(existingOrders))
	for _, o := range existingOrders {
		if _, skip := disabled[o.Pair]; skip {
			continue
		}
		ordersToCancel = append(ordersToCancel, o)
	}
	if len(ordersToCancel) > 0 {
		strat.Exchange.CancelOrders(ordersToCancel)
	}

	// Close positions with market reduce-only orders for enabled pairs only
	positions := strat.Exchange.GetPositions()
	closeOrders := make([]exchange.Order, 0, len(positions))
	for pair, pos := range positions {
		if pos.Size == 0 {
			continue
		}
		if _, skip := disabled[pair]; skip {
			continue
		}
		var side string
		switch {
		case pos.Size < 0:
			side = "buy"
		default:
			side = "sell"
		}
		closeOrders = append(closeOrders, exchange.Order{
			Pair:       pair,
			Side:       side,
			Type:       "market",
			Size:       math.Abs(pos.Size),
			Price:      0,
			ReduceOnly: true,
		})
	}
	if len(closeOrders) > 0 {
		strat.Exchange.PlaceOrders(closeOrders)
	}

	strat.traders = nil
	strat.Exchange = nil
	strat.config = nil
	log.Println("Marketmaker stopped!")
}

func (strat *Marketmaker) update() {
}

func (strat *Marketmaker) getInitialBars(pair string) error {
	bars, err := strat.Exchange.GetBars(pair, BAR_INTERVAL, BARS_LENGTH)
	if err != nil {
		return err
	}

	t := strat.traders[pair]
	if t == nil {
		return nil
	}

	t.Lock()
	defer t.Unlock()

	t.Bars = t.Bars[:0]
	for _, bar := range bars {
		insertWithLimitInPlace(&t.Bars, bar, BARS_LENGTH)
	}
	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.SMA20 = sma
	}

	return nil
}

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
	bidSize := bidPrice.Size
	askSize := askPrice.Size
	if bid <= 0 || ask <= 0 || ask < bid {
		return
	}

	t.Lock()
	defer t.Unlock()

	t.bestBid = bid
	t.bestAsk = ask
	t.bidsVol = bidSize
	t.asksVol = askSize
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
	insertWithLimitInPlace(&t.Prices, [2]exchange.Price{bidPrice, askPrice}, ARRAY_LENGTH)
	insertWithLimitInPlace(&t.spreadPct, spreadPct, ARRAY_LENGTH)
	t.updateSpreadAvg()
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

	insertWithLimitInPlace(&t.Trades, *trade, TRADES_LENGTH)
	t.tradePerMinute = t.calculateTradesInDuration(time.Minute)
	t.calculateSlippage(trade)
}

// calculateTradesInDuration calculates the number of stored trades inside duration.
// The caller must hold t.Lock or t.RLock.
func (t *trader) calculateTradesInDuration(duration time.Duration) int {
	if len(t.Trades) == 0 {
		return 0
	}

	var mostRecentTime time.Time
	for _, trade := range t.Trades {
		if trade.Order != nil && !trade.Order.Time.IsZero() {
			mostRecentTime = trade.Order.Time
			break
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
	return count
}

func (t *trader) updateBar(bar *exchange.Bar) {
	if bar == nil {
		return
	}

	t.Lock()
	defer t.Unlock()

	previousSMA := t.SMA20
	t.updateBars(*bar)

	if sma := calculateSMA(t.Bars, 20); sma > 0 {
		t.SMA20 = sma
		if previousSMA > 0 {
			t.SMA20SlopePct = ((sma - previousSMA) / previousSMA) * 100
		}
	}
}

// calculateSlippage records price impact versus mark price.
// The caller must hold t.Lock.
func (t *trader) calculateSlippage(trade *exchange.Trade) {
	if trade == nil || len(trade.Fills) == 0 || t.MarkPrice <= 0 {
		return
	}

	side := ""
	if trade.Order != nil {
		side = strings.ToLower(trade.Order.Side)
	}

	var slippagePct float64
	for _, fill := range trade.Fills {
		if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
			continue
		}
		if side == "" {
			side = strings.ToLower(fill.Side)
		}

		// Track worst impacted fill versus mark price.
		switch side {
		case "buy":
			impact := ((fill.Price - t.MarkPrice) / t.MarkPrice) * 100
			if impact > slippagePct {
				slippagePct = impact
			}
		case "sell":
			impact := -((t.MarkPrice - fill.Price) / t.MarkPrice) * 100
			if impact < slippagePct {
				slippagePct = impact
			}
		}
	}

	if side != "buy" && side != "sell" {
		return
	}
	if math.Abs(slippagePct) < 0.01 {
		return
	}

	insertWithLimitInPlace(&t.slippagePct, slippagePct, ARRAY_LENGTH)
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
	if n > ARRAY_LENGTH {
		n = ARRAY_LENGTH
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

func latestFloat(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	return values[0]
}

func (t *trader) updateBars(bar exchange.Bar) {
	if len(t.Bars) > 0 && t.Bars[0].Time.Equal(bar.Time) {
		t.Bars[0] = bar
		return
	}
	if len(t.Bars) > 0 && bar.Time.Before(t.Bars[0].Time) {
		return
	}

	insertWithLimitInPlace(&t.Bars, bar, BARS_LENGTH)
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

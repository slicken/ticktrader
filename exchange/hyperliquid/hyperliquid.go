package hyperliquid

import (
	"context"
	"fmt"
	"log"
	"math"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"ticktrader/config"
	"ticktrader/exchange"

	hl "go_hyperliquid"
)

type Hyperliquid struct {
	*exchange.Exchange
	cli              *hl.Hyperliquid
	assetMap         map[string]hl.AssetInfo
	initTime         time.Time
	latencyMs        atomic.Int64         // Real latency in milliseconds
	latencyTimestamp map[string]time.Time // Track when orders were sent
}

// New Hyperliquid cli
func NewHyperliquid(cfg *config.ExchangeConfig) *Hyperliquid {
	hlClient := hl.NewHyperliquid(&hl.HyperliquidClientConfig{
		IsMainnet:      !cfg.Testnet,
		AccountAddress: strings.ToLower(cfg.APIKey),    // Main account address
		PrivateKey:     strings.ToLower(cfg.APISecret), // API wallet private key
	})
	if hlClient == nil {
		log.Fatalf("Failed to initialize authenticated cli")
	}

	e := exchange.NewExchange(cfg)
	hyperliquid := &Hyperliquid{
		Exchange:         &e,
		cli:              hlClient,
		assetMap:         make(map[string]hl.AssetInfo),
		initTime:         time.Now(),
		latencyMs:        atomic.Int64{},
		latencyTimestamp: make(map[string]time.Time),
	}

	if err := hyperliquid.getAssetAndPairs(); err != nil {
		log.Fatalf("Failed to load pairs: %v", err)
	}

	return hyperliquid
}

// SetPairs adds all symbols to exchange memory
func (e *Hyperliquid) getAssetAndPairs() error {
	// Get perpetual asset map using the hyperliquid repo's function
	perpsAssetMap, err := e.cli.BuildMetaMap()
	if err != nil {
		return fmt.Errorf("failed to build perpetual asset map: %v", err)
	}

	e.Lock()
	defer e.Unlock()

	// Clear existing data
	e.assetMap = perpsAssetMap
	e.Assets = make(map[string]*exchange.Asset)
	e.Pairs = make(map[string]*exchange.Pair)

	// Blacklist of known broken pairs (not found on website, Oracle Price issues)
	brokenPairs := map[string]bool{
		"AI":      true,
		"BADGER":  true,
		"BLZ":     true,
		"BNT":     true,
		"CANTO":   true,
		"CATI":    true,
		"CYBER":   true,
		"FRIEND":  true,
		"FTM":     true,
		"HPOS":    true,
		"ILV":     true,
		"JELLY":   true,
		"KPEPE":   true,
		"LISTA":   true,
		"LOOM":    true,
		"MYRO":    true,
		"NFTI":    true,
		"NIL":     true,
		"NOT":     true,
		"NTRN":    true,
		"OMNI":    true,
		"ONDO":    true,
		"ORBS":    true,
		"OX":      true,
		"PANDORA": true,
		"PIXEL":   true,
		"RDNT":    true,
		"RLB":     true,
		"RNDR":    true,
		"SHIA":    true,
		"STRAX":   true,
		"UNIBOT":  true,
		// Add more broken pairs as discovered
	}

	var quote exchange.Asset
	for assetName, assetInfo := range perpsAssetMap {
		// Skip broken pairs
		if brokenPairs[assetName] {
			continue
		}

		asset := &exchange.Asset{
			Name:    assetName,
			Decimal: assetInfo.SzDecimals,
		}

		// Calculate step size based on size decimals from asset info
		asset.StepSize = 1.0 // Default step size
		if assetInfo.SzDecimals > 0 {
			asset.StepSize = 1.0 / float64(assetInfo.SzDecimals)
		}

		e.Assets[assetName] = asset

		// Create pair (we'll need to get MaxLeverage from somewhere else if needed)
		pair := &exchange.Pair{
			Name:            assetName,
			Base:            *asset,
			Quote:           quote,
			Enabled:         true,
			IsPerp:          true,
			MaxLeverage:     10, // Default max leverage
			FundingInterval: 1 * time.Hour,
			NextFundingTime: e.calculateNextFundingTime(),
		}

		e.Pairs[assetName] = pair
	}

	// Build spot asset map using the hyperliquid repo's function
	spotAssetMap, err := e.cli.BuildSpotMetaMap()
	if err != nil {
		return fmt.Errorf("failed to build spot asset map: %v", err)
	}

	// Add spot pairs that don't conflict with existing perpetual pairs
	for assetName, spotAssetInfo := range spotAssetMap {
		// Skip if this pair already exists (perpetual takes precedence)
		if _, exists := e.Pairs[assetName]; exists {
			continue
		}

		// Skip broken pairs
		if brokenPairs[assetName] {
			continue
		}

		asset := &exchange.Asset{
			Name:    assetName,
			Decimal: spotAssetInfo.SzDecimals,
		}

		// Calculate step size based on size decimals from spot asset info
		asset.StepSize = 1.0 // Default step size
		if spotAssetInfo.SzDecimals > 0 {
			asset.StepSize = 1.0 / float64(spotAssetInfo.SzDecimals)
		}

		e.Assets[assetName] = asset

		// Create spot pair (IsPerp = false)
		pair := &exchange.Pair{
			Name:            assetName,
			Base:            *asset,
			Quote:           quote,
			Enabled:         true,
			IsPerp:          false, // Spot pair
			MaxLeverage:     1,     // No leverage for spot
			FundingInterval: 0,     // No funding for spot
		}

		e.Pairs[assetName] = pair
	}

	return nil
}

func (e *Hyperliquid) calculateNextFundingTime() time.Time {
	now := time.Now().UTC()

	// Hyperliquid funding occurs every hour at the top of the hour
	// Calculate the next funding time (next hour at 00:00)
	nextFunding := time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.UTC)

	// If we're at the last hour of the day, move to next day
	if nextFunding.Hour() == 0 {
		nextFunding = nextFunding.Add(24 * time.Hour)
	}

	return nextFunding.Local()
}

func (e *Hyperliquid) Start(ctx context.Context) error {
	if err := e.cli.WebSocketAPI.Connect(); err != nil {
		return err
	}

	// Disable debug to reduce noise
	e.cli.WebSocketAPI.SetDebug(false)

	if err := e.subscribe(); err != nil {
		return err
	}

	e.Runnning = true
	return nil
}

func (e *Hyperliquid) Stop() error {
	e.cli.WebSocketAPI.Disconnect()
	e.Runnning = false
	log.Println("Hyperliquid stopped!")
	return nil
}

func (e *Hyperliquid) subscribe() error {
	// update orders and positions
	err := e.UpdateOrders()
	if err != nil {
		log.Printf("Failed to update orders: %v", err)
	}
	log.Printf("Orders %d\n", len(e.Orders))

	// update positions and account state
	if err := e.UpdateAccountState(); err != nil {
		log.Printf("Failed to update positions and state: %v", err)
	}
	log.Printf("Positions %d\n", len(e.Positions))

	if len(e.Orders) > 0 {
		for k, n := range e.GetPositions() {
			log.Printf("Position %s %f PNL %f\n", k, n.Size, n.PNL)
		}
	}

	addr := strings.ToLower(e.APIKey)
	log.Println("Subscribing to position updates")
	if err := e.cli.WebSocketAPI.SubscribeUserFills(addr, func(data interface{}) {
		e.handlePositions(data)
	}); err != nil {
		return fmt.Errorf("position updates subscription failed: %v", err)
	}

	log.Println("Subscribing to orders updates")
	if err := e.cli.WebSocketAPI.SubscribeOrderUpdates(addr, func(data interface{}) {
		e.handleOrders(data)
	}); err != nil {
		return fmt.Errorf("order updates subscription failed: %v", err)
	}

	return nil
}

func (e *Hyperliquid) SubscribePair(pair string) error {
	// --- TODO: Fix this ---

	// resp, err := e.cli.UpdateLeverage(pair, true, int(math.Min(e.Assets[pair].MaxLeverage, 10)))
	// if err != nil {
	// 	return fmt.Errorf("failed to update leverage: %v", err)
	// }
	// log.Printf("Leverage updated: %v", resp)

	log.Printf("Subscribing to %s ctx\n", pair)
	if err := e.cli.WebSocketAPI.SubscribeActiveAssetCtx(pair, func(data interface{}) {
		e.handleAssetCtx(data)
	}); err != nil {
		return fmt.Errorf("asset ctx subscription failed: %v", err)
	}

	log.Printf("Subscribing to %s data\n", pair)
	if err := e.cli.SubscribeActiveAssetData(e.ExchangeConfig.APIKey, pair, func(data interface{}) {
		e.handleAssetData(data)
	}); err != nil {
		return fmt.Errorf("asset data subscription failed: %v", err)
	}

	return nil
}

func (e *Hyperliquid) SubscribePrices(pair string) error {
	log.Printf("Subscribing to %s prices\n", pair)
	if err := e.cli.WebSocketAPI.SubscribeBbo(pair, func(data interface{}) {
		e.handlePrices(data)
	}); err != nil {
		return fmt.Errorf("prices subscription failed: %v", err)
	}
	return nil
}

func (e *Hyperliquid) SubscribeOrderbook(pair string) error {
	log.Printf("Subscribing to %s orderbook\n", pair)
	if err := e.cli.WebSocketAPI.SubscribeOrderbook(pair, func(data interface{}) {
		e.handleOrderbook(data)
	}); err != nil {
		return fmt.Errorf("orderbook subscription failed: %v", err)
	}
	return nil
}

func (e *Hyperliquid) SubscribeTrades(pair string) error {
	log.Printf("Subscribing to %s trades\n", pair)
	if err := e.cli.WebSocketAPI.SubscribeTrades(pair, func(data interface{}) {
		e.handleTrades(data)
	}); err != nil {
		return fmt.Errorf("trades subscription failed: %v", err)
	}
	return nil
}

func (e *Hyperliquid) SubscribeBars(pair, interval string) error {
	log.Printf("Subscribing to bars for %s [%s]\n", pair, interval)
	if err := e.cli.SubscribeCandle(pair, interval, func(data interface{}) {
		e.handleBars(data)
	}); err != nil {
		return fmt.Errorf("bars subscription failed: %v", err)
	}
	return nil
}

// UnsubscribePair unsubscribes from asset context and data for a pair
func (e *Hyperliquid) UnsubscribePair(pair string) error {
	log.Printf("Unsubscribing from %s ctx\n", pair)
	if err := e.cli.WebSocketAPI.UnsubscribeActiveAssetCtx(pair); err != nil {
		log.Printf("Error unsubscribing from asset context for %s: %v", pair, err)
		return fmt.Errorf("asset ctx unsubscription failed: %v", err)
	}

	log.Printf("Unsubscribing from %s data\n", pair)
	if err := e.cli.WebSocketAPI.UnsubscribeActiveAssetData(e.ExchangeConfig.APIKey, pair); err != nil {
		log.Printf("Error unsubscribing from asset data for %s: %v", pair, err)
		return fmt.Errorf("asset data unsubscription failed: %v", err)
	}
	return nil
}

// UnsubscribePrices unsubscribes from BBO price updates for a pair
func (e *Hyperliquid) UnsubscribePrices(pair string) error {
	log.Printf("Unsubscribing from %s prices\n", pair)
	if err := e.cli.WebSocketAPI.UnsubscribeBbo(pair); err != nil {
		return fmt.Errorf("prices unsubscription failed: %v", err)
	}
	return nil
}

// UnsubscribeOrderbook unsubscribes from orderbook updates for a pair
func (e *Hyperliquid) UnsubscribeOrderbook(pair string) error {
	log.Printf("Unsubscribing from %s orderbook\n", pair)
	if err := e.cli.WebSocketAPI.UnsubscribeOrderbook(pair); err != nil {
		return fmt.Errorf("orderbook unsubscription failed: %v", err)
	}
	return nil
}

// UnsubscribeTrades unsubscribes from trade updates for a pair
func (e *Hyperliquid) UnsubscribeTrades(pair string) error {
	log.Printf("Unsubscribing from %s trades\n", pair)
	if err := e.cli.WebSocketAPI.UnsubscribeTrades(pair); err != nil {
		return fmt.Errorf("trades unsubscription failed: %v", err)
	}
	return nil
}

// UnsubscribeBars unsubscribes from bar/candle updates for a pair
func (e *Hyperliquid) UnsubscribeBars(pair, interval string) error {
	log.Printf("Unsubscribing from bars for %s [%s]\n", pair, interval)
	if err := e.cli.UnsubscribeCandle(pair, interval); err != nil {
		return fmt.Errorf("bars unsubscription failed: %v", err)
	}
	return nil
}

// UpdateAccountState updates both positions and account state in a single API call
func (e *Hyperliquid) UpdateAccountState() error {
	state, err := e.cli.GetAccountState()
	if err != nil {
		return err
	}

	// Update account-level fields using atomic operations
	e.AccountValue.Store(state.CrossMarginSummary.AccountValue)
	// Set AvailableBalance and FreeMargin
	// For now using AccountValue as AvailableBalance and calculated FreeMargin
	e.AvailableBalance.Store(state.CrossMarginSummary.AccountValue)
	e.FreeMargin.Store(state.CrossMaintenanceMarginUsed)

	// Efficiently update positions with minimal locking
	newPositions := make(map[string]*exchange.Position, len(state.AssetPositions))
	for _, pos := range state.AssetPositions {
		// Skip positions with @ in coin name (spot orders)
		if strings.Contains(pos.Position.Coin, "@") {
			continue
		}

		position := &exchange.Position{
			Pair:     pos.Position.Coin,
			Size:     pos.Position.Szi,     // Size of position
			AvgPrice: pos.Position.EntryPx, // Entry price
			PNL:      pos.Position.UnrealizedPnl,
		}

		// Store in new positions map
		newPositions[pos.Position.Coin] = position
	}

	e.Lock()
	e.Positions = newPositions
	e.Unlock()

	return nil
}

// UpdateOrders fetches and updates all open orders from the API
func (e *Hyperliquid) UpdateOrders() error {
	orders, err := e.cli.GetOpenOrders(e.ExchangeConfig.APIKey)
	if err != nil {
		return err
	}

	e.Lock()
	defer e.Unlock()

	// Clear existing orders
	e.Orders = make(map[string]*exchange.Order)

	// Process each order
	for _, order := range *orders {
		// Skip spot orders (those with @ in coin name)
		if strings.Contains(order.Coin, "@") {
			continue
		}

		orderID := strconv.FormatInt(order.Oid, 10)
		// Convert side from A/B to sell/buy
		side := order.Side
		switch side {
		case "A":
			side = "sell"
		case "B":
			side = "buy"
		}

		// Store in orders map
		orderObj := &exchange.Order{
			ID:         orderID,
			Pair:       order.Coin,
			Side:       side,
			Price:      order.LimitPx,
			Size:       order.Sz,
			Type:       "limit",
			Time:       time.Unix(order.Timestamp, 0).Local(),
			Status:     "open",
			ReduceOnly: order.ReduceOnly,
		}
		e.Orders[orderID] = orderObj
	}
	return nil
}

func (e *Hyperliquid) handleAssetData(data interface{}) {
	// Parse the asset data to extract availableToTrade array
	assetData, ok := data.(map[string]interface{})
	if !ok {
		return
	}

	// Extract and update exchange account values from availableToTrade array
	if availableToTrade, ok := assetData["availableToTrade"].([]interface{}); ok && len(availableToTrade) >= 2 {
		// availableToTrade[0] = Available Balance, availableToTrade[1] = Free Margin
		if availableBalance, ok := availableToTrade[0].(float64); ok {
			e.AvailableBalance.Store(availableBalance)
		}
		if freeMargin, ok := availableToTrade[1].(float64); ok {
			e.FreeMargin.Store(freeMargin)
		}
	}
}

// handleAssetCtx processes asset context updates from websocket
// This updates Pair fields with real-time data like oracle price, market price, funding rate, etc.
func (e *Hyperliquid) handleAssetCtx(data interface{}) {
	assetCtx, ok := data.(map[string]interface{})
	if !ok {
		return
	}

	// Extract asset name from the data
	assetName, ok := assetCtx["coin"].(string)
	if !ok {
		return
	}

	// Get or create pair for this asset
	pair, err := e.Pair(assetName)
	if err != nil {
		return
	}

	// Extract the context data
	ctxData, ok := assetCtx["ctx"].(map[string]interface{})
	if !ok {
		return
	}

	// Update oracle price (index price)
	if oraclePriceStr, ok := ctxData["oraclePx"].(string); ok {
		pair.IndexPrice, _ = strconv.ParseFloat(oraclePriceStr, 64)
	}

	// Update mark price
	if markPriceStr, ok := ctxData["markPx"].(string); ok {
		pair.MarkPrice, _ = strconv.ParseFloat(markPriceStr, 64)
	}

	// Update current price - prefer mid price, then oracle, then mark
	if midPriceStr, ok := ctxData["midPx"].(string); ok {
		midPrice, _ := strconv.ParseFloat(midPriceStr, 64)
		if midPrice > 0 {
			pair.Price = midPrice
		}
	} else if pair.IndexPrice > 0 {
		pair.Price = pair.IndexPrice
	} else if pair.MarkPrice > 0 {
		pair.Price = pair.MarkPrice
	}

	// Update premium percentage from exchange data
	if premiumStr, ok := ctxData["premium"].(string); ok {
		premium, _ := strconv.ParseFloat(premiumStr, 64)
		// Convert to percentage format to match website display
		// HyperLiquid sends decimal format, convert to percentage
		pair.PremiumPct = premium * 100
	} else {
		// Fallback calculation if premium not provided by exchange
		if pair.MarkPrice > 0 && pair.IndexPrice > 0 {
			pair.PremiumPct = ((pair.MarkPrice - pair.IndexPrice) / pair.IndexPrice) * 100
		}
	}

	// Update funding rate
	if fundingRateStr, ok := ctxData["funding"].(string); ok {
		fundingRate, _ := strconv.ParseFloat(fundingRateStr, 64)
		pair.FundingRate = fundingRate * 100
		pair.NextFundingTime = e.calculateNextFundingTime()
	}

	// Open Interest
	if oiStr, ok := ctxData["openInterest"].(string); ok {
		pair.OpenInterest, _ = strconv.ParseFloat(oiStr, 64)
	}

	// 24h Notional Volume (USD)
	if volStr, ok := ctxData["dayNtlVlm"].(string); ok {
		pair.Volume, _ = strconv.ParseFloat(volStr, 64)
	}

	// Store updated pair back to cache
	e.Lock()
	pairPtr := &pair
	e.Pairs[assetName] = pairPtr

	// update positions
	position := e.Positions[assetName]
	if position != nil {
		position.PNL = position.CalculateUnrealizedPNL(pair.MarkPrice)
	}
	e.Unlock()

	// send update to pairinfo channel
	select {
	case e.Notifications.Pair <- exchange.ExchangeUpdate[*exchange.Pair]{
		Pair: assetName,
		Type: "pair",
		Data: &pair,
	}:
	default:
	}
}

// handlePositions processes user positions
func (e *Hyperliquid) handlePositions(data interface{}) {

	e.Lock()
	// Predeclare to avoid goto jumping over declaration in the same scope
	var fills []interface{}
	payload, ok := data.(map[string]interface{})
	if !ok {
		goto sync
	}

	if isSnapshot, ok := payload["isSnapshot"].(bool); ok && isSnapshot {
		goto sync
	}

	fills, ok = payload["fills"].([]interface{})
	if !ok || len(fills) == 0 {
		goto sync
	}

	for _, f := range fills {
		fill, ok := f.(map[string]interface{})
		if !ok {
			continue
		}

		coin, _ := fill["coin"].(string)
		sideRaw, _ := fill["side"].(string)
		szStr, _ := fill["sz"].(string)
		pxStr, _ := fill["px"].(string)
		if coin == "" || sideRaw == "" || szStr == "" || pxStr == "" {
			continue
		}

		sz, _ := strconv.ParseFloat(szStr, 64)
		px, _ := strconv.ParseFloat(pxStr, 64)
		if sz <= 0 || px <= 0 {
			continue
		}

		side := sideRaw
		switch side {
		case "A":
			side = "sell"
		case "B":
			side = "buy"
		}

		// Best-effort: clean temp/cliID orders for this coin to avoid lingering temp state
		for id, o := range e.Orders {
			if o.Pair == coin && (o.Status == "temp" || (len(id) > 2 && id[:2] == "0x")) {
				delete(e.Orders, id)
				delete(e.latencyTimestamp, id)
			}
		}
		currentPos, exists := e.Positions[coin]
		if !exists {
			currentPos = &exchange.Position{Pair: coin}
		}

		prevSize := currentPos.Size
		switch side {
		case "buy":
			if prevSize < 0 { // covering short
				cover := math.Min(sz, math.Abs(prevSize))
				currentPos.Size = prevSize + cover
				if sz > math.Abs(prevSize) { // flip to long
					currentPos.Size = sz - math.Abs(prevSize)
					currentPos.AvgPrice = px
				}
				if currentPos.Size == 0 {
					delete(e.Positions, coin)
					if e.Exchange.Debug {
						log.Printf("POSITION CLOSED [%s]", coin)
					}
					// notify closed by skipping position send
					continue
				}
			} else { // add to long
				newSize := prevSize + sz
				currentPos.AvgPrice = ((math.Abs(prevSize) * currentPos.AvgPrice) + (sz * px)) / math.Abs(newSize)
				currentPos.Size = newSize
			}
		case "sell":
			if prevSize > 0 { // reducing long
				reduce := math.Min(sz, prevSize)
				currentPos.Size = prevSize - reduce
				if sz > prevSize { // flip to short
					currentPos.Size = -(sz - prevSize)
					currentPos.AvgPrice = px
				}
				if currentPos.Size == 0 {
					delete(e.Positions, coin)
					if e.Exchange.Debug {
						log.Printf("POSITION CLOSED [%s]", coin)
					}
					// notify closed by skipping position send
					continue
				}
			} else { // add to short
				newAbs := math.Abs(prevSize) + sz
				currentPos.AvgPrice = ((math.Abs(prevSize) * currentPos.AvgPrice) + (sz * px)) / newAbs
				currentPos.Size = -newAbs
			}
		default:
			continue
		}

		// Save back and unlock
		e.Positions[coin] = currentPos

		if e.Exchange.Debug {
			if !exists {
				log.Printf("POSITION OPEN [%s %s %.6f @ %.6f]",
					coin, side, math.Abs(currentPos.Size), currentPos.AvgPrice)
			} else {
				log.Printf("POSITION UPDATE [%s %s %.6f @ %.6f]",
					coin, side, math.Abs(currentPos.Size), currentPos.AvgPrice)
			}
		}

		// Notify updated position
		select {
		case e.Notifications.Positions <- exchange.ExchangeUpdate[*exchange.Position]{
			Pair: coin,
			Type: "position",
			Data: currentPos,
		}:
		default:
		}
	}

sync:
	e.Unlock()

	if err := e.UpdateAccountState(); err != nil {
		log.Printf("Failed to get account state: %s", err)
	}
}

func (e *Hyperliquid) handleOrders(data interface{}) {
	orders, ok := data.([]interface{})
	if !ok {
		return
	}

	var status string
	for _, orderData := range orders {
		// validate orderData is not broken
		orderMap, ok := orderData.(map[string]interface{})
		if !ok {
			continue
		}
		orderInfo, ok := orderMap["order"].(map[string]interface{})
		if !ok {
			continue
		}
		status, ok = orderMap["status"].(string)
		if !ok {
			continue
		}
		orderID, ok := orderInfo["oid"].(float64)
		if !ok {
			continue
		}
		orderIDStr := fmt.Sprintf("%.0f", orderID)
		coin, ok := orderInfo["coin"].(string)
		if !ok {
			continue
		}
		// Convert A/B to sell/buy
		side := orderInfo["side"].(string)
		switch side {
		case "A":
			side = "sell"
		case "B":
			side = "buy"
		}
		orderType := "limit"
		reduceOnly := false
		if reduceOnlyVal, ok := orderInfo["reduceOnly"].(bool); ok {
			reduceOnly = reduceOnlyVal
		}

		// Extract trigger-related fields from HyperLiquid order data
		var triggerPrice float64
		var triggerCondition string
		if triggerPx, ok := orderInfo["triggerPx"].(string); ok {
			triggerPrice, _ = strconv.ParseFloat(triggerPx, 64)
		}
		if triggerCond, ok := orderInfo["triggerCondition"].(string); ok {
			triggerCondition = triggerCond
		}

		price, _ := strconv.ParseFloat(orderInfo["limitPx"].(string), 64)
		size, _ := strconv.ParseFloat(orderInfo["sz"].(string), 64)
		tstamp, _ := orderInfo["timestamp"].(int64)
		timestamp := time.Unix(int64(tstamp)/1000, (int64(tstamp)%1000)*1000000).Local()

		// Construct orderObj once before switch
		var orderObj exchange.Order
		orderObj = exchange.Order{
			ID:           orderIDStr,
			Pair:         coin,
			Side:         side,
			Price:        price,
			Size:         size,
			Type:         orderType,
			Time:         timestamp,
			Status:       status,
			ReduceOnly:   reduceOnly,
			TriggerPrice: triggerPrice,
			TriggerType:  triggerCondition,
		}

		e.Lock()

		// Extract optional cli order id (cloid) if present
		cloid, _ := orderMap["cloid"].(string)

		// Calculate latency if we have a timestamp for this order
		latencyMsg := ""
		if sentTime, exists := e.latencyTimestamp[orderIDStr]; exists {
			latency := time.Since(sentTime).Milliseconds()
			latencyMsg = fmt.Sprintf("latency: %dms", latency)
			delete(e.latencyTimestamp, orderIDStr)
		} else if cloid != "" {
			// Fallback: use cloid timestamp if WS arrives before HTTP response remaps IDs
			if sentTime, exists := e.latencyTimestamp[cloid]; exists {
				latency := time.Since(sentTime).Milliseconds()
				latencyMsg = fmt.Sprintf("latency: %dms", latency)
				// migrate latency tracking to real order id
				e.latencyTimestamp[orderIDStr] = sentTime
				delete(e.latencyTimestamp, cloid)
			}
		}

		// Best-effort: clean temp/cliID orders for this coin to avoid lingering temp state,
		// but keep the current cloid so we can migrate it to real oid
		for id, o := range e.Orders {
			if id == cloid {
				continue
			}

			// Clean up stuck temp orders older than 30 seconds (any pair)
			if o.Status == "temp" && time.Since(o.Time) > 30*time.Second {
				delete(e.Orders, id)
				delete(e.latencyTimestamp, id)
				if e.Exchange.Debug {
					log.Printf("ORDER CLEANUP [%s] - removing stuck temp order after 30s", id)
				}
				continue
			}

			// Clean temp/cliID orders for this specific coin
			if o.Pair == coin && (o.Status == "temp" || (len(id) > 2 && id[:2] == "0x")) {
				delete(e.Orders, id)
				delete(e.latencyTimestamp, id)
			}
		}

		// If we already track this order under cloid, migrate it under real oid
		if cloid != "" {
			if _, exists := e.Orders[orderIDStr]; !exists {
				if ord, ok := e.Orders[cloid]; ok {
					e.Orders[orderIDStr] = ord
					delete(e.Orders, cloid)
				}
			}
		}

		switch status {
		case "open":
			// Check if order already exists and update it, or create new one
			if existingOrder, exists := e.Orders[orderIDStr]; exists {
				// Update existing order with latest data
				existingOrder.Price = price
				existingOrder.Size = size
				existingOrder.Time = timestamp
				existingOrder.ReduceOnly = reduceOnly
				existingOrder.Status = "open"
				// Keep the orderObj with the correct data from websocket
			} else {
				// Add new order to tracking
				e.Orders[orderIDStr] = &orderObj
			}

		default:
			if order, exists := e.Orders[orderIDStr]; exists {
				orderObj = *order
				orderObj.Status = status
				// Delete the filled order from tracking
				delete(e.Orders, orderIDStr)
				delete(e.latencyTimestamp, orderIDStr)
			} else {
				// No tracked order found, use websocket data
				orderObj.Price = price
				orderObj.Size = size
			}
		}

		e.Unlock()

		if e.Exchange.Debug {
			log.Printf("ORDER %s [%s %s %s %.6f @ %.6f %t] %s", strings.ToUpper(status), orderIDStr, coin, side, size, price, orderObj.ReduceOnly, latencyMsg)
		}

		// send typed update to Notifications
		select {
		case e.Notifications.Orders <- exchange.ExchangeUpdate[*exchange.Order]{
			Pair: coin,
			Type: "order",
			Data: &orderObj,
		}:
		default:
		}
	}
}

func (e *Hyperliquid) handlePrices(data interface{}) {
	bboData := data.(map[string]interface{})
	coin := bboData["coin"].(string)
	timeData := bboData["time"].(float64)
	bboLevels := bboData["bbo"].([]interface{})

	// Convert timestamp to local time (milliseconds to seconds)
	bboTime := time.Unix(int64(timeData)/1000, (int64(timeData)%1000)*1000000).Local()

	// Extract bid and ask
	bidLevel := bboLevels[0].(map[string]interface{})
	askLevel := bboLevels[1].(map[string]interface{})

	// Parse prices and sizes
	bidPrice, _ := strconv.ParseFloat(bidLevel["px"].(string), 64)
	bidSize, _ := strconv.ParseFloat(bidLevel["sz"].(string), 64)
	askPrice, _ := strconv.ParseFloat(askLevel["px"].(string), 64)
	askSize, _ := strconv.ParseFloat(askLevel["sz"].(string), 64)

	// Create Price structs
	bidPriceStruct := exchange.Price{
		Price: bidPrice,
		Size:  bidSize,
		Time:  bboTime,
	}

	askPriceStruct := exchange.Price{
		Price: askPrice,
		Size:  askSize,
		Time:  bboTime,
	}

	// Send prices update
	prices := []exchange.Price{bidPriceStruct, askPriceStruct}
	select {
	case e.Notifications.Prices <- exchange.ExchangeUpdate[*[]exchange.Price]{
		Pair: coin,
		Type: "prices",
		Data: &prices,
	}:
	default:
	}
}

// handleOrderbook processes orderbook updates
func (e *Hyperliquid) handleOrderbook(data interface{}) {
	orderbook, ok := data.(map[string]interface{})
	if !ok {
		return
	}
	levels, ok := orderbook["levels"].([]interface{})
	if !ok || len(levels) != 2 {
		return
	}
	asksSlice, ok := levels[1].([]interface{})
	if !ok {
		return
	}
	bidsSlice, ok := levels[0].([]interface{})
	if !ok {
		return
	}

	coin := orderbook["coin"].(string)

	now := time.Now()
	asks := make([]exchange.Price, 0, len(asksSlice))
	bids := make([]exchange.Price, 0, len(bidsSlice))

	// Process asks using the old untyped logic
	for _, ask := range asksSlice {
		askMap, ok := ask.(map[string]interface{})
		if !ok {
			continue
		}
		priceStr, _ := askMap["px"].(string)
		sizeStr, _ := askMap["sz"].(string)
		price, _ := strconv.ParseFloat(priceStr, 64)
		size, _ := strconv.ParseFloat(sizeStr, 64)
		if price <= 0 || size <= 0 {
			continue
		}

		asks = append(asks, exchange.Price{
			Price: price,
			Size:  size,
			Time:  now,
		})
	}

	// Process bids using the old untyped logic
	for _, bid := range bidsSlice {
		bidMap, ok := bid.(map[string]interface{})
		if !ok {
			continue
		}
		priceStr, _ := bidMap["px"].(string)
		sizeStr, _ := bidMap["sz"].(string)
		price, _ := strconv.ParseFloat(priceStr, 64)
		size, _ := strconv.ParseFloat(sizeStr, 64)
		if price <= 0 || size <= 0 {
			continue
		}

		bids = append(bids, exchange.Price{
			Price: price,
			Size:  size,
			Time:  now,
		})
	}

	e.Lock()
	ob, exists := e.Orderbook[coin]
	if !exists {
		ob = &exchange.Orderbook{Pair: coin}
		e.Orderbook[coin] = ob
	}
	ob.Asks = asks
	ob.Bids = bids
	ob.Sort()
	ob.LastUpdated = now
	snapshot := ob.Clone()
	e.Unlock()

	// Send our Exchange orderbook to notifications
	select {
	case e.Notifications.Orderbooks <- exchange.ExchangeUpdate[*exchange.Orderbook]{
		Pair: coin,
		Type: "orderbook",
		Data: &snapshot,
	}:
	default:
	}
}

// handleTrades processes trade updates
func (e *Hyperliquid) handleTrades(data interface{}) {
	// Handle the actual data structure - array of trade objects
	trades, ok := data.([]interface{})
	if !ok {
		return
	}

	if time.Now().Before(e.initTime.Add(5 * time.Second)) {
		return
	}

	// Pre-allocate map with estimated size to reduce rehashing
	tradeGroups := make(map[string][]map[string]interface{}, len(trades))

	// First pass: Group fills by hash
	for _, tradeData := range trades {
		trade, ok := tradeData.(map[string]interface{})
		if !ok {
			continue
		}
		hash, ok := trade["hash"].(string)
		if !ok {
			continue
		}

		tradeGroups[hash] = append(tradeGroups[hash], trade)
	}

	// Process each unique trade (not individual fills)
	for hash, fills := range tradeGroups {
		if len(fills) == 0 {
			return
		}

		firstFill := fills[0]
		coin, _ := firstFill["coin"].(string)
		side, _ := firstFill["side"].(string)

		switch side {
		case "A":
			side = "sell"
		case "B":
			side = "buy"
		}

		// Calculate trade timestamp once per trade group
		var tradeTimestamp time.Time
		if tradeTime, ok := firstFill["time"].(float64); ok {
			tradeTimestamp = time.Unix(int64(tradeTime)/1000, (int64(tradeTime)%1000)*1000000).Local()
		} else {
			// Use current time if timestamp is missing from trade data
			tradeTimestamp = time.Now()
		}

		// Pre-allocate fillsData slice with exact capacity
		fillsData := make([]*exchange.Fill, 0, len(fills))
		var totalSize float64
		var weightedPrice float64

		for _, fill := range fills {
			priceStr, _ := fill["px"].(string)
			sizeStr, _ := fill["sz"].(string)

			price, _ := strconv.ParseFloat(priceStr, 64)
			size, _ := strconv.ParseFloat(sizeStr, 64)

			totalSize += size
			weightedPrice += price * size

			// Create Fill struct directly in the slice
			fillsData = append(fillsData, &exchange.Fill{
				Pair:  coin,
				Side:  side,
				Price: price,
				Size:  size,
				Time:  tradeTimestamp,
			})
		}

		var avgPrice float64
		if totalSize > 0 {
			avgPrice = weightedPrice / totalSize
		}

		// Create Order struct on stack
		order := exchange.Order{
			ID:    hash,
			Pair:  coin,
			Side:  side,
			Type:  "limit",
			Price: avgPrice,
			Size:  totalSize,
			Time:  tradeTimestamp,
		}

		tradeData := exchange.Trade{
			Order: &order,
			Fills: fillsData,
		}

		select {
		case e.Notifications.Trades <- exchange.ExchangeUpdate[*exchange.Trade]{
			Pair: coin,
			Type: "trade",
			Data: &tradeData,
		}:
		default:
		}
	}
}

func (e *Hyperliquid) handleBars(data interface{}) {
	candle, ok := data.(map[string]interface{})
	if !ok {
		if e.Exchange.Debug {
			log.Printf("Invalid data format")
		}
		return
	}
	// Extract symbol from 's' field
	symbol, ok := candle["s"].(string)
	if !ok {
		if e.Exchange.Debug {
			log.Printf("Invalid symbol format")
		}
		return
	}
	// Extract candle data fields
	openTime, ok := candle["t"].(float64)
	if !ok {
		if e.Exchange.Debug {
			log.Printf("Invalid openTime format")
		}
		return
	}
	open, _ := strconv.ParseFloat(candle["o"].(string), 64)
	high, _ := strconv.ParseFloat(candle["h"].(string), 64)
	low, _ := strconv.ParseFloat(candle["l"].(string), 64)
	close, _ := strconv.ParseFloat(candle["c"].(string), 64)
	volume, _ := strconv.ParseFloat(candle["v"].(string), 64)
	// Convert timestamp to local time (milliseconds to seconds)
	barTime := time.Unix(int64(openTime)/1000, (int64(openTime)%1000)*1000000).Local()

	bar := exchange.Bar{
		Time:   barTime,
		Open:   open,
		High:   high,
		Low:    low,
		Close:  close,
		Volume: volume,
	}

	// send typed update to Notifications
	select {
	case e.Notifications.Bars <- exchange.ExchangeUpdate[*exchange.Bar]{
		Pair: symbol,
		Type: "bars",
		Data: &bar,
	}:
	default:
	}
}

// PlaceOrders places multiple orders in a single request
func (e *Hyperliquid) PlaceOrders(orders []exchange.Order) error {
	// Convert our orders to exchange's internal type
	ordersToPlace := make([]hl.OrderRequest, 0, len(orders)*2)
	useTpSlGrouping := false

	// Validate orders first (before locking)
	for _, order := range orders {
		if order.Type == "" {
			return fmt.Errorf("invalid order type: %s", order.Type)
		}
		if order.Price < 0 {
			return fmt.Errorf("invalid price: %f", order.Price)
		}
		if order.Size < 0 {
			return fmt.Errorf("invalid size: %f", order.Size)
		}
	}

	e.Lock()

	for _, order := range orders {
		var orderType hl.OrderType
		var tif string

		switch {
		case order.Type == "market":
			tif = hl.TifIoc
			orderType = hl.OrderType{
				Limit: &hl.LimitOrderType{
					Tif: tif,
				},
			}
		case order.PostOnly:
			tif = hl.TifAlo
			orderType = hl.OrderType{
				Limit: &hl.LimitOrderType{
					Tif: tif,
				},
			}
		case order.TriggerPrice > 0:
			// Any order becomes a trigger order if TriggerPrice is not 0
			var isMarket bool
			if order.TriggerType == "market" {
				isMarket = true
			} else {
				isMarket = false // Default to limit
			}

			// For standalone trigger orders, we need to determine if it's TP or SL based on the side and price
			var tpSl hl.TpSl
			if order.Side == "buy" {
				// Buy trigger order - likely a stop loss (trigger when price drops)
				tpSl = hl.TriggerSl
			} else {
				// Sell trigger order - likely a take profit (trigger when price rises)
				tpSl = hl.TriggerTp
			}

			orderType = hl.OrderType{
				Trigger: &hl.TriggerOrderType{
					IsMarket:  isMarket,
					TriggerPx: fmt.Sprintf("%f", order.TriggerPrice),
					TpSl:      tpSl,
				},
			}
		default:
			tif = hl.TifGtc
			orderType = hl.OrderType{
				Limit: &hl.LimitOrderType{
					Tif: tif,
				},
			}
		}

		// Create the order request
		bulkOrder := hl.OrderRequest{
			Coin:       order.Pair,
			IsBuy:      order.Side == "buy",
			LimitPx:    order.Price,
			Sz:         math.Abs(order.Size),
			OrderType:  orderType,
			ReduceOnly: order.ReduceOnly,
		}

		// For market orders, set limit price to cross the spread for immediate fill
		if order.Type == "market" && bulkOrder.LimitPx == 0 {
			if ob, ok := e.Orderbook[order.Pair]; ok && len(ob.Asks) > 0 && len(ob.Bids) > 0 {
				switch order.Side {
				case "buy":
					bulkOrder.LimitPx = ob.Asks[0].Price * 1.1
				case "sell":
					bulkOrder.LimitPx = ob.Bids[0].Price * 0.9
				}
			}
		}

		// For trigger orders with Price = 0, set limit price for immediate execution when triggered
		if order.TriggerPrice > 0 && order.Price == 0 && order.TriggerType == "market" {
			if ob, ok := e.Orderbook[order.Pair]; ok && len(ob.Asks) > 0 && len(ob.Bids) > 0 {
				switch order.Side {
				case "buy":
					// For buy trigger, set limit price slightly above current ask for immediate execution
					bulkOrder.LimitPx = ob.Asks[0].Price * 1.001
				case "sell":
					// For sell trigger, set limit price slightly below current bid for immediate execution
					bulkOrder.LimitPx = ob.Bids[0].Price * 0.999
				}
			}
		}

		// Generate cli order ID if not provided
		if order.ID == "" {
			order.ID = hl.GetRandomCloid()
		}
		bulkOrder.Cloid = order.ID

		ordersToPlace = append(ordersToPlace, bulkOrder)
		if e.Exchange.Debug {
			if order.TriggerPrice > 0 {
				log.Printf("PLACE ORDER [%s %s %.6f @ %.6f trigger:%.6f %s]", order.Pair, order.Side, order.Size, order.Price, order.TriggerPrice, order.TriggerType)
			} else {
				log.Printf("PLACE ORDER [%s %s %.6f @ %.6f %t %t]", order.Pair, order.Side, order.Size, order.Price, order.PostOnly, order.ReduceOnly)
			}
		}

		// Add order directly to tracking (same as ModifyOrders/CancelOrders)
		order.Status = "temp"
		order.Time = time.Now()
		e.Orders[order.ID] = &order

		// Check if this order has linked TP/SL orders (for grouping)
		// This works for both regular orders and trigger orders
		if order.TakeProfit || order.StopLoss {
			useTpSlGrouping = true
		}
		// Position overlay for non-trigger orders only
		if order.TriggerPrice == 0 {

			// TEMP position overlay to remove strategy gap until response arrives
			// Apply signed delta now; will be reverted after BulkOrders response (success or error)
			delta := order.Size
			if order.Side == "sell" {
				delta = -delta
			}
			if delta != 0 {
				if existingPosition, exists := e.Positions[order.Pair]; exists {
					existingPosition.Size += delta
					// Keep avg price unchanged for overlay; authoritative price comes from fills
					if existingPosition.Size == 0 {
						delete(e.Positions, order.Pair)
					} else {
						e.Positions[order.Pair] = existingPosition
					}
				} else {
					e.Positions[order.Pair] = &exchange.Position{Pair: order.Pair, Size: delta, AvgPrice: order.Price}
				}
			}

			// For market orders, immediately update positions since they're filled instantly
			if order.Type == "market" {
				// Calculate position change
				var newSize float64
				switch order.Side {
				case "buy":
					newSize = order.Size
				case "sell":
					newSize = -order.Size
				default:
				}

				// Update position immediately
				if existingPosition, exists := e.Positions[order.Pair]; exists {
					// Position exists - update it
					existingPosition.Size += newSize
					// Recalculate average price
					if existingPosition.Size != 0 {
						existingPosition.AvgPrice = (existingPosition.AvgPrice*math.Abs(existingPosition.Size-newSize) + order.Price*math.Abs(newSize)) / math.Abs(existingPosition.Size)
					}
				} else {
					// New position
					e.Positions[order.Pair] = &exchange.Position{
						Pair:     order.Pair,
						Size:     newSize,
						AvgPrice: order.Price,
					}
				}

				if e.Exchange.Debug {
					log.Printf("POSITION OPENED [%s %s %.6f @ %.6f]", order.Pair, order.Side, newSize, order.Price)
				}
			}
		}
	}
	e.Unlock()

	if len(ordersToPlace) == 0 {
		return nil
	}

	// Store latency timestamp before API call
	startTime := time.Now()
	grouping := hl.GroupingNa
	if useTpSlGrouping {
		grouping = hl.GroupingTpSl
	}
	response, err := e.cli.BulkOrders(ordersToPlace, grouping, false)
	if err != nil {
		e.Lock()
		// Revert temp overlays and cleanup tracked temp orders
		for _, order := range orders {
			if order.ID != "" {
				delete(e.Orders, order.ID)
			}
			// revert overlay
			delta := order.Size
			if order.Side == "sell" {
				delta = -delta
			}
			if delta != 0 {
				if pos, ok := e.Positions[order.Pair]; ok {
					pos.Size -= delta
					if pos.Size == 0 {
						delete(e.Positions, order.Pair)
					} else {
						e.Positions[order.Pair] = pos
					}
				}
			}
		}
		e.Unlock()
		return err
	}

	// API call succeeded - store latency timestamps
	e.latencyMs.Store(time.Since(startTime).Milliseconds())
	e.Lock()
	for _, order := range orders {
		if order.ID != "" {
			e.latencyTimestamp[order.ID] = startTime
		}
	}
	e.Unlock()

	// Process response and update orders
	if response != nil && len(response.Response.Data.Statuses) > 0 {
		e.Lock()
		for i, status := range response.Response.Data.Statuses {
			if i < len(orders) {
				orderID := orders[i].ID

				if status.Error != "" {
					// Remove failed order from tracking
					delete(e.Orders, orderID)
					delete(e.latencyTimestamp, orderID)
					if e.Exchange.Debug {
						log.Printf("ORDER ERROR [%s] - %s", orderID, status.Error)
					}
				} else {
					// Success: Update order with real ID from response
					var realID string
					if status.Filled.OrderId != 0 {
						realID = fmt.Sprintf("%d", status.Filled.OrderId)
					} else if status.Resting.OrderId != 0 {
						realID = fmt.Sprintf("%d", status.Resting.OrderId)
					} else {
						// No real ID provided in response - this indicates a problem
						// Remove the order from tracking since we can't get a proper ID
						delete(e.Orders, orderID)
						delete(e.latencyTimestamp, orderID)
						if e.Exchange.Debug {
							log.Printf("ORDER ERROR [%s] - no real ID provided in response", orderID)
						}
						continue
					}

					// Update order with real ID (status will be confirmed by websocket)
					if order, exists := e.Orders[orderID]; exists {
						order.ID = realID
						e.Orders[realID] = order
						delete(e.Orders, orderID)

						// Update latency tracking
						if sentTime, exists := e.latencyTimestamp[orderID]; exists {
							e.latencyTimestamp[realID] = sentTime
							delete(e.latencyTimestamp, orderID)
						}
					}
				}

				// Revert TEMP overlay applied at placement regardless of outcome
				// Authoritative position comes from user fills
				delta := orders[i].Size
				if orders[i].Side == "sell" {
					delta = -delta
				}
				if delta != 0 {
					if pos, ok := e.Positions[orders[i].Pair]; ok {
						pos.Size -= delta
						if pos.Size == 0 {
							delete(e.Positions, orders[i].Pair)
						} else {
							e.Positions[orders[i].Pair] = pos
						}
					}
				}
			}
		}
		e.Unlock()
	}

	return nil
}

// ModifyOrders modifies multiple orders in a single request
func (e *Hyperliquid) ModifyOrders(orders []exchange.Order) error {
	// Convert our orders to exchange's internal type FIRST
	ordersToModify := make([]hl.ModifyOrderRequest, 0, len(orders))
	originalOrders := make(map[string]*exchange.Order, len(orders))

	e.Lock()
	for _, order := range orders {
		// Handle both cli order IDs and real order IDs
		if order.ID == "" {
			if e.Exchange.Debug {
				log.Printf("invalid order ID: empty")
			}
			continue
		}
		// Check if it's a cli order ID (starts with 0x)
		if len(order.ID) > 2 && order.ID[:2] == "0x" {
			if e.Exchange.Debug {
				log.Printf("skipping order ID %s - waiting for confirmation", order.ID)
			}
			continue
		}
		// Parse as real order ID
		oid, err := strconv.ParseInt(order.ID, 10, 64)
		if err != nil {
			continue
		}
		// Only operate on properly synced orders (status "open")
		if order.Status != "open" {
			continue
		}
		if order.Price <= 0 {
			continue
		}
		if order.Size == 0 {
			continue
		}
		// Store original order for potential rollback
		if existingOrder, exists := e.Orders[order.ID]; exists {
			originalOrders[order.ID] = existingOrder
		} else {
			continue
		}

		// Create order type with appropriate TIF
		tif := hl.TifGtc
		if order.PostOnly {
			tif = hl.TifAlo
		}
		orderType := hl.OrderType{
			Limit: &hl.LimitOrderType{
				Tif: tif, // Must be one of: TifGtc, TifIoc, TifAlo
			},
		}

		// Create modify request
		modify := hl.ModifyOrderRequest{
			OrderId:    int(oid),
			Coin:       order.Pair,
			IsBuy:      order.Side == "buy",
			Sz:         math.Abs(order.Size), // Ensure positive size
			LimitPx:    order.Price,
			OrderType:  orderType,
			ReduceOnly: order.ReduceOnly,
		}
		ordersToModify = append(ordersToModify, modify)
		if e.Exchange.Debug {
			log.Printf("MODIFY ORDER [%s %s %s %.6f @ %.6f %t %t]", order.ID, order.Pair, order.Side, order.Size, order.Price, order.PostOnly, order.ReduceOnly)
		}

		// Update order in tracking with new data and set status to temp
		order.Status = "temp"
		order.Time = time.Now()
		e.Orders[order.ID] = &order
	}
	e.Unlock()

	if len(ordersToModify) == 0 {
		return nil
	}

	// Store latency timestamp before API call
	startTime := time.Now()
	response, err := e.cli.BulkModifyOrders(ordersToModify, false)
	if err != nil {
		// Network/connection error - restore all orders to original state
		e.Lock()
		for orderID, originalOrder := range originalOrders {
			e.Orders[orderID] = originalOrder
		}
		e.Unlock()
		return err
	}

	// API call succeeded - store latency timestamps
	e.latencyMs.Store(time.Since(startTime).Milliseconds())
	e.Lock()
	for orderID := range originalOrders {
		e.latencyTimestamp[orderID] = startTime
	}
	e.Unlock()

	// Process response - update orders with new IDs and status
	if response != nil && len(response.Response.Data.Statuses) > 0 {
		e.Lock()
		for i, status := range response.Response.Data.Statuses {
			if i < len(orders) {
				orderID := orders[i].ID

				if status.Error != "" {
					// Check if error indicates order should be removed
					delete(e.Orders, orderID)
					delete(e.latencyTimestamp, orderID)
					if e.Exchange.Debug {
						log.Printf("ORDER ERROR [%s] - %s (removing order)", orderID, status.Error)
					}
				} else {
					// Success: Update order with new ID from response
					var newID string
					if status.Filled.OrderId != 0 {
						newID = fmt.Sprintf("%d", status.Filled.OrderId)
					} else if status.Resting.OrderId != 0 {
						newID = fmt.Sprintf("%d", status.Resting.OrderId)
					} else {
						// No new ID provided, keep existing
						newID = orderID
					}

					// Update order with new ID and keep temp status (will be confirmed by websocket)
					if order, exists := e.Orders[orderID]; exists {
						order.ID = newID
						if newID != orderID {
							e.Orders[newID] = order
							delete(e.Orders, orderID)

							// Move latency timestamp to new ID
							if sentTime, exists := e.latencyTimestamp[orderID]; exists {
								e.latencyTimestamp[newID] = sentTime
								delete(e.latencyTimestamp, orderID)
							}
						}
					}
				}
			}
		}
		e.Unlock()
	}

	return nil
}

// CancelOrders cancels multiple orders in a single request
func (e *Hyperliquid) CancelOrders(orders []exchange.Order) error {
	// Convert our orders to exchange's internal type FIRST
	ordersToCancel := make([]hl.CancelOidWire, 0, len(orders))
	orderIDs := make([]string, 0, len(orders))

	e.Lock()
	for _, order := range orders {
		// Handle both cli order IDs and real order IDs
		if order.ID == "" {
			if e.Exchange.Debug {
				log.Printf("invalid order ID: empty")
			}
			continue
		}
		// Check if it's a cli order ID (starts with 0x)
		if len(order.ID) > 2 && order.ID[:2] == "0x" {
			if e.Exchange.Debug {
				log.Printf("skipping order ID %s - waiting for confirmation", order.ID)
			}
			continue
		}
		// Parse as real order ID
		oid, err := strconv.ParseInt(order.ID, 10, 64)
		if err != nil {
			continue
		}
		// Only operate on properly synced orders (status "open")
		if order.Status != "open" {
			continue
		}
		if order.Price <= 0 {
			continue
		}
		if order.Size == 0 {
			continue
		}

		// Check if order.Pair exists in assetMap
		assetInfo, ok := e.assetMap[order.Pair]
		if !ok {
			continue
		}

		// Verify the order still exists in tracking
		if _, exists := e.Orders[order.ID]; !exists {
			continue
		}

		cancel := hl.CancelOidWire{
			Asset: assetInfo.AssetId,
			Oid:   int(oid),
		}
		ordersToCancel = append(ordersToCancel, cancel)
		orderIDs = append(orderIDs, order.ID)
		if e.Exchange.Debug {
			log.Printf("CANCEL ORDER [%s %s]", order.ID, order.Pair)
		}
		// Do NOT remove the order from tracking yet. Mark it as temp (cancelling)
		// so that if a fill arrives during cancellation, websocket handlers can
		// still correlate and update positions correctly.
		if existing, exists := e.Orders[order.ID]; exists {
			existing.Status = "temp"
			existing.Time = time.Now()
			e.Orders[order.ID] = existing
		}
	}
	e.Unlock()

	if len(ordersToCancel) == 0 {
		return nil
	}

	// Store latency timestamp before API call
	startTime := time.Now()
	response, err := e.cli.BulkCancelOrders(ordersToCancel)
	if err != nil {
		// Network/connection error - restore all orders
		e.Lock()
		for _, orderID := range orderIDs {
			// Find the original order and restore it
			for _, order := range orders {
				if order.ID == orderID {
					e.Orders[orderID] = &order
					break
				}
			}
		}
		e.Unlock()
		return err
	}

	// API call succeeded - store latency timestamps
	e.latencyMs.Store(time.Since(startTime).Milliseconds())
	e.Lock()
	for _, orderID := range orderIDs {
		e.latencyTimestamp[orderID] = startTime
	}
	e.Unlock()

	// Process cancellation response
	if response != nil && len(response.Response.Data.Statuses) > 0 {
		for i, status := range response.Response.Data.Statuses {
			if i < len(orderIDs) {
				orderID := orderIDs[i]
				if status.Error != "" {
					// Order cancellation failed (already filled/cancelled) - keep it removed
					if e.Exchange.Debug {
						log.Printf("ORDER ERROR [%s] - %s", orderID, status.Error)
					}
				}
			}
		}
	}

	return nil
}

func (e *Hyperliquid) GetBars(pair, interval string, length int) ([]exchange.Bar, error) {
	// Helper function to convert interval string to minutes
	intervalToMinutes := func(interval string) int {
		switch interval {
		case "1m":
			return 1
		case "5m":
			return 5
		case "15m":
			return 15
		case "30m":
			return 30
		case "1h":
			return 60
		case "4h":
			return 240
		case "1d":
			return 1440
		case "1w":
			return 10080
		default:
			// Return 1 minute as default for unknown intervals
			return 1
		}
	}
	endTime := time.Now().UnixMilli()
	startTime := endTime - (int64(intervalToMinutes(interval)) * int64(length) * 60 * 1000)

	candles, err := e.cli.GetCandleSnapshot(pair, interval, startTime, endTime)
	if err != nil {
		return nil, fmt.Errorf("failed to get bars: %w", err)
	}

	// Convert candles to exchange.Bars format and reverse order in one loop
	var bars []exchange.Bar
	for i := len(*candles) - 1; i >= 0; i-- {
		candle := (*candles)[i]
		// Convert timestamp to local time (milliseconds to seconds)
		barTime := time.Unix(candle.OpenTime/1000, (candle.OpenTime%1000)*1000000).Local()

		bars = append(bars, exchange.Bar{
			Time:   barTime,
			Open:   candle.Open,
			High:   candle.High,
			Low:    candle.Low,
			Close:  candle.Close,
			Volume: candle.Volume,
		})
	}

	return bars, nil
}

// GetLatency returns the real latency in milliseconds
func (e *Hyperliquid) GetLatency() int64 {
	return e.latencyMs.Load()
}

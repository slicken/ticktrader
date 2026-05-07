package lighter

import (
	"context"
	"fmt"
	"log"
	"math"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"trader-mux/config"
	"trader-mux/exchange"

	lighterapi "github.com/defi-maker/golighter/client"
	lighterclient "github.com/elliottech/lighter-go/client"
	lighterhttp "github.com/elliottech/lighter-go/client/http"
	lightertypes "github.com/elliottech/lighter-go/types"
	"github.com/elliottech/lighter-go/types/txtypes"
)

const (
	mainnetURL = "https://mainnet.zklighter.elliot.ai"
	testnetURL = "https://testnet.zklighter.elliot.ai"

	defaultAccountIndex = int64(0)
	mainnetChainID      = uint32(304)
	testnetChainID      = uint32(300)
	defaultOrderExpiry  = 30 * time.Minute
	authTokenTTL        = 7*time.Hour - time.Minute
	maxBarsLength       = 200
)

type marketInfo struct {
	ID            int
	Symbol        string
	PriceTick     float64
	SizeTick      float64
	QuoteTick     float64
	PriceDecimals uint8
	SizeDecimals  uint8
}

type subscription struct {
	cancel context.CancelFunc
	unsub  func() error
}

type Lighter struct {
	*exchange.Exchange

	http *lighterapi.HTTPClient
	tx   *lighterclient.TxClient

	accountIndex int64
	apiKeyIndex  uint8
	chainID      uint32
	baseURL      string
	wsURL        string

	marketsBySymbol map[string]marketInfo
	symbolByMarket  map[int]string
	unsubOrderbook  map[string]func() error
	streams         map[string]subscription

	latencyMs atomic.Int64
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
}

func NewLighter(cfg *config.ExchangeConfig) *Lighter {
	baseURL := lighterBaseURL(cfg)
	httpClient := lighterapi.NewHTTPClient(baseURL)
	if httpClient == nil {
		log.Fatalf("failed to initialize Lighter HTTP client for %s", baseURL)
	}

	nonceHTTP := lighterhttp.NewClient(baseURL)
	accountIndex := defaultAccountIndex
	chainID := lighterChainID(cfg)
	txClient, err := lighterclient.NewTxClient(nonceHTTP, cfg.APISecret, accountIndex, cfg.APIKeyIndex, chainID)
	if err != nil {
		log.Fatalf("failed to initialize Lighter tx client: %v", err)
	}

	e := exchange.NewExchange(cfg)
	l := &Lighter{
		Exchange:        &e,
		http:            httpClient,
		tx:              txClient,
		accountIndex:    accountIndex,
		apiKeyIndex:     cfg.APIKeyIndex,
		chainID:         chainID,
		baseURL:         baseURL,
		wsURL:           lighterWSURL(baseURL),
		marketsBySymbol: make(map[string]marketInfo),
		symbolByMarket:  make(map[int]string),
		unsubOrderbook:  make(map[string]func() error),
		streams:         make(map[string]subscription),
	}

	if err := l.loadAssetsAndPairs(); err != nil {
		log.Fatalf("failed to load Lighter pairs: %v", err)
	}

	return l
}

func lighterChainID(cfg *config.ExchangeConfig) uint32 {
	if cfg.Testnet {
		return testnetChainID
	}
	return mainnetChainID
}

func lighterBaseURL(cfg *config.ExchangeConfig) string {
	if cfg.BaseURL != "" {
		return strings.TrimRight(cfg.BaseURL, "/")
	}
	if cfg.Testnet {
		return testnetURL
	}
	return mainnetURL
}

func lighterWSURL(baseURL string) string {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "wss://mainnet.zklighter.elliot.ai/stream"
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	default:
		u.Scheme = "wss"
	}
	u.Path = "/stream"
	u.RawQuery = ""
	return u.String()
}

func (e *Lighter) loadAssetsAndPairs() error {
	books, err := e.getOrderBooks()
	if err != nil {
		return fmt.Errorf("get order books: %w", err)
	}

	details, err := e.getOrderBookDetails(0)
	if err != nil {
		return fmt.Errorf("get order book details: %w", err)
	}

	detailByID := make(map[int]lighterOrderBookDetail, len(details.OrderBookDetails))
	for _, detail := range details.OrderBookDetails {
		detailByID[detail.MarketID] = detail
	}

	fundingByID := make(map[int]lighterapi.FundingRate)
	if rates, err := e.http.GetFundingRates(); err == nil {
		for _, rate := range rates.FundingRates {
			fundingByID[rate.MarketId] = rate
		}
	}

	e.Lock()
	defer e.Unlock()

	e.Assets = make(map[string]*exchange.Asset)
	e.Pairs = make(map[string]*exchange.Pair)
	e.marketsBySymbol = make(map[string]marketInfo)
	e.symbolByMarket = make(map[int]string)

	quote := exchange.Asset{Name: "USDC", Decimal: 6, TickSize: 0.000001, StepSize: 0.000001}
	for _, book := range books.OrderBooks {
		if strings.EqualFold(book.Status, "inactive") {
			continue
		}

		detail := detailByID[book.MarketID]
		symbol := strings.ToUpper(book.Symbol)
		priceDecimals := firstNonZeroUint8(book.SupportedPriceDecimals, detail.PriceDecimals, detail.SupportedPriceDecimals)
		sizeDecimals := firstNonZeroUint8(book.SupportedSizeDecimals, detail.SizeDecimals, detail.SupportedSizeDecimals)
		quoteDecimals := firstNonZeroUint8(book.SupportedQuoteDecimals, detail.SupportedQuoteDecimals, 6)
		priceTick := decimalTick(priceDecimals)
		sizeTick := decimalTick(sizeDecimals)

		minOrderSize := parseFloatDefault(firstNonEmpty(book.MinBaseAmount, detail.MinBaseAmount), 0)
		asset := &exchange.Asset{
			Name:         symbol,
			Decimal:      int(sizeDecimals),
			MinOrderSize: minOrderSize,
			TickSize:     priceTick,
			StepSize:     sizeTick,
			MaxLeverage:  leverageFromMargin(detail.MinInitialMarginFraction),
			OnlyIsolated: false,
		}

		maxLeverage := int(asset.MaxLeverage)
		if maxLeverage <= 0 {
			maxLeverage = 10
		}

		pair := &exchange.Pair{
			Name:            symbol,
			Price:           detail.LastTradePrice,
			MarkPrice:       detail.LastTradePrice,
			Base:            *asset,
			Quote:           quote,
			Enabled:         true,
			IsPerp:          true,
			MaxLeverage:     maxLeverage,
			OpenInterest:    detail.OpenInterest,
			Volume:          detail.DailyQuoteTokenVolume,
			FundingInterval: time.Hour,
			NextFundingTime: nextHour(),
		}
		if funding, ok := fundingByID[book.MarketID]; ok {
			pair.FundingRate = funding.Rate * 100
		}

		e.Assets[symbol] = asset
		e.Pairs[symbol] = pair
		e.marketsBySymbol[symbol] = marketInfo{
			ID:            book.MarketID,
			Symbol:        symbol,
			PriceTick:     priceTick,
			SizeTick:      sizeTick,
			QuoteTick:     decimalTick(quoteDecimals),
			PriceDecimals: priceDecimals,
			SizeDecimals:  sizeDecimals,
		}
		e.symbolByMarket[book.MarketID] = symbol
	}

	return nil
}

func (e *Lighter) Start(ctx context.Context) error {
	e.ctx, e.cancel = context.WithCancel(ctx)

	if err := e.UpdateOrders(); err != nil {
		log.Printf("[Lighter] failed to update orders: %v", err)
	}
	if err := e.UpdateAccountState(); err != nil {
		log.Printf("[Lighter] failed to update account state: %v", err)
	}

	e.Lock()
	e.Runnning = true
	e.Unlock()
	return nil
}

func (e *Lighter) Stop() error {
	if e.cancel != nil {
		e.cancel()
	}

	e.mu.Lock()
	for _, unsub := range e.unsubOrderbook {
		_ = unsub()
	}
	for _, sub := range e.streams {
		if sub.unsub != nil {
			_ = sub.unsub()
		} else {
			sub.cancel()
		}
	}
	e.unsubOrderbook = make(map[string]func() error)
	e.streams = make(map[string]subscription)
	e.mu.Unlock()

	e.Lock()
	e.Runnning = false
	e.Unlock()
	log.Println("Lighter stopped!")
	return nil
}

func (e *Lighter) SubscribePair(pair string) error {
	pair = strings.ToUpper(pair)
	market, err := e.market(pair)
	if err != nil {
		return err
	}
	return e.ensureStream("pair:"+pair, func(ctx context.Context) (func() error, error) {
		return e.subscribeMarketStatsRaw(ctx, market)
	})
}

func (e *Lighter) SubscribePrices(pair string) error {
	pair = strings.ToUpper(pair)
	market, err := e.market(pair)
	if err != nil {
		return err
	}
	return e.ensureStream("prices:"+pair, func(ctx context.Context) (func() error, error) {
		return e.subscribeTickerRaw(ctx, market)
	})
}

func (e *Lighter) SubscribeOrderbook(pair string) error {
	return e.ensureOrderbookSubscription(strings.ToUpper(pair))
}

func (e *Lighter) SubscribeTrades(pair string) error {
	pair = strings.ToUpper(pair)
	market, err := e.market(pair)
	if err != nil {
		return err
	}
	return e.ensureStream("trades:"+pair, func(ctx context.Context) (func() error, error) {
		return e.subscribeTradesRaw(ctx, market)
	})
}

func (e *Lighter) SubscribeBars(pair, interval string) error {
	pair = strings.ToUpper(pair)
	_, err := e.market(pair)
	if err != nil {
		return err
	}

	key := "bars:" + pair + ":" + interval
	e.mu.Lock()
	if _, exists := e.streams[key]; exists {
		e.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.streams[key] = subscription{cancel: cancel}
	e.mu.Unlock()

	go e.pollBars(ctx, pair, interval)
	return nil
}

func (e *Lighter) ensureStream(key string, subscribe func(context.Context) (func() error, error)) error {
	e.mu.Lock()
	if _, exists := e.streams[key]; exists {
		e.mu.Unlock()
		return nil
	}
	ctx, cancel := context.WithCancel(e.ctx)
	e.streams[key] = subscription{cancel: cancel}
	e.mu.Unlock()

	unsub, err := subscribe(ctx)
	if err != nil {
		cancel()
		e.mu.Lock()
		delete(e.streams, key)
		e.mu.Unlock()
		return err
	}

	e.mu.Lock()
	e.streams[key] = subscription{cancel: cancel, unsub: unsub}
	e.mu.Unlock()
	return nil
}

func (e *Lighter) UnsubscribePair(pair string) error {
	return e.cancelStream("pair:" + strings.ToUpper(pair))
}

func (e *Lighter) UnsubscribePrices(pair string) error {
	return e.cancelStream("prices:" + strings.ToUpper(pair))
}

func (e *Lighter) UnsubscribeOrderbook(pair string) error {
	pair = strings.ToUpper(pair)
	e.mu.Lock()
	unsub := e.unsubOrderbook[pair]
	delete(e.unsubOrderbook, pair)
	e.mu.Unlock()
	if unsub != nil {
		return unsub()
	}
	return nil
}

func (e *Lighter) UnsubscribeTrades(pair string) error {
	return e.cancelStream("trades:" + strings.ToUpper(pair))
}

func (e *Lighter) UnsubscribeBars(pair, interval string) error {
	return e.cancelStream("bars:" + strings.ToUpper(pair) + ":" + interval)
}

func (e *Lighter) UpdateAccountState() error {
	account, err := e.fetchAccount()
	if err != nil {
		return err
	}

	e.AccountValue.Store(parseFloatDefault(account.TotalAssetValue, 0))
	e.AvailableBalance.Store(parseFloatDefault(account.AvailableBalance, 0))
	e.FreeMargin.Store(parseFloatDefault(account.AvailableBalance, 0))

	positions := make(map[string]*exchange.Position)
	for _, pos := range account.Positions {
		symbol := e.symbolByMarket[pos.MarketID]
		if symbol == "" {
			symbol = strings.ToUpper(pos.Symbol)
		}
		size := signedSize(parseFloatDefault(pos.Position, 0), pos.Sign)
		if size == 0 {
			continue
		}
		positions[symbol] = &exchange.Position{
			Pair:     symbol,
			Size:     size,
			AvgPrice: parseFloatDefault(pos.AvgEntryPrice, 0),
			PNL:      parseFloatDefault(pos.UnrealizedPNL, 0),
		}
	}

	e.Lock()
	e.Positions = positions
	e.Unlock()
	return nil
}

func (e *Lighter) UpdateOrders() error {
	auth, err := e.authToken()
	if err != nil {
		return err
	}

	resp, err := e.getActiveOrders(e.accountIndex, 255, auth)
	if err != nil {
		resp = &lighterOrdersResponse{}
		for _, market := range e.marketsSnapshot() {
			marketResp, marketErr := e.getActiveOrders(e.accountIndex, market.ID, auth)
			if marketErr != nil {
				continue
			}
			resp.Orders = append(resp.Orders, marketResp.Orders...)
		}
	}

	orders := make(map[string]*exchange.Order, len(resp.Orders))
	for _, order := range resp.Orders {
		symbol := e.symbolByMarket[order.MarketIndex]
		if symbol == "" {
			continue
		}
		id := lighterOrderID(order)
		if id == "" {
			continue
		}
		orders[id] = &exchange.Order{
			ID:         id,
			Pair:       symbol,
			Side:       lighterOrderSide(order),
			Price:      parseFloatDefault(order.Price, 0),
			Size:       parseFloatDefault(firstNonEmpty(order.RemainingBaseAmount, order.InitialBaseAmount), 0),
			Type:       firstNonEmpty(order.Type, "limit"),
			Time:       unixAuto(order.Timestamp),
			Status:     firstNonEmpty(order.Status, "open"),
			ReduceOnly: order.ReduceOnly,
		}
	}

	e.Lock()
	e.Orders = orders
	e.Unlock()
	return nil
}

func (e *Lighter) PlaceOrders(orders []exchange.Order) error {
	for i := range orders {
		order := orders[i]
		if err := e.placeOrder(&order); err != nil {
			return err
		}
	}
	return nil
}

func (e *Lighter) ModifyOrders(orders []exchange.Order) error {
	for _, order := range orders {
		if order.ID == "" || order.Status != "open" {
			continue
		}
		market, err := e.market(order.Pair)
		if err != nil {
			return err
		}
		index, err := strconv.ParseInt(order.ID, 10, 64)
		if err != nil {
			continue
		}

		req := &lightertypes.ModifyOrderTxReq{
			MarketIndex:  int16(market.ID),
			Index:        index,
			BaseAmount:   e.baseSteps(market, order.Size),
			Price:        e.priceSteps(market, order.Price),
			TriggerPrice: e.priceSteps(market, order.TriggerPrice),
		}

		txInfo, err := e.tx.GetModifyOrderTransaction(req, nil)
		if err != nil {
			return err
		}

		start := time.Now()
		if _, err := e.http.SendRawTx(txInfo); err != nil {
			return err
		}
		e.latencyMs.Store(time.Since(start).Milliseconds())

		e.Lock()
		if existing := e.Orders[order.ID]; existing != nil {
			existing.Price = order.Price
			existing.Size = order.Size
			existing.Status = "temp"
			existing.Time = time.Now()
		}
		e.Unlock()
	}
	return nil
}

func (e *Lighter) CancelOrders(orders []exchange.Order) error {
	for _, order := range orders {
		if order.ID == "" || order.Status != "open" {
			continue
		}
		market, err := e.market(order.Pair)
		if err != nil {
			return err
		}
		index, err := strconv.ParseInt(order.ID, 10, 64)
		if err != nil {
			continue
		}

		req := &lightertypes.CancelOrderTxReq{
			MarketIndex: int16(market.ID),
			Index:       index,
		}
		txInfo, err := e.tx.GetCancelOrderTransaction(req, nil)
		if err != nil {
			return err
		}

		start := time.Now()
		if _, err := e.http.SendRawTx(txInfo); err != nil {
			return err
		}
		e.latencyMs.Store(time.Since(start).Milliseconds())

		e.Lock()
		if existing := e.Orders[order.ID]; existing != nil {
			existing.Status = "temp"
			existing.Time = time.Now()
		}
		e.Unlock()
	}
	return nil
}

func (e *Lighter) GetBars(pair, interval string, length int) ([]exchange.Bar, error) {
	if length > maxBarsLength {
		length = maxBarsLength
	}

	market, err := e.market(pair)
	if err != nil {
		return nil, err
	}
	resolution := lighterResolution(interval)
	end := time.Now().Unix()
	start := time.Now().Add(-intervalDuration(interval) * time.Duration(length+1)).Unix()
	candles, err := e.getCandles(market.ID, resolution, start, end, int32(length))
	if err != nil {
		return nil, err
	}

	candleData := candles.Candles
	if length > 0 && len(candleData) > length {
		candleData = candleData[len(candleData)-length:]
	}

	bars := make([]exchange.Bar, 0, len(candleData))
	for _, candle := range candleData {
		bars = append(bars, exchange.Bar{
			Time:   unixAuto(candle.Timestamp),
			Open:   candle.Open,
			High:   candle.High,
			Low:    candle.Low,
			Close:  candle.Close,
			Volume: candle.Volume,
		})
	}
	return bars, nil
}

func (e *Lighter) GetLatency() int64 {
	return e.latencyMs.Load()
}

func (e *Lighter) ensureOrderbookSubscription(pair string) error {
	market, err := e.market(pair)
	if err != nil {
		return err
	}

	e.mu.Lock()
	if _, exists := e.unsubOrderbook[pair]; exists {
		e.mu.Unlock()
		return nil
	}
	e.mu.Unlock()

	unsub, err := e.subscribeOrderbookRaw(e.ctx, market)
	if err != nil {
		return err
	}

	e.mu.Lock()
	e.unsubOrderbook[pair] = unsub
	e.mu.Unlock()
	return nil
}

func (e *Lighter) placeOrder(order *exchange.Order) error {
	if order.Type == "" {
		order.Type = "limit"
	}
	if order.Side != "buy" && order.Side != "sell" {
		return fmt.Errorf("invalid order side: %s", order.Side)
	}
	if order.Size <= 0 {
		return fmt.Errorf("invalid order size: %f", order.Size)
	}

	market, err := e.market(order.Pair)
	if err != nil {
		return err
	}

	price := order.Price
	if price <= 0 || order.Type == "market" {
		price = e.crossPrice(market.Symbol, order.Side)
	}
	if price <= 0 {
		return fmt.Errorf("price unavailable for %s %s order", market.Symbol, order.Type)
	}

	clientOrderID := time.Now().UnixNano() & ((1 << 48) - 1)
	if parsed, err := strconv.ParseInt(order.ID, 10, 64); err == nil && parsed > 0 && parsed <= txtypes.MaxClientOrderIndex {
		clientOrderID = parsed
	}

	req := &lightertypes.CreateOrderTxReq{
		MarketIndex:      int16(market.ID),
		ClientOrderIndex: clientOrderID,
		BaseAmount:       e.baseSteps(market, order.Size),
		Price:            e.priceSteps(market, price),
		IsAsk:            boolToUint8(order.Side == "sell"),
		Type:             e.orderType(order),
		TimeInForce:      e.timeInForce(order),
		ReduceOnly:       boolToUint8(order.ReduceOnly),
		TriggerPrice:     e.priceSteps(market, order.TriggerPrice),
	}
	if req.TimeInForce != txtypes.ImmediateOrCancel {
		req.OrderExpiry = time.Now().Add(defaultOrderExpiry).UnixMilli()
	}

	txInfo, err := e.tx.GetCreateOrderTransaction(req, nil)
	if err != nil {
		return err
	}

	start := time.Now()
	if _, err := e.http.SendRawTx(txInfo); err != nil {
		return err
	}
	e.latencyMs.Store(time.Since(start).Milliseconds())

	tracked := *order
	tracked.ID = strconv.FormatInt(clientOrderID, 10)
	tracked.Pair = market.Symbol
	tracked.Price = price
	tracked.Status = "temp"
	tracked.Time = time.Now()

	e.Lock()
	e.Orders[tracked.ID] = &tracked
	e.Unlock()

	if e.Exchange.Debug {
		log.Printf("LIGHTER PLACE [%s %s %.6f @ %.6f %s]", tracked.Pair, tracked.Side, tracked.Size, tracked.Price, tracked.Type)
	}
	return nil
}

func (e *Lighter) handleOrderbook(pair string, resp lighterapi.LighterOrderBookResponse) {
	now := time.Now()
	e.Lock()
	ob := e.Orderbook[pair]
	if ob == nil || resp.IsSnapshot {
		ob = &exchange.Orderbook{
			Pair: pair,
			Bids: make([]exchange.Price, 0, len(resp.Bids)),
			Asks: make([]exchange.Price, 0, len(resp.Asks)),
		}
		e.Orderbook[pair] = ob
		if resp.IsSnapshot {
			ob.Bids = ob.Bids[:0]
			ob.Asks = ob.Asks[:0]
		}
	}

	if resp.IsSnapshot {
		for _, bid := range resp.Bids {
			ob.Bids = append(ob.Bids, priceFromLevel(bid, now))
		}
		for _, ask := range resp.Asks {
			ob.Asks = append(ob.Asks, priceFromLevel(ask, now))
		}
		ob.Sort()
	} else {
		for _, bid := range resp.Bids {
			ob.AddBid(priceFromLevel(bid, now))
		}
		for _, ask := range resp.Asks {
			ob.AddAsk(priceFromLevel(ask, now))
		}
	}
	ob.LastUpdated = now
	snapshot := ob.Clone()

	if pairData := e.Pairs[pair]; pairData != nil {
		if len(ob.Bids) > 0 && len(ob.Asks) > 0 {
			pairData.Price = (ob.Bids[0].Price + ob.Asks[0].Price) / 2
			pairData.MarkPrice = pairData.Price
		}
	}
	e.Unlock()

	select {
	case e.Notifications.Orderbooks <- exchange.ExchangeUpdate[*exchange.Orderbook]{Pair: pair, Type: "orderbook", Data: &snapshot}:
	default:
	}
	e.notifyUpdate(pair, "orderbook", &snapshot)
}

func (e *Lighter) handleAccount(resp lighterapi.LighterAccountResponse) {
	if resp.RawAccountUpdate == nil {
		return
	}

	update := resp.RawAccountUpdate
	for key, pos := range update.Positions {
		symbol := e.symbolByMarket[int(pos.MarketId)]
		if symbol == "" {
			symbol = strings.ToUpper(pos.Symbol)
		}
		if symbol == "" {
			symbol = strings.ToUpper(key)
		}
		position := &exchange.Position{
			Pair:     symbol,
			Size:     signedSize(parseFloatDefault(pos.Position, 0), int(pos.Sign)),
			AvgPrice: parseFloatDefault(pos.AvgEntryPrice, 0),
			PNL:      parseFloatDefault(pos.UnrealizedPnl, 0),
		}

		e.Lock()
		if position.Size == 0 {
			delete(e.Positions, symbol)
		} else {
			e.Positions[symbol] = position
		}
		e.Unlock()

		select {
		case e.Notifications.Positions <- exchange.ExchangeUpdate[*exchange.Position]{Pair: symbol, Type: "position", Data: position}:
		default:
		}
		e.notifyUpdate(symbol, "position", position)
	}

	for _, trades := range update.Trades {
		for _, wsTrade := range trades {
			symbol := e.symbolByMarket[int(wsTrade.MarketId)]
			if symbol == "" {
				continue
			}
			trade := tradeFromWS(symbol, wsTrade)
			select {
			case e.Notifications.Trades <- exchange.ExchangeUpdate[*exchange.Trade]{Pair: symbol, Type: "trade", Data: trade}:
			default:
			}
			e.notifyUpdate(symbol, "trade", trade)
		}
	}
}

func (e *Lighter) pollBars(ctx context.Context, pair, interval string) {
	ticker := time.NewTicker(intervalDuration(interval))
	defer ticker.Stop()

	var lastTime time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			bars, err := e.GetBars(pair, interval, 2)
			if err != nil || len(bars) == 0 {
				continue
			}
			bar := bars[len(bars)-1]
			if !bar.Time.After(lastTime) {
				continue
			}
			lastTime = bar.Time
			select {
			case e.Notifications.Bars <- exchange.ExchangeUpdate[*exchange.Bar]{Pair: pair, Type: "bar", Data: &bar}:
			default:
			}
			e.notifyUpdate(pair, "bar", &bar)
		}
	}
}

func (e *Lighter) notifyUpdate(pair, updateType string, data any) {
	select {
	case e.Notifications.Updates <- exchange.ExchangeUpdate[any]{Pair: pair, Type: updateType, Data: data}:
	default:
	}
}

func (e *Lighter) fetchAccount() (*lighterAccount, error) {
	resp, err := e.getAccount(e.accountIndex)
	if err != nil {
		return nil, err
	}
	if resp == nil || len(resp.Accounts) == 0 {
		return nil, fmt.Errorf("empty Lighter account response")
	}
	for i := range resp.Accounts {
		if resp.Accounts[i].Index == e.accountIndex {
			return &resp.Accounts[i], nil
		}
	}
	return &resp.Accounts[0], nil
}

func (e *Lighter) authToken() (string, error) {
	return e.tx.GetAuthToken(time.Now().Add(authTokenTTL))
}

func (e *Lighter) market(pair string) (marketInfo, error) {
	e.RLock()
	defer e.RUnlock()
	market, ok := e.marketsBySymbol[strings.ToUpper(pair)]
	if !ok {
		return marketInfo{}, fmt.Errorf("Lighter pair %s not found", pair)
	}
	return market, nil
}

func (e *Lighter) marketsSnapshot() []marketInfo {
	e.RLock()
	defer e.RUnlock()
	markets := make([]marketInfo, 0, len(e.marketsBySymbol))
	for _, market := range e.marketsBySymbol {
		markets = append(markets, market)
	}
	return markets
}

func (e *Lighter) cancelStream(key string) error {
	e.mu.Lock()
	sub := e.streams[key]
	delete(e.streams, key)
	e.mu.Unlock()
	if sub.unsub != nil {
		return sub.unsub()
	}
	if sub.cancel != nil {
		sub.cancel()
	}
	return nil
}

func (e *Lighter) crossPrice(pair, side string) float64 {
	e.RLock()
	defer e.RUnlock()
	ob := e.Orderbook[pair]
	if ob == nil || len(ob.Bids) == 0 || len(ob.Asks) == 0 {
		return 0
	}
	if side == "buy" {
		return ob.Asks[0].Price * 1.01
	}
	return ob.Bids[0].Price * 0.99
}

func (e *Lighter) priceSteps(market marketInfo, price float64) uint32 {
	if price <= 0 || market.PriceTick <= 0 {
		return 0
	}
	steps := math.Floor(price / market.PriceTick)
	if steps < 1 {
		steps = 1
	}
	if steps > math.MaxUint32 {
		steps = math.MaxUint32
	}
	return uint32(steps)
}

func (e *Lighter) baseSteps(market marketInfo, size float64) int64 {
	if size <= 0 || market.SizeTick <= 0 {
		return 0
	}
	steps := math.Floor(math.Abs(size) / market.SizeTick)
	if steps < 1 {
		steps = 1
	}
	if steps > float64(txtypes.MaxOrderBaseAmount) {
		steps = float64(txtypes.MaxOrderBaseAmount)
	}
	return int64(steps)
}

func (e *Lighter) orderType(order *exchange.Order) uint8 {
	if order.TriggerPrice > 0 {
		if order.TakeProfit {
			if order.TriggerType == "limit" {
				return txtypes.TakeProfitLimitOrder
			}
			return txtypes.TakeProfitOrder
		}
		if order.TriggerType == "limit" {
			return txtypes.StopLossLimitOrder
		}
		return txtypes.StopLossOrder
	}
	if order.Type == "market" {
		return txtypes.MarketOrder
	}
	return txtypes.LimitOrder
}

func (e *Lighter) timeInForce(order *exchange.Order) uint8 {
	if order.Type == "market" || order.TriggerType == "market" {
		return txtypes.ImmediateOrCancel
	}
	if order.PostOnly {
		return txtypes.PostOnly
	}
	return txtypes.GoodTillTime
}

func firstNonZeroUint8(values ...uint8) uint8 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

func decimalTick(decimals uint8) float64 {
	return math.Pow(10, -float64(decimals))
}

func leverageFromMargin(marginFraction uint32) float64 {
	if marginFraction == 0 {
		return 10
	}
	return float64(txtypes.MarginFractionTick) / float64(marginFraction)
}

func nextHour() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), now.Day(), now.Hour()+1, 0, 0, 0, time.UTC).Local()
}

func parseFloatDefault(raw string, fallback float64) float64 {
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	return value
}

func parseFloat(raw string) (float64, bool) {
	if raw == "" {
		return 0, false
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func signedSize(size float64, sign int) float64 {
	if sign < 0 && size > 0 {
		return -size
	}
	return size
}

func priceFromLevel(level lighterapi.PriceLevel, t time.Time) exchange.Price {
	return exchange.Price{
		Price: parseFloatDefault(level.Price, 0),
		Size:  parseFloatDefault(level.Quantity, 0),
		Time:  t,
	}
}

func lighterOrderID(order lighterOrder) string {
	if order.OrderID != "" {
		return order.OrderID
	}
	if order.OrderIndex > 0 {
		return strconv.FormatInt(order.OrderIndex, 10)
	}
	if order.ClientOrderIndex > 0 {
		return strconv.FormatInt(order.ClientOrderIndex, 10)
	}
	return ""
}

func lighterOrderSide(order lighterOrder) string {
	if order.Side == "buy" || order.Side == "sell" {
		return order.Side
	}
	if order.IsAsk {
		return "sell"
	}
	return "buy"
}

func unixAuto(ts int64) time.Time {
	if ts <= 0 {
		return time.Now()
	}
	if ts > 1_000_000_000_000 {
		return time.UnixMilli(ts).Local()
	}
	return time.Unix(ts, 0).Local()
}

func lighterResolution(interval string) string {
	switch interval {
	case "1m":
		return "1m"
	case "5m":
		return "5m"
	case "15m":
		return "15m"
	case "30m":
		return "30m"
	case "1h":
		return "1h"
	case "4h":
		return "4h"
	case "12h":
		return "12h"
	case "1d":
		return "1d"
	case "1w":
		return "1w"
	default:
		return interval
	}
}

func intervalDuration(interval string) time.Duration {
	switch interval {
	case "1m":
		return time.Minute
	case "5m":
		return 5 * time.Minute
	case "15m":
		return 15 * time.Minute
	case "30m":
		return 30 * time.Minute
	case "1h":
		return time.Hour
	case "4h":
		return 4 * time.Hour
	case "1d":
		return 24 * time.Hour
	default:
		return time.Minute
	}
}

func tradeFromREST(pair string, t lighterTrade) *exchange.Trade {
	side := t.Side
	if side != "buy" && side != "sell" {
		side = "buy"
		if t.IsMakerAsk {
			side = "sell"
		}
	}
	fill := &exchange.Fill{
		Pair:  pair,
		Side:  side,
		Price: parseFloatDefault(t.Price, 0),
		Size:  parseFloatDefault(t.Size, 0),
		Time:  unixAuto(t.Timestamp),
	}
	order := &exchange.Order{
		ID:    strconv.FormatInt(t.TradeID, 10),
		Pair:  pair,
		Side:  side,
		Price: fill.Price,
		Size:  fill.Size,
		Type:  "limit",
		Time:  fill.Time,
	}
	return &exchange.Trade{Order: order, Fills: []*exchange.Fill{fill}}
}

func tradeFromWS(pair string, t lighterapi.WSTrade) *exchange.Trade {
	side := "buy"
	if t.IsMakerAsk {
		side = "sell"
	}
	fill := &exchange.Fill{
		Pair:  pair,
		Side:  side,
		Price: parseFloatDefault(t.Price, 0),
		Size:  parseFloatDefault(t.Size, 0),
		Time:  unixAuto(t.Timestamp),
	}
	order := &exchange.Order{
		ID:    strconv.FormatInt(t.TradeId, 10),
		Pair:  pair,
		Side:  side,
		Price: fill.Price,
		Size:  fill.Size,
		Type:  "limit",
		Time:  fill.Time,
	}
	return &exchange.Trade{Order: order, Fills: []*exchange.Fill{fill}}
}

func boolToUint8(value bool) uint8 {
	if value {
		return 1
	}
	return 0
}

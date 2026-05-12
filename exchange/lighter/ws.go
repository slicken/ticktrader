package lighter

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"trader-mux/exchange"

	lighterapi "github.com/defi-maker/golighter/client"
	"github.com/gorilla/websocket"
)

func (e *Lighter) subscribeOrderbookRaw(ctx context.Context, market marketInfo) (func() error, error) {
	return e.subscribeRaw(ctx, fmt.Sprintf("order_book/%d", market.ID), func(data []byte) error {
		return e.handleRawOrderbook(market.Symbol, data)
	})
}

func (e *Lighter) subscribeTickerRaw(ctx context.Context, market marketInfo) (func() error, error) {
	return e.subscribeRaw(ctx, fmt.Sprintf("ticker/%d", market.ID), func(data []byte) error {
		return e.handleRawTicker(market.Symbol, data)
	})
}

func (e *Lighter) subscribeTradesRaw(ctx context.Context, market marketInfo) (func() error, error) {
	return e.subscribeRaw(ctx, fmt.Sprintf("trade/%d", market.ID), func(data []byte) error {
		return e.handleRawTrades(market.Symbol, data)
	})
}

func (e *Lighter) subscribeMarketStatsRaw(ctx context.Context, market marketInfo) (func() error, error) {
	return e.subscribeRaw(ctx, fmt.Sprintf("market_stats/%d", market.ID), func(data []byte) error {
		return e.handleRawMarketStats(market.Symbol, data)
	})
}

func (e *Lighter) subscribeRaw(ctx context.Context, channel string, handle func([]byte) error) (func() error, error) {
	runCtx, runCancel := context.WithCancel(ctx)

	var connMu sync.Mutex
	var currentConn *websocket.Conn

	go func() {
		const minBackoff = time.Second
		const maxBackoff = 30 * time.Second
		backoff := minBackoff

		for {
			if runCtx.Err() != nil {
				return
			}

			conn, _, err := websocket.DefaultDialer.Dial(e.wsURL, http.Header{})
			if err != nil {
				if runCtx.Err() == nil {
					log.Printf("[Lighter] websocket %s dial error: %v", channel, err)
				}
				if !sleepCtx(runCtx, backoff) {
					return
				}
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}

			if runCtx.Err() != nil {
				_ = conn.Close()
				return
			}

			var writeMu sync.Mutex
			writeJSON := func(value any) error {
				writeMu.Lock()
				defer writeMu.Unlock()
				return conn.WriteJSON(value)
			}

			if err := writeJSON(map[string]string{
				"type":    "subscribe",
				"channel": channel,
			}); err != nil {
				if runCtx.Err() == nil {
					log.Printf("[Lighter] websocket %s subscribe send failed: %v", channel, err)
				}
				_ = conn.Close()
				if !sleepCtx(runCtx, backoff) {
					return
				}
				if backoff < maxBackoff {
					backoff *= 2
					if backoff > maxBackoff {
						backoff = maxBackoff
					}
				}
				continue
			}
			if e.Exchange.Debug {
				log.Printf("[Lighter] websocket subscribed %s", channel)
			}

			connMu.Lock()
			currentConn = conn
			connMu.Unlock()
			backoff = minBackoff

			connCtx, connCancel := context.WithCancel(runCtx)
			done := make(chan struct{})
			go func(c *websocket.Conn) {
				defer close(done)
				e.readRaw(connCtx, c, channel, writeJSON, handle)
			}(conn)

			<-done
			connCancel()

			connMu.Lock()
			currentConn = nil
			connMu.Unlock()

			if runCtx.Err() != nil {
				return
			}
			log.Printf("[Lighter] websocket %s disconnected, reconnecting...", channel)
			if !sleepCtx(runCtx, backoff) {
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
			}
		}
	}()

	return func() error {
		runCancel()
		connMu.Lock()
		c := currentConn
		currentConn = nil
		connMu.Unlock()
		if c != nil {
			_ = c.Close()
		}
		return nil
	}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func (e *Lighter) readRaw(ctx context.Context, conn *websocket.Conn, channel string, writeJSON func(any) error, handle func([]byte) error) {
	defer conn.Close()

	keepAlive := time.NewTicker(30 * time.Second)
	defer keepAlive.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-keepAlive.C:
				_ = writeJSON(map[string]string{"type": "ping"})
			}
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		_, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("[Lighter] websocket %s error: %v", channel, err)
			}
			return
		}

		var envelope struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &envelope); err == nil {
			switch envelope.Type {
			case "ping":
				_ = writeJSON(map[string]string{"type": "pong"})
				continue
			case "connected", "subscribed":
				continue
			}
		}
		if err := handle(data); err != nil && e.Exchange.Debug {
			log.Printf("[Lighter] websocket %s handler failed: %v", channel, err)
		}
	}
}

func (e *Lighter) handleRawOrderbook(symbol string, data []byte) error {
	var msg struct {
		Type      string `json:"type"`
		OrderBook struct {
			Asks []struct {
				Price string `json:"price"`
				Size  string `json:"size"`
			} `json:"asks"`
			Bids []struct {
				Price string `json:"price"`
				Size  string `json:"size"`
			} `json:"bids"`
		} `json:"order_book"`
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}

	switch msg.Type {
	case "subscribed/order_book", "update/order_book":
		resp := lighterapi.LighterOrderBookResponse{
			Bids:       make([]lighterapi.PriceLevel, 0, len(msg.OrderBook.Bids)),
			Asks:       make([]lighterapi.PriceLevel, 0, len(msg.OrderBook.Asks)),
			Timestamp:  msg.Timestamp,
			IsSnapshot: msg.Type == "subscribed/order_book",
		}
		for _, bid := range msg.OrderBook.Bids {
			resp.Bids = append(resp.Bids, lighterapi.PriceLevel{Price: bid.Price, Quantity: bid.Size})
		}
		for _, ask := range msg.OrderBook.Asks {
			resp.Asks = append(resp.Asks, lighterapi.PriceLevel{Price: ask.Price, Quantity: ask.Size})
		}
		e.handleOrderbook(symbol, resp)
	}
	return nil
}

func (e *Lighter) handleRawTicker(symbol string, data []byte) error {
	var msg struct {
		Type   string `json:"type"`
		Ticker struct {
			Ask struct {
				Price string `json:"price"`
				Size  string `json:"size"`
			} `json:"a"`
			Bid struct {
				Price string `json:"price"`
				Size  string `json:"size"`
			} `json:"b"`
		} `json:"ticker"`
		Timestamp int64 `json:"timestamp"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	if msg.Type != "update/ticker" {
		return nil
	}

	now := unixAuto(msg.Timestamp)
	prices := []exchange.Price{
		{Price: parseFloatDefault(msg.Ticker.Bid.Price, 0), Size: parseFloatDefault(msg.Ticker.Bid.Size, 0), Time: now},
		{Price: parseFloatDefault(msg.Ticker.Ask.Price, 0), Size: parseFloatDefault(msg.Ticker.Ask.Size, 0), Time: now},
	}

	select {
	case e.Notifications.Prices <- exchange.ExchangeUpdate[*[]exchange.Price]{Pair: symbol, Type: "prices", Data: &prices}:
	default:
	}
	e.notifyUpdate(symbol, "prices", &prices)
	return nil
}

func (e *Lighter) handleRawTrades(symbol string, data []byte) error {
	var msg struct {
		Type              string         `json:"type"`
		Trades            []lighterTrade `json:"trades"`
		LiquidationTrades []lighterTrade `json:"liquidation_trades"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	if msg.Type != "update/trade" {
		return nil
	}

	for _, rawTrade := range append(msg.Trades, msg.LiquidationTrades...) {
		trade := tradeFromREST(symbol, rawTrade)
		select {
		case e.Notifications.Trades <- exchange.ExchangeUpdate[*exchange.Trade]{Pair: symbol, Type: "trade", Data: trade}:
		default:
		}
		e.notifyUpdate(symbol, "trade", trade)
	}
	return nil
}

func (e *Lighter) handleRawMarketStats(symbol string, data []byte) error {
	var msg struct {
		Type        string `json:"type"`
		MarketStats struct {
			Symbol                string   `json:"symbol"`
			LastTradePrice        string   `json:"last_trade_price"`
			MarkPrice             string   `json:"mark_price"`
			MidPrice              string   `json:"mid_price"`
			OpenInterest          string   `json:"open_interest"`
			CurrentFundingRate    string   `json:"current_funding_rate"`
			DailyQuoteTokenVolume *float64 `json:"daily_quote_token_volume"`
		} `json:"market_stats"`
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return err
	}
	if msg.Type != "update/market_stats" {
		return nil
	}

	pair := symbol
	if msg.MarketStats.Symbol != "" {
		pair = msg.MarketStats.Symbol
	}

	e.Lock()
	pairData := e.Pairs[pair]
	if pairData != nil {
		if price, ok := parseFloat(firstNonEmpty(msg.MarketStats.LastTradePrice, msg.MarketStats.MidPrice)); ok {
			pairData.Price = price
		}
		if markPrice, ok := parseFloat(msg.MarketStats.MarkPrice); ok {
			pairData.MarkPrice = markPrice
		}
		if openInterest, ok := parseFloat(msg.MarketStats.OpenInterest); ok {
			pairData.OpenInterest = openInterest
		}
		if fundingRate, ok := parseFloat(msg.MarketStats.CurrentFundingRate); ok {
			pairData.FundingRate = fundingRate * 100
		}
		if msg.MarketStats.DailyQuoteTokenVolume != nil {
			pairData.Volume = *msg.MarketStats.DailyQuoteTokenVolume
		}
	}
	e.Unlock()

	if pairData != nil {
		select {
		case e.Notifications.Pair <- exchange.ExchangeUpdate[*exchange.Pair]{Pair: pair, Type: "pair", Data: pairData}:
		default:
		}
		e.notifyUpdate(pair, "pair", pairData)
	}
	return nil
}

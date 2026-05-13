package exchange

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"ticktrader/config"
)

type Exchange struct {
	*config.ExchangeConfig
	// Assets stores exchange assets it can hold
	// it should automaticly populate on Start()
	Assets map[string]*Asset
	// Pairs stores exchange pairs it can trade
	// it should automaticly populate on Start()
	Pairs map[string]*Pair
	// Orders stores exchange orders for for each pair
	// it should automaticly populate after opening orders
	Orders map[string]*Order
	// Positions store exchange positions for each asset
	// it should automaticly populate when in position
	Positions map[string]*Position
	// Orderbook builds and stores orderbook for each pair
	// it is automaticly built when subsribing to an orderbooks
	Orderbook map[string]*Orderbook
	// Notification is notified for type-safe updates
	Notifications *NotificationChannels

	// Account-level fields for perpetual trading
	AccountValue     atomic.Value // Total account value in USD (atomic)
	AvailableBalance atomic.Value // Available balance for trading in USD (atomic)
	FreeMargin       atomic.Value // Available margin for new positions (atomic)
	// Runnnig should be set to true after Start()
	Runnning bool
	sync.RWMutex
}

// NewExchange creates a new Exchange instance with initialized fields
func NewExchange(cfg *config.ExchangeConfig) Exchange {
	return Exchange{
		ExchangeConfig: cfg,
		Assets:         make(map[string]*Asset),
		Pairs:          make(map[string]*Pair),
		Orders:         make(map[string]*Order),
		Positions:      make(map[string]*Position),
		Orderbook:      make(map[string]*Orderbook),

		Notifications: &NotificationChannels{
			Pair:       make(chan ExchangeUpdate[*Pair], 1000),
			Positions:  make(chan ExchangeUpdate[*Position], 1000),
			Orders:     make(chan ExchangeUpdate[*Order], 1000),
			Prices:     make(chan ExchangeUpdate[*[]Price], 1000),
			Trades:     make(chan ExchangeUpdate[*Trade], 1000),
			Orderbooks: make(chan ExchangeUpdate[*Orderbook], 1000),
			Bars:       make(chan ExchangeUpdate[*Bar], 1000),
			Updates:    make(chan ExchangeUpdate[any], 1000),
		},
	}
}

// I is the strategy-facing exchange adapter contract.
//
// Venue packages must implement this interface by translating the common
// marketmaker types in exchange/types.go to the venue's native API. Strategies
// and models should depend only on this interface, not on concrete exchange
// SDKs or venue packages.
//
// Adapter implementation rules:
//   - Embed *Exchange from NewExchange(cfg) and use its maps/channels as the
//     shared local state cache.
//   - Start(ctx) should open required clients, load static metadata, and prepare
//     websocket connectivity. Stop() must close websocket connections and cancel
//     background goroutines.
//   - Getter methods such as GetPairs, GetOrders, GetPosition, and GetOrderbook
//     should read from the local Exchange state, not make network calls.
//   - Request methods such as GetBars, PlaceOrders, ModifyOrders, and
//     CancelOrders should prefer the venue's websocket command/API path when one
//     exists. If the venue has no websocket command for that action, a REST
//     fallback is allowed.
//   - Request methods that mutate state should update local Exchange state and
//     publish typed notifications when the venue confirms the change, or track a
//     clear temporary state until confirmation arrives.
//
// Subscription rules:
//   - SubscribePair, SubscribePrices, SubscribeTrades, and SubscribeOrderbook
//     must be real websocket subscriptions. They must not be implemented with
//     REST polling.
//   - If a venue does not provide websocket data for one of those Subscribe*
//     methods, the adapter must return a clear error instead of silently falling
//     back to REST or pretending the subscription exists.
//   - SubscribeBars is the only subscription allowed to use REST polling, and
//     only when the venue does not provide websocket bars/klines/candles.
//   - Subscription handlers must update local state first, then publish to the
//     matching typed channel in Notifications using non-blocking sends.
//
// Pair and data mapping rules:
//   - Pair names exposed to strategies are normalized symbols such as BTC, ETH,
//     and SOL. Keep venue-specific market IDs/symbols inside the adapter.
//   - Prices notifications should publish [bestBid, bestAsk].
//   - Orderbook notifications should publish snapshots or updates with bids
//     sorted descending and asks sorted ascending.
//   - Trades, positions, orders, pair stats, and bars should use the shared
//     exchange types and preserve venue errors when data cannot be mapped safely.
type I interface {
	// Exchange Initialization
	Name() string                    // returns the name of the exchange
	Enabled() bool                   // returns if the exchange is enabled
	Start(ctx context.Context) error // USER IMPLEMENTATION: starts the exchange
	Stop() error                     // USER IMPLEMENTATION: stops the exchange
	// Exchange assets
	GetAssets() map[string]Asset    // returns all assets on the exchange
	Pair(pair string) (Pair, error) // returns a specific pair by name
	GetPairs() map[string]Pair      // returns all pairs on the exchange
	// Exchange states
	GetOrder(pair string) ([]Order, error)       // returns orders for a pair
	GetOrders() map[string]Order                 // returns all orders
	GetPosition(pair string) (Position, error)   // returns position for a pair
	GetPositions() map[string]Position           // returns all positions
	GetOrderbook(pair string) (Orderbook, error) // returns orderbook for a pair
	GetOrderbooks() map[string]Orderbook         // returns all orderbooks
	// Exchange requests
	GetBars(pair, interval string, length int) ([]Bar, error) // USER IMPLEMENTATION: returns historical bars
	PlaceOrders([]Order) error                                // USER IMPLEMENTATION: places new orders
	ModifyOrders([]Order) error                               // USER IMPLEMENTATION: modifies existing orders
	CancelOrders([]Order) error                               // USER IMPLEMENTATION: cancels orders
	// All Subscribe* methods except SubscribeBars must be backed by websocket streams.
	// If a venue does not support a websocket stream for the requested data, return an error.
	// SubscribeBars may use REST polling only when the venue has no websocket candle stream.
	SubscribePair(pair string) error           // USER IMPLEMENTATION: websocket pair updates
	SubscribePrices(pair string) error         // USER IMPLEMENTATION: websocket prices updates
	SubscribeTrades(pair string) error         // USER IMPLEMENTATION: websocket trade updates
	SubscribeOrderbook(pair string) error      // USER IMPLEMENTATION: websocket orderbook updates
	SubscribeBars(pair, interval string) error // USER IMPLEMENTATION: websocket bars, or REST polling if unsupported
	// Unsubscribe from websocket events
	UnsubscribePair(pair string) error           // USER IMPLEMENTATION: unsubscribe from pair updates
	UnsubscribePrices(pair string) error         // USER IMPLEMENTATION: unsubscribe from prices updates
	UnsubscribeTrades(pair string) error         // USER IMPLEMENTATION: unsubscribe from trade updates
	UnsubscribeOrderbook(pair string) error      // USER IMPLEMENTATION: unsubscribe from orderbook updates
	UnsubscribeBars(pair, interval string) error // USER IMPLEMENTATION: unsubscribe from bar updates
	// Typed notification channels
	GetUpdates() <-chan ExchangeUpdate[any]                 // notifier on any update (backward compatibility)
	GetPairUpdates() <-chan ExchangeUpdate[*Pair]           // notifier on pair updates
	GetOrderUpdates() <-chan ExchangeUpdate[*Order]         // notifier on order updates
	GetPositionUpdates() <-chan ExchangeUpdate[*Position]   // notifier on position updates
	GetPricesUpdates() <-chan ExchangeUpdate[*[]Price]      // notifier on prices updates
	GetTradeUpdates() <-chan ExchangeUpdate[*Trade]         // notifier on trade updates
	GetOrderbookUpdates() <-chan ExchangeUpdate[*Orderbook] // notifier on orderbook updates
	GetBarUpdates() <-chan ExchangeUpdate[*Bar]             // notifier on bar updates
	// Get latency
	GetLatency() int64 // USER IMPLEMENTATION: returns the websocket latency in milliseconds
}

func (e *Exchange) Name() string {
	e.RLock()
	defer e.RUnlock()

	return e.ExchangeConfig.Name
}

// Enabled is a method that returns if the current exchange is enabled
func (e *Exchange) Enabled() bool {
	e.RLock()
	defer e.RUnlock()

	return e.Runnning
}

// GetAssets returns exchange assets
func (e *Exchange) GetAssets() map[string]Asset {
	e.RLock()
	defer e.RUnlock()

	assets := make(map[string]Asset)
	for k, v := range e.Assets {
		assets[k] = *v
	}
	return assets
}

// Pair returns the correctly formatted pair from a universal argument
func (e *Exchange) Pair(pair string) (Pair, error) {
	e.RLock()
	defer e.RUnlock()

	if pd := e.Pairs[pair]; pd != nil && pd.Name != "" {
		return *pd, nil
	}
	return Pair{}, fmt.Errorf("%s not found", pair)
}

// GetPairs returns exchange pairs
func (e *Exchange) GetPairs() map[string]Pair {
	e.RLock()
	defer e.RUnlock()

	pairs := make(map[string]Pair)
	for pair, pd := range e.Pairs {
		pairs[pair] = *pd
	}
	return pairs
}

func (e *Exchange) GetOrder(pair string) ([]Order, error) {
	e.RLock()
	defer e.RUnlock()

	var orders []Order
	for _, order := range e.Orders {
		if order.Pair == pair {
			orders = append(orders, *order)
		}
	}

	if len(orders) == 0 {
		return orders, fmt.Errorf("no %s orders found", pair)
	}

	return orders, nil
}

// GetOrders returns exchange orders
func (e *Exchange) GetOrders() map[string]Order {
	e.RLock()
	defer e.RUnlock()

	orders := make(map[string]Order, len(e.Orders))
	for id, order := range e.Orders {
		orders[id] = *order
	}
	return orders
}

// GetPosition returns exchange position
func (e *Exchange) GetPosition(pair string) (Position, error) {
	e.RLock()
	defer e.RUnlock()

	if pos, exists := e.Positions[pair]; exists {
		return *pos, nil
	}

	return Position{}, fmt.Errorf("position %v does not exist", pair)
}

// GetPositions returns exchange positions
func (e *Exchange) GetPositions() map[string]Position {
	e.RLock()
	defer e.RUnlock()

	positions := make(map[string]Position)
	for pair, pos := range e.Positions {
		positions[pair] = *pos
	}
	return positions
}

// GetOrderbook returns orderbook for pair
func (e *Exchange) GetOrderbook(pair string) (Orderbook, error) {
	e.RLock()
	defer e.RUnlock()

	if ob, exists := e.Orderbook[pair]; exists {
		return ob.Clone(), nil
	}

	return Orderbook{}, fmt.Errorf("orderbook %s not found", pair)
}

// GetOrderbooks returns exchange orderbooks
func (e *Exchange) GetOrderbooks() map[string]Orderbook {
	e.RLock()
	defer e.RUnlock()

	orderbooks := make(map[string]Orderbook)
	for pair, ob := range e.Orderbook {
		orderbooks[pair] = ob.Clone()
	}
	return orderbooks
}

// GetPairUpdates returns the typed pair info updates channel
func (e *Exchange) GetPairUpdates() <-chan ExchangeUpdate[*Pair] {
	return e.Notifications.Pair
}

// GetPositionUpdates returns the typed position updates channel
func (e *Exchange) GetPositionUpdates() <-chan ExchangeUpdate[*Position] {
	return e.Notifications.Positions
}

// GetOrderUpdates returns the typed order updates channel
func (e *Exchange) GetOrderUpdates() <-chan ExchangeUpdate[*Order] {
	return e.Notifications.Orders
}

// GetPricesUpdates returns the typed prices updates channel
func (e *Exchange) GetPricesUpdates() <-chan ExchangeUpdate[*[]Price] {
	return e.Notifications.Prices
}

// GetTradeUpdates returns the typed trade updates channel
func (e *Exchange) GetTradeUpdates() <-chan ExchangeUpdate[*Trade] {
	return e.Notifications.Trades
}

// GetOrderbookUpdates returns the typed orderbook updates channel
func (e *Exchange) GetOrderbookUpdates() <-chan ExchangeUpdate[*Orderbook] {
	return e.Notifications.Orderbooks
}

// GetBarUpdates returns the typed bar updates channel
func (e *Exchange) GetBarUpdates() <-chan ExchangeUpdate[*Bar] {
	return e.Notifications.Bars
}

// GetUpdates returns the notification channel (backward compatibility)
func (e *Exchange) GetUpdates() <-chan ExchangeUpdate[any] {
	return e.Notifications.Updates
}

// GetAccountValue returns the current account value as float64
func (e *Exchange) GetAccountValue() float64 {
	if value := e.AccountValue.Load(); value != nil {
		return value.(float64)
	}
	return 0.0
}

// GetAvailableBalance returns the current available balance as float64
func (e *Exchange) GetAvailableBalance() float64 {
	if value := e.AvailableBalance.Load(); value != nil {
		return value.(float64)
	}
	return 0.0
}

// GetFreeMargin returns the current free margin as float64
func (e *Exchange) GetFreeMargin() float64 {
	if value := e.FreeMargin.Load(); value != nil {
		return value.(float64)
	}
	return 0.0
}

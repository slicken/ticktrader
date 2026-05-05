# trader-mux

## Exchange Adapter

This repo is built around the exchange adapter pattern.

- The model talks only to the shared interface: `exchange.I`.
- Each exchange implements an adapter in `exchange/<name>/`.
- Adapters map exchange-specific APIs/ws data into shared types (`Order`, `Trade`, `Price`, `Position`, `Pair`).
- To add a new exchange, implement `exchange.I` in a new adapter and register it in `main.go`.

Currently supports:
- Hyperliquid
- Lighter

This keeps strategy/model logic the same while swapping exchanges through one interface.

## Implementing a New Exchange Adapter

Create `exchange/<your-exchange>/` and implement `exchange.I`.

### 1) Structure

- Create a struct that embeds `*exchange.Exchange` from `exchange.NewExchange(cfg)`.
- Keep venue-specific clients, symbol maps, subscriptions, and auth fields in this struct.

### 2) What You Must Implement vs What Is Automatic

If your adapter struct embeds `*exchange.Exchange`, many `exchange.I` methods are already satisfied automatically by promoted methods.

**You must implement (exchange-specific logic):**
- `Start(ctx context.Context) error`
- `Stop() error`
- `GetBars(pair, interval string, length int) ([]exchange.Bar, error)`
- `PlaceOrders([]exchange.Order) error`
- `ModifyOrders([]exchange.Order) error`
- `CancelOrders([]exchange.Order) error`
- `SubscribePair(pair string) error`
- `SubscribePrices(pair string) error`
- `SubscribeTrades(pair string) error`
- `SubscribeOrderbook(pair string) error`
- `SubscribeBars(pair, interval string) error`
- `UnsubscribePair(pair string) error`
- `UnsubscribePrices(pair string) error`
- `UnsubscribeTrades(pair string) error`
- `UnsubscribeOrderbook(pair string) error`
- `UnsubscribeBars(pair, interval string) error`
- `GetLatency() int64`

**Usually automatic from embedded `*exchange.Exchange`:**
- `Name()`, `Enabled()`
- `GetAssets()`, `Pair()`, `GetPairs()`
- `GetOrder()`, `GetOrders()`
- `GetPosition()`, `GetPositions()`
- `GetOrderbook()`, `GetOrderbooks()`
- `GetUpdates()`, `GetPairUpdates()`, `GetOrderUpdates()`
- `GetPositionUpdates()`, `GetPricesUpdates()`, `GetTradeUpdates()`
- `GetOrderbookUpdates()`, `GetBarUpdates()`

You still need to keep the underlying maps/channels updated inside your own Start/WS handlers/trading methods.

### 3) Adapter Rules

- Keep pair names normalized for strategy (`BTC`, `ETH`, etc.); convert venue symbols inside adapter.
- Publish prices as `[bestBid, bestAsk]`.
- Keep orderbook sorted: bids desc, asks asc.
- Update local state first, then publish typed notifications.
- Use non-blocking channel sends (`select { case ch <- v: default: }`).
- `SubscribePair/Prices/Trades/Orderbook` must be websocket-backed.
- `SubscribeBars` can use REST polling only if websocket bars are unavailable.

### 4) Register in `main.go`

Add your adapter in `loadExchange`:
- map config name -> constructor
- return your `exchange.I` implementation

## License

See `LICENSE`.

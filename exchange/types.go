package exchange

import (
	"time"
)

type Asset struct {
	Name         string
	Decimal      int
	MinOrderSize float64
	MaxOrderSize float64
	TickSize     float64
	StepSize     float64
	MaxLeverage  float64
	OnlyIsolated bool
}

type Pair struct {
	Name            string
	Price           float64
	MarkPrice       float64
	IndexPrice      float64
	PremiumPct      float64
	FundingRate     float64
	NextFundingTime time.Time
	FundingInterval time.Duration // Usually 8 hours for most exchanges
	Base            Asset
	Quote           Asset
	Enabled         bool
	IsPerp          bool    // True for perpetual futures, false for spot
	MaxLeverage     int     // Maximum leverage allowed for this asset
	OpenInterest    float64 // Open interest in USD
	Volume          float64 // 24h volume in USD
}

type Order struct {
	ID           string
	Pair         string
	Price        float64
	Size         float64 // Always positive
	Side         string  // "buy" or "sell"
	Status       string  // "open", "filled", "canceled"
	Type         string  // "market" or "limit"
	TriggerPrice float64 // if not nil = activated
	TriggerType  string  // "market" or "limit"
	ReduceOnly   bool
	PostOnly     bool
	StopLoss     bool // Enable stop loss grouping with parent order
	TakeProfit   bool // Enable take profit grouping with parent order
	Time         time.Time
}

type Fill struct {
	Pair  string
	Side  string
	Price float64
	Size  float64
	Time  time.Time
}

type Trade struct {
	Order *Order
	Fills []*Fill
}

type Position struct {
	Pair     string
	Size     float64 // Negative for short, positive for long
	AvgPrice float64
	PNL      float64
}

type Orderbook struct {
	Pair        string
	Bids        []Price
	Asks        []Price
	LastUpdated time.Time
}

type Price struct {
	Price float64
	Size  float64
	Time  time.Time
}

type Bar struct {
	Time   time.Time
	Open   float64
	High   float64
	Low    float64
	Close  float64
	Volume float64
}

type ExchangeUpdate[T any] struct {
	Pair string
	Type string // "orderbook", "order", "trade", etc.
	Data T
}

// Type-safe notification channels
type NotificationChannels struct {
	Pair       chan ExchangeUpdate[*Pair]
	Positions  chan ExchangeUpdate[*Position]
	Orders     chan ExchangeUpdate[*Order]
	Prices     chan ExchangeUpdate[*[]Price]
	Trades     chan ExchangeUpdate[*Trade]
	Orderbooks chan ExchangeUpdate[*Orderbook]
	Bars       chan ExchangeUpdate[*Bar]
	// Generic channel for backward compatibility
	Updates chan ExchangeUpdate[any]
}

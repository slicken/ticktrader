package exchange

import (
	"sort"
	"time"
)

const max_depth = 500

// NewOrderbook initalizes price orderbook
func NewOrderbook(pair string) *Orderbook {
	return &Orderbook{
		Pair: pair,
		Bids: make([]Price, 0, max_depth),
		Asks: make([]Price, 0, max_depth),
	}
}

func (o *Orderbook) Sort() {
	sort.SliceStable(o.Bids, func(i, j int) bool {
		return o.Bids[i].Price > o.Bids[j].Price
	})
	sort.SliceStable(o.Asks, func(i, j int) bool {
		return o.Asks[i].Price < o.Asks[j].Price
	})
}

func (o *Orderbook) Clone() Orderbook {
	return Orderbook{
		Pair:        o.Pair,
		Bids:        append([]Price(nil), o.Bids...),
		Asks:        append([]Price(nil), o.Asks...),
		LastUpdated: o.LastUpdated,
	}
}

func (o *Orderbook) AddBid(price Price) {
	if price.Price <= 0 {
		return
	}
	idx := sort.Search(len(o.Bids), func(i int) bool {
		return o.Bids[i].Price <= price.Price
	})

	if price.Size == 0 {
		if idx < len(o.Bids) && o.Bids[idx].Price == price.Price {
			copy(o.Bids[idx:], o.Bids[idx+1:])
			o.Bids = o.Bids[:len(o.Bids)-1]
			o.LastUpdated = time.Now()
		}
		return
	}

	if idx < len(o.Bids) && o.Bids[idx].Price == price.Price {
		o.Bids[idx] = price
		o.LastUpdated = time.Now()
		return
	}

	o.Bids = append(o.Bids, Price{}) // grow slice
	copy(o.Bids[idx+1:], o.Bids[idx:])
	o.Bids[idx] = price
	if len(o.Bids) > max_depth {
		o.Bids = o.Bids[:max_depth]
	}
	o.LastUpdated = time.Now()
}

func (o *Orderbook) AddAsk(price Price) {
	if price.Price <= 0 {
		return
	}
	idx := sort.Search(len(o.Asks), func(i int) bool {
		return o.Asks[i].Price >= price.Price
	})

	if price.Size == 0 {
		if idx < len(o.Asks) && o.Asks[idx].Price == price.Price {
			copy(o.Asks[idx:], o.Asks[idx+1:])
			o.Asks = o.Asks[:len(o.Asks)-1]
			o.LastUpdated = time.Now()
		}
		return
	}

	if idx < len(o.Asks) && o.Asks[idx].Price == price.Price {
		o.Asks[idx] = price
		o.LastUpdated = time.Now()
		return
	}

	o.Asks = append(o.Asks, Price{}) // grow slice
	copy(o.Asks[idx+1:], o.Asks[idx:])
	o.Asks[idx] = price
	if len(o.Asks) > max_depth {
		o.Asks = o.Asks[:max_depth]
	}
	o.LastUpdated = time.Now()
}

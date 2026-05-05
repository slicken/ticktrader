package exchange

import (
	"sort"
	"time"
)

const max_depth = 100

// NewOrderbook initalizes price orderbook
func NewOrderbook(pair string) *Orderbook {
	return &Orderbook{
		Pair: pair,
		Bids: make([]Price, 0, max_depth),
		Asks: make([]Price, 0, max_depth),
	}
}

// Sort asks bids in ascending and descending order respectively
func (o *Orderbook) Sort() {
	// sort asks in ascending order
	sort.SliceStable(o.Asks, func(i, j int) bool {
		return o.Asks[i].Price < o.Asks[j].Price
	})
	// sort bids in descending order
	sort.SliceStable(o.Bids, func(i, j int) bool {
		return o.Bids[i].Price > o.Bids[j].Price
	})
}

func (o *Orderbook) AddAsk(price Price) {
	// delete price if size is 0
	if price.Size == 0 {
		for i, v := range o.Asks {
			if price.Price == v.Price {
				copy(o.Asks[i:], o.Asks[i+1:])
				o.Asks = o.Asks[:len(o.Asks)-1]
				o.LastUpdated = time.Now()
				return
			}
		}
		return
	}
	if len(o.Asks) >= max_depth {
		o.Asks = o.Asks[1:]
	}
	// Insert in sorted order (ascending)
	idx := sort.Search(len(o.Asks), func(i int) bool {
		return o.Asks[i].Price >= price.Price
	})
	o.Asks = append(o.Asks, Price{}) // grow slice
	copy(o.Asks[idx+1:], o.Asks[idx:])
	o.Asks[idx] = price
	o.LastUpdated = time.Now()
}

func (o *Orderbook) AddBid(price Price) {
	// delete price if size is 0
	if price.Size == 0 {
		for i, v := range o.Bids {
			if price.Price == v.Price {
				copy(o.Bids[i:], o.Bids[i+1:])
				o.Bids = o.Bids[:len(o.Bids)-1]
				o.LastUpdated = time.Now()
				return
			}
		}
		return
	}
	if len(o.Bids) >= max_depth {
		o.Bids = o.Bids[1:]
	}
	// Insert in sorted order (descending)
	idx := sort.Search(len(o.Bids), func(i int) bool {
		return o.Bids[i].Price <= price.Price
	})
	o.Bids = append(o.Bids, Price{}) // grow slice
	copy(o.Bids[idx+1:], o.Bids[idx:])
	o.Bids[idx] = price
	o.LastUpdated = time.Now()
}

package lighter

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const lighterCodeOK = 200

type lighterResultCode struct {
	Code    int32  `json:"code"`
	Message string `json:"message,omitempty"`
}

type lighterOrderBookResponse struct {
	lighterResultCode
	OrderBooks []lighterOrderBook `json:"order_books,omitempty"`
}

type lighterOrderBook struct {
	Symbol                 string `json:"symbol"`
	MarketID               int    `json:"market_id"`
	Status                 string `json:"status"`
	MinBaseAmount          string `json:"min_base_amount"`
	SupportedSizeDecimals  uint8  `json:"supported_size_decimals"`
	SupportedPriceDecimals uint8  `json:"supported_price_decimals"`
	SupportedQuoteDecimals uint8  `json:"supported_quote_decimals"`
}

type lighterOrderBookDetailsResponse struct {
	lighterResultCode
	OrderBookDetails []lighterOrderBookDetail `json:"order_book_details"`
}

type lighterOrderBookDetail struct {
	Symbol                   string  `json:"symbol"`
	MarketID                 int     `json:"market_id"`
	Status                   string  `json:"status"`
	MinBaseAmount            string  `json:"min_base_amount"`
	SupportedSizeDecimals    uint8   `json:"supported_size_decimals"`
	SupportedPriceDecimals   uint8   `json:"supported_price_decimals"`
	SupportedQuoteDecimals   uint8   `json:"supported_quote_decimals"`
	SizeDecimals             uint8   `json:"size_decimals"`
	PriceDecimals            uint8   `json:"price_decimals"`
	MinInitialMarginFraction uint32  `json:"min_initial_margin_fraction"`
	LastTradePrice           float64 `json:"last_trade_price"`
	DailyQuoteTokenVolume    float64 `json:"daily_quote_token_volume"`
	OpenInterest             float64 `json:"open_interest"`
}

type lighterOrdersResponse struct {
	lighterResultCode
	Orders []lighterOrder `json:"orders,omitempty"`
}

type lighterOrder struct {
	OrderIndex          int64  `json:"order_index,omitempty"`
	ClientOrderIndex    int64  `json:"client_order_index,omitempty"`
	OrderID             string `json:"order_id,omitempty"`
	ClientOrderID       string `json:"client_order_id,omitempty"`
	MarketIndex         int    `json:"market_index,omitempty"`
	InitialBaseAmount   string `json:"initial_base_amount,omitempty"`
	RemainingBaseAmount string `json:"remaining_base_amount,omitempty"`
	Price               string `json:"price,omitempty"`
	IsAsk               bool   `json:"is_ask,omitempty"`
	Side                string `json:"side,omitempty"`
	Type                string `json:"type,omitempty"`
	Status              string `json:"status,omitempty"`
	Timestamp           int64  `json:"timestamp,omitempty"`
	ReduceOnly          bool   `json:"reduce_only,omitempty"`
}

type lighterAccountResponse struct {
	lighterResultCode
	Accounts []lighterAccount `json:"accounts"`
}

type lighterAccount struct {
	Index            int64             `json:"index"`
	AvailableBalance string            `json:"available_balance"`
	Positions        []lighterPosition `json:"positions"`
	TotalAssetValue  string            `json:"total_asset_value"`
}

type lighterPosition struct {
	MarketID      int    `json:"market_id"`
	Symbol        string `json:"symbol"`
	Sign          int    `json:"sign"`
	Position      string `json:"position"`
	AvgEntryPrice string `json:"avg_entry_price"`
	UnrealizedPNL string `json:"unrealized_pnl"`
}

type lighterCandlesResponse struct {
	lighterResultCode
	Resolution string          `json:"r"`
	Candles    []lighterCandle `json:"c"`
}

type lighterCandle struct {
	Timestamp int64   `json:"t"`
	Open      float64 `json:"o"`
	High      float64 `json:"h"`
	Low       float64 `json:"l"`
	Close     float64 `json:"c"`
	Volume    float64 `json:"v"`
}

type lighterTrade struct {
	TradeID    int64  `json:"trade_id"`
	Price      string `json:"price"`
	Size       string `json:"size"`
	Side       string `json:"side"`
	IsMakerAsk bool   `json:"is_maker_ask"`
	Timestamp  int64  `json:"timestamp"`
}

func (e *Lighter) getOrderBooks() (*lighterOrderBookResponse, error) {
	result := &lighterOrderBookResponse{}
	if err := e.getLighter("api/v1/orderBooks", nil, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Lighter) getOrderBookDetails(marketID int) (*lighterOrderBookDetailsResponse, error) {
	params := map[string]any{}
	if marketID > 0 {
		params["market_id"] = marketID
	}
	result := &lighterOrderBookDetailsResponse{}
	if err := e.getLighter("api/v1/orderBookDetails", params, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Lighter) getActiveOrders(accountIndex int64, marketID int, auth string) (*lighterOrdersResponse, error) {
	result := &lighterOrdersResponse{}
	if err := e.getLighter("api/v1/orders", map[string]any{
		"account_index": accountIndex,
		"market_id":     marketID,
		"auth":          auth,
	}, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Lighter) getAccount(accountIndex int64) (*lighterAccountResponse, error) {
	result := &lighterAccountResponse{}
	if err := e.getLighter("api/v1/account", map[string]any{
		"by":    "index",
		"value": accountIndex,
	}, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Lighter) getCandles(marketID int, resolution string, start, end int64, countBack int32) (*lighterCandlesResponse, error) {
	result := &lighterCandlesResponse{}
	if err := e.getLighter("api/v1/candles", map[string]any{
		"market_id":       marketID,
		"resolution":      resolution,
		"start_timestamp": start,
		"end_timestamp":   end,
		"count_back":      countBack,
	}, result); err != nil {
		return nil, err
	}
	return result, nil
}

func (e *Lighter) getLighter(path string, params map[string]any, result any) error {
	u, err := url.Parse(e.baseURL)
	if err != nil {
		return err
	}
	u.Path = path

	q := u.Query()
	for key, value := range params {
		if value == nil {
			continue
		}
		q.Set(key, fmt.Sprintf("%v", value))
	}
	u.RawQuery = q.Encode()

	resp, err := http.Get(u.String())
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("lighter %s http status %d: %s", path, resp.StatusCode, string(body))
	}

	status := &lighterResultCode{}
	if err := json.Unmarshal(body, status); err == nil && status.Code != 0 && status.Code != lighterCodeOK {
		return fmt.Errorf("lighter %s result code %d: message=%q body=%s", path, status.Code, status.Message, string(body))
	}
	return json.Unmarshal(body, result)
}

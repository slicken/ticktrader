package exchange

import (
	"math"
)

// CalculateUnrealizedPNL calculates unrealized PNL using the formula:
// side * (mark_price - entry_price) * position_size
// where side = 1 for long positions, -1 for short positions
func (p Position) CalculateUnrealizedPNL(markPrice float64) float64 {
	if p.Size == 0 {
		return 0
	}

	// Determine side: 1 for long (positive size), -1 for short (negative size)
	var side float64
	if p.Size > 0 {
		side = 1
	} else {
		side = -1
	}

	// Calculate unrealized PNL
	return side * (markPrice - p.AvgPrice) * math.Abs(p.Size)
}

// calculatePnLPercentage calculates the percentage gain/loss for a position based on margin used
// This matches Hyperliquid's display format where percentage is relative to margin (capital at risk)
func (p Position) CalculatePNLPercentage(pairData Pair) float64 {
	if p.Size == 0 {
		return 0
	}

	pnl := p.CalculateUnrealizedPNL(pairData.MarkPrice)

	// Calculate margin: Position Value / Leverage using the pair's actual leverage
	positionValue := math.Abs(p.Size) * pairData.MarkPrice
	leverage := float64(pairData.MaxLeverage)
	if leverage <= 0 {
		leverage = 3.0 // Fallback to default 3x leverage for Hyperliquid
	}
	margin := positionValue / leverage

	if margin == 0 {
		return 0
	}

	return (pnl / margin) * 100
}

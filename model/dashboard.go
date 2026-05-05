package model

import (
	"context"
	"encoding/json"
	"html/template"
	"log"
	"net/http"
	"sort"
	"sync"
	"time"
	"trader-mux/exchange"
)

const (
	DASHBOARD_CHART_WINDOW     = 30 * time.Second
	DASHBOARD_REFRESH_INTERVAL = 1000 * time.Millisecond
)

type Dashboard struct {
	model     *Marketmaker
	addr      string
	server    *http.Server
	historyMu sync.Mutex
	histories map[string]*dashboardHistory
}

type dashboardHistory struct {
	// PairedQuotes keeps each bid with the ask from the same exchange update.
	// Merging bids and asks in separate slices breaks index alignment and makes charts look spiky.
	PairedQuotes [][2]exchange.Price
	MarkPrices   []exchange.Price
	SMA20Prices  []exchange.Price
	Trades       []exchange.Price
}

type DashboardData struct {
	PairsData          []DashboardPairData `json:"pairs_data"`
	LastUpdate         time.Time           `json:"last_update"`
	LatencyMs          map[string]int64    `json:"latency_ms"`
	ChartWindowSeconds int                 `json:"chart_window_seconds"`
}

type dashboardTemplateData struct {
	ChartWindowSeconds int
	RefreshMs          int
}

type DashboardPairData struct {
	Exchange        string           `json:"exchange"`
	Symbol          string           `json:"symbol"`
	MarkPrice       float64          `json:"mark_price"`
	MidPrice        float64          `json:"mid_price"`
	LastTradePrice  float64          `json:"last_trade_price"`
	SpreadAvg       float64          `json:"spread_avg"`
	SlippageAvg     float64          `json:"slippage_avg"`
	TradesPerMinute int              `json:"trades_per_minute"`
	OpenInterest    float64          `json:"open_interest"`
	FundingRate     float64          `json:"funding_rate"`
	SMA20           float64          `json:"sma20"`
	SMA20SlopePct   float64          `json:"sma20_slope_pct"`
	BidPrices       []exchange.Price `json:"bid_prices"`
	AskPrices       []exchange.Price `json:"ask_prices"`
	MarkPrices      []exchange.Price `json:"mark_prices"`
	SMA20Prices     []exchange.Price `json:"sma20_prices"`
	Trades          []exchange.Price `json:"trades"`
}

func NewDashboard(model *Marketmaker, addr string) *Dashboard {
	return &Dashboard{
		model:     model,
		addr:      addr,
		histories: make(map[string]*dashboardHistory),
	}
}

func (d *Dashboard) Start(ctx context.Context) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", d.dashboardHandler)
	mux.HandleFunc("/api/data", d.apiHandler)

	d.server = &http.Server{
		Addr:    d.addr,
		Handler: mux,
	}

	go func() {
		if err := d.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Dashboard failed: %v", err)
		}
	}()

	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := d.server.Shutdown(shutdownCtx); err != nil {
		log.Printf("Dashboard shutdown failed: %v", err)
	}
}

func (d *Dashboard) dashboardHandler(w http.ResponseWriter, r *http.Request) {
	tmpl := `
<!DOCTYPE html>
<html>
<head>
	<title>Trader Mux Dashboard</title>
	<meta charset="utf-8">
	<script src="https://cdn.jsdelivr.net/npm/chart.js"></script>
	<style>
		body {
			font-family: 'Courier New', 'Monaco', 'Menlo', 'Consolas', monospace;
			margin: 0;
			padding: 12px;
			background-color: #1a1a1a;
			color: #ffffff;
			overflow: hidden;
		}
		.meta {
			color: #9ca3af;
			margin-bottom: 8px;
			font-size: 12px;
		}
		.pair-grid {
			display: grid;
			gap: 8px;
			height: calc(100vh - 44px);
			min-height: 0;
			--metric-scale: 1;
			--metric-grid-columns: repeat(auto-fit, minmax(112px, 1fr));
			--metrics-column: 34%;
		}
		.pair-section {
			display: grid;
			grid-template-columns: minmax(420px, var(--metrics-column)) minmax(0, 1fr);
			gap: 8px;
			background: #1a1a1a;
			border-radius: 8px;
			padding: clamp(8px, calc(8px * var(--metric-scale)), 14px);
			min-height: 0;
			overflow: hidden;
		}
		.metrics-panel {
			display: flex;
			flex-direction: column;
			gap: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-width: 0;
			min-height: 0;
			overflow: auto;
		}
		.pair-title {
			display: flex;
			align-items: baseline;
			justify-content: space-between;
			gap: 10px;
		}
		.symbol {
			font-size: clamp(13px, calc(14px * var(--metric-scale)), 30px);
			font-weight: 700;
		}
		.metric-grid {
			display: grid;
			grid-template-columns: var(--metric-grid-columns);
			gap: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-height: 0;
		}
		.metric {
			background: #2a2a2a;
			border-radius: 6px;
			padding: clamp(6px, calc(6px * var(--metric-scale)), 12px);
			min-width: 0;
		}
		.metric-label {
			color: #9ca3af;
			font-size: clamp(8px, calc(8px * var(--metric-scale)), 15px);
			text-transform: uppercase;
			margin-bottom: clamp(2px, calc(2px * var(--metric-scale)), 5px);
		}
		.metric-value {
			font-size: clamp(11px, calc(12px * var(--metric-scale)), 28px);
			font-weight: 700;
			overflow: hidden;
			text-overflow: ellipsis;
			white-space: nowrap;
		}
		.chart-panel {
			display: flex;
			flex-direction: column;
			min-width: 0;
			min-height: 0;
		}
		.chart-wrap {
			position: relative;
			flex: 1;
			min-height: 0;
		}
		.chart-points {
			position: absolute;
			top: 6px;
			right: 8px;
			z-index: 2;
			color: #9ca3af;
			font-size: clamp(9px, calc(10px * var(--metric-scale)), 12px);
			background: rgba(26, 26, 26, 0.55);
			border: 1px solid rgba(156, 163, 175, 0.18);
			border-radius: 6px;
			padding: 3px 7px;
			pointer-events: none;
			white-space: nowrap;
		}
		.positive { color: #4ade80; }
		.negative { color: #f87171; }
		.neutral { color: #d1d5db; }
		@media (max-width: 1100px) {
			.pair-grid {
				height: auto;
				overflow: visible;
			}
			body { overflow: auto; }
			.pair-section {
				grid-template-columns: 1fr;
				min-height: 420px;
			}
			.chart-wrap {
				min-height: 260px;
			}
		}
	</style>
</head>
<body>
	<div class="meta" id="meta">Connecting...</div>
	<div class="pair-grid" id="pair-grid"></div>

	<script>
		const fmt = (value, digits = 6) => Number(value || 0).toFixed(digits);
		const pct = (value, digits = 4) => fmt(value, digits) + '%';
		const cls = value => value > 0 ? 'positive' : value < 0 ? 'negative' : 'neutral';
		const DASHBOARD_CHART_WINDOW_SECONDS = {{ .ChartWindowSeconds }};
		const DASHBOARD_REFRESH_MS = {{ .RefreshMs }};
		let chartWindowSeconds = DASHBOARD_CHART_WINDOW_SECONDS;
		let charts = {};
		let renderedKeys = [];

		function rowKey(row) {
			return row.exchange + ':' + row.symbol;
		}

		function chartOptions() {
			return {
				type: 'line',
				data: {
					labels: [],
					datasets: [{
						label: 'bid',
						data: [],
						borderColor: '#4ade80',
						backgroundColor: 'rgba(74, 222, 128, 0.10)',
						borderWidth: 1.6,
						pointRadius: 0,
						pointHoverRadius: 3,
						tension: 0,
					}, {
						label: 'ask',
						data: [],
						borderColor: '#f87171',
						backgroundColor: 'rgba(248, 113, 113, 0.10)',
						borderWidth: 1.6,
						pointRadius: 0,
						pointHoverRadius: 3,
						tension: 0,
					}, {
						label: 'mark',
						data: [],
						borderColor: '#9ca3af',
						backgroundColor: 'rgba(156, 163, 175, 0.10)',
						borderWidth: 1.3,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'SMA20',
						data: [],
						borderColor: '#60a5fa',
						backgroundColor: 'rgba(96, 165, 250, 0.10)',
						borderWidth: 1.4,
						pointRadius: 0,
						pointHoverRadius: 0,
						tension: 0,
					}, {
						label: 'trades',
						data: [],
						borderColor: 'rgba(255, 255, 255, 0.7)',
						backgroundColor: '#ffffff',
						borderWidth: 1,
						pointRadius: 0,
						pointHoverRadius: 0,
						showLine: false,
					}],
				},
				options: {
					responsive: true,
					maintainAspectRatio: false,
					animation: false,
					interaction: { mode: 'nearest', intersect: false },
					plugins: {
						legend: { labels: { color: '#d1d5db' } },
						tooltip: {
							callbacks: {
								label: ctx => {
									const base = ctx.dataset.label + ': ' + fmt(ctx.parsed.y);
									if (ctx.dataset.label !== 'trades') {
										return base;
									}
									const sz = ctx.dataset.tradeSizes && ctx.dataset.tradeSizes[ctx.dataIndex];
									if (sz != null && Number.isFinite(sz) && sz > 0) {
										return base + ' | size ' + fmt(sz, 6);
									}
									return base;
								},
							},
						},
					},
					scales: {
						x: {
							ticks: { color: '#9ca3af', maxTicksLimit: 8 },
							grid: { color: 'rgba(156, 163, 175, 0.12)' },
						},
						y: {
							position: 'right',
							ticks: { color: '#9ca3af' },
							grid: { color: 'rgba(156, 163, 175, 0.12)' },
						},
					},
				},
			};
		}

		function clamp(min, value, max) {
			return Math.max(min, Math.min(value, max));
		}

		function chartWindowLabel() {
			if (chartWindowSeconds >= 60 && chartWindowSeconds % 60 === 0) {
				return (chartWindowSeconds / 60) + 'm';
			}
			return chartWindowSeconds + 's';
		}

		function maxTimeMs(series) {
			let max = 0;
			for (const point of series) {
				const ms = new Date(point.Time).getTime();
				if (!Number.isFinite(ms)) {
					continue;
				}
				if (ms > max) {
					max = ms;
				}
			}
			return max;
		}

		function trimPricesByWindow(series, cutoffMs) {
			return series.filter(point => new Date(point.Time).getTime() >= cutoffMs);
		}

		function trimRollingChartWindow(bids, asks, marks, sma20s, trades) {
			const windowMs = chartWindowSeconds * 1000;
			const endMs = Math.max(
				maxTimeMs(bids),
				maxTimeMs(asks),
				maxTimeMs(marks),
				maxTimeMs(sma20s),
				maxTimeMs(trades),
			);
			if (!Number.isFinite(endMs) || endMs <= 0) {
				return { bids, asks, marks, sma20s, trades };
			}

			const cutoffMs = endMs - windowMs;
			let nextBids = trimPricesByWindow(bids, cutoffMs);
			let nextAsks = trimPricesByWindow(asks, cutoffMs);
			const nextMarks = trimPricesByWindow(marks, cutoffMs);
			const nextSma20s = trimPricesByWindow(sma20s, cutoffMs);
			const nextTrades = trimPricesByWindow(trades, cutoffMs);

			const pairCount = Math.min(nextBids.length, nextAsks.length);
			nextBids = nextBids.slice(-pairCount);
			nextAsks = nextAsks.slice(-pairCount);

			return {
				bids: nextBids,
				asks: nextAsks,
				marks: nextMarks,
				sma20s: nextSma20s,
				trades: nextTrades,
			};
		}

		function adaptMetricSizing(rowCount) {
			const grid = document.getElementById('pair-grid');
			const count = Math.max(rowCount || renderedKeys.length || 1, 1);
			const gridHeight = grid.clientHeight || Math.max(window.innerHeight - 44, 0);
			const rowHeight = gridHeight / count;
			const scale = clamp(0.95, rowHeight / 230, 2.05);
			const minWidth = Math.round(clamp(112, 112 + ((scale - 1) * 70), 190));
			const metricColumns = rowHeight >= 250 ? 'repeat(3, minmax(0, 1fr))' : 'repeat(auto-fit, minmax(' + minWidth + 'px, 1fr))';
			const metricsColumn = scale > 1.6 ? '44%' : scale > 1.25 ? '40%' : '34%';

			grid.style.setProperty('--metric-scale', scale.toFixed(2));
			grid.style.setProperty('--metric-grid-columns', metricColumns);
			grid.style.setProperty('--metrics-column', metricsColumn);
		}

		function metric(label, value, className) {
			return '<div class="metric">' +
				'<div class="metric-label">' + label + '</div>' +
				'<div class="metric-value ' + (className || 'neutral') + '">' + value + '</div>' +
			'</div>';
		}

		function metricsHtml(row) {
			return '<div class="pair-title">' +
				'<div class="symbol">' + row.symbol + '</div>' +
			'</div>' +
			'<div class="metric-grid">' +
				metric('Mark Price', fmt(row.mark_price)) +
				metric('Mid Price', fmt(row.mid_price)) +
				metric('Last Traded', fmt(row.last_trade_price)) +
				metric('Trades / min', row.trades_per_minute) +
				metric('Spread Avg', pct(row.spread_avg), cls(row.spread_avg)) +
				metric('Slippage Avg', pct(row.slippage_avg), cls(row.slippage_avg)) +
				metric('Open Interest', fmt(row.open_interest, 2)) +
				metric('Funding', pct(row.funding_rate, 6), cls(row.funding_rate)) +
				metric('SMA20', fmt(row.sma20)) +
				metric('SMA20 Slope', pct(row.sma20_slope_pct), cls(row.sma20_slope_pct)) +
			'</div>';
		}

		function renderSections(rows) {
			const keys = rows.map(rowKey);
			if (keys.length === renderedKeys.length && keys.every((key, i) => key === renderedKeys[i])) {
				return;
			}

			Object.values(charts).forEach(chart => chart.destroy());
			charts = {};
			renderedKeys = keys;

			const grid = document.getElementById('pair-grid');
			grid.style.gridTemplateRows = rows.length > 0
				? 'repeat(' + rows.length + ', minmax(0, 1fr))'
				: '1fr';
			adaptMetricSizing(rows.length);
			grid.innerHTML = rows.map((row, i) =>
				'<section class="pair-section">' +
					'<div class="metrics-panel" id="metrics-' + i + '"></div>' +
					'<div class="chart-panel">' +
						'<div class="chart-wrap">' +
							'<div class="chart-points" id="chart-points-' + i + '">0 points</div>' +
							'<canvas id="chart-' + i + '"></canvas>' +
						'</div>' +
					'</div>' +
				'</section>'
			).join('');

			if (typeof Chart === 'undefined') {
				document.getElementById('meta').textContent = 'Chart.js failed to load, showing metrics only';
				return;
			}

			rows.forEach((row, i) => {
				charts[rowKey(row)] = new Chart(document.getElementById('chart-' + i), chartOptions());
			});
		}

		function updateSection(row, i) {
			const metrics = document.getElementById('metrics-' + i);
			if (metrics) metrics.innerHTML = metricsHtml(row);

			const points = document.getElementById('chart-points-' + i);
			let bids = (row.bid_prices || []).slice().reverse();
			let asks = (row.ask_prices || []).slice().reverse();
			let marks = (row.mark_prices || []).slice().reverse();
			let sma20s = (row.sma20_prices || []).slice().reverse();
			let trades = (row.trades || []).slice().reverse();
			const trimmed = trimRollingChartWindow(bids, asks, marks, sma20s, trades);
			bids = trimmed.bids;
			asks = trimmed.asks;
			marks = trimmed.marks;
			sma20s = trimmed.sma20s;
			trades = trimmed.trades;
			if (points) points.textContent = bids.length + ' points / ' + chartWindowLabel();

			const chart = charts[rowKey(row)];
			if (!chart) return;
			chart.data.labels = bids.map(price => new Date(price.Time).toLocaleTimeString());
			chart.data.datasets[0].data = bids.map(price => price.Price);
			chart.data.datasets[0].label = 'bid';
			chart.data.datasets[1].data = asks.map(price => price.Price);
			chart.data.datasets[1].label = 'ask';
			chart.data.datasets[2].data = alignSeriesToPrices(bids, marks);
			chart.data.datasets[2].label = 'mark';
			chart.data.datasets[3].data = alignSeriesToPrices(bids, sma20s);
			chart.data.datasets[3].label = 'SMA20';
			const tradeAlign = alignTradesToPrices(bids, trades);
			chart.data.datasets[4].data = tradeAlign.prices;
			chart.data.datasets[4].tradeSizes = tradeAlign.sizes;
			const tradeRadii = radiiFromTradeSizes(tradeAlign.prices, tradeAlign.sizes);
			chart.data.datasets[4].pointRadius = tradeRadii;
			chart.data.datasets[4].pointHoverRadius = tradeRadii.map(r => (r > 0 ? r + 2.5 : 0));
			chart.data.datasets[4].label = 'trades';
			setPriceScale(chart, bids, asks, marks);
			chart.update('none');
		}

		function setPriceScale(chart, bids, asks, marks) {
			const prices = bids.concat(asks, marks)
				.map(price => price.Price)
				.filter(price => price > 0);

			if (prices.length === 0) {
				delete chart.options.scales.y.min;
				delete chart.options.scales.y.max;
				return;
			}

			const min = Math.min(...prices);
			const max = Math.max(...prices);
			const padding = Math.max((max - min) * 0.08, max * 0.0001);
			chart.options.scales.y.min = min - padding;
			chart.options.scales.y.max = max + padding;
		}

		function alignSeriesToPrices(prices, series) {
			const aligned = new Array(prices.length).fill(null);
			let seriesIndex = 0;
			let latest = null;
			for (let priceIndex = 0; priceIndex < prices.length; priceIndex++) {
				const priceTime = new Date(prices[priceIndex].Time).getTime();
				while (
					seriesIndex < series.length &&
					new Date(series[seriesIndex].Time).getTime() <= priceTime
				) {
					latest = series[seriesIndex].Price;
					seriesIndex++;
				}
				aligned[priceIndex] = latest;
			}
			return aligned;
		}

		function alignTradesToPrices(prices, trades) {
			const aligned = new Array(prices.length).fill(null);
			const sizes = new Array(prices.length).fill(null);
			let priceIndex = 0;
			for (const trade of trades) {
				const tradeTime = new Date(trade.Time).getTime();
				while (
					priceIndex < prices.length - 1 &&
					new Date(prices[priceIndex + 1].Time).getTime() <= tradeTime
				) {
					priceIndex++;
				}
				if (prices.length > 0) {
					aligned[priceIndex] = trade.Price;
					sizes[priceIndex] = trade.Size;
				}
			}
			return { prices: aligned, sizes: sizes };
		}

		function radiiFromTradeSizes(priceData, sizeData) {
			const radii = new Array(priceData.length).fill(0);
			let minS = Infinity;
			let maxS = -Infinity;
			for (let i = 0; i < priceData.length; i++) {
				const p = priceData[i];
				if (p == null || !Number.isFinite(p)) {
					continue;
				}
				const s = sizeData[i];
				if (s != null && Number.isFinite(s) && s > 0) {
					if (s < minS) {
						minS = s;
					}
					if (s > maxS) {
						maxS = s;
					}
				}
			}
			const rMin = 2.8;
			const rMax = 8.5;
			const defaultR = 4.2;
			for (let i = 0; i < priceData.length; i++) {
				const p = priceData[i];
				if (p == null || !Number.isFinite(p)) {
					radii[i] = 0;
					continue;
				}
				const s = sizeData[i];
				if (s == null || !Number.isFinite(s) || s <= 0) {
					radii[i] = defaultR;
					continue;
				}
				if (!Number.isFinite(minS) || minS === maxS) {
					radii[i] = defaultR;
				} else {
					const t = (s - minS) / (maxS - minS);
					radii[i] = rMin + t * (rMax - rMin);
				}
			}
			return radii;
		}

		async function refresh() {
			try {
				const response = await fetch('/api/data');
				const data = await response.json();
				if (typeof data.chart_window_seconds === 'number' && data.chart_window_seconds > 0) {
					chartWindowSeconds = data.chart_window_seconds;
				}
				renderSections(data.pairs_data);
				data.pairs_data.forEach(updateSection);

				const latency = Object.entries(data.latency_ms || {})
					.map(([exchange, ms]) => exchange + ': ' + ms + 'ms')
					.join(' | ');
				document.getElementById('meta').textContent =
					'Pairs: ' + data.pairs_data.length + ' | Last update: ' + new Date(data.last_update).toLocaleTimeString() + ' | ' + latency;
			} catch (error) {
				document.getElementById('meta').textContent = 'Disconnected: ' + error;
			}
		}

		refresh();
		setInterval(refresh, DASHBOARD_REFRESH_MS);
		window.addEventListener('resize', () => adaptMetricSizing(renderedKeys.length));
	</script>
</body>
</html>`

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	data := dashboardTemplateData{
		ChartWindowSeconds: int(DASHBOARD_CHART_WINDOW / time.Second),
		RefreshMs:          int(DASHBOARD_REFRESH_INTERVAL / time.Millisecond),
	}
	if err := template.Must(template.New("dashboard").Parse(tmpl)).Execute(w, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) apiHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if err := json.NewEncoder(w).Encode(d.getDashboardData()); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func (d *Dashboard) getDashboardData() DashboardData {
	rows := make([]DashboardPairData, 0)
	latency := make(map[string]int64, 1)

	strat := d.model
	if strat == nil || strat.Exchange == nil {
		return DashboardData{
			PairsData:          rows,
			LastUpdate:         time.Now(),
			LatencyMs:          latency,
			ChartWindowSeconds: int(DASHBOARD_CHART_WINDOW / time.Second),
		}
	}

	exchangeName := strat.Exchange.Name()
	latency[exchangeName] = strat.Exchange.GetLatency()

	pairs := make([]string, 0, len(strat.traders))
	for pair := range strat.traders {
		pairs = append(pairs, pair)
	}
	sort.Strings(pairs)

	for _, pair := range pairs {
		t := strat.traders[pair]
		if t == nil {
			continue
		}

		t.RLock()
		midPrice := t.Price
		if t.bestBid > 0 && t.bestAsk > 0 {
			midPrice = (t.bestBid + t.bestAsk) / 2
		}
		prices := copyLatestPricePairs(t.Prices)
		bidPrices, askPrices := splitBidAskPricePairs(prices)
		sampleTime := latestPriceTime(bidPrices, askPrices)
		row := DashboardPairData{
			Exchange:        exchangeName,
			Symbol:          t.Pair,
			MarkPrice:       t.MarkPrice,
			MidPrice:        midPrice,
			LastTradePrice:  t.lastTradePrice,
			SpreadAvg:       t.spreadAvg,
			SlippageAvg:     t.slippageAvg,
			TradesPerMinute: t.tradePerMinute,
			OpenInterest:    t.openInterest,
			FundingRate:     t.fundingRate,
			SMA20:           t.SMA20,
			SMA20SlopePct:   t.SMA20SlopePct,
			BidPrices:       bidPrices,
			AskPrices:       askPrices,
			MarkPrices:      dashboardPricePoint(t.MarkPrice, sampleTime),
			SMA20Prices:     dashboardPricePoint(t.SMA20, sampleTime),
			Trades:          tradePricePoints(t.Trades),
		}
		t.RUnlock()

		d.applyHistory(rowKey(row), &row)
		rows = append(rows, row)
	}

	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Exchange == rows[j].Exchange {
			return rows[i].Symbol < rows[j].Symbol
		}
		return rows[i].Exchange < rows[j].Exchange
	})

	return DashboardData{
		PairsData:          rows,
		LastUpdate:         time.Now(),
		LatencyMs:          latency,
		ChartWindowSeconds: int(DASHBOARD_CHART_WINDOW / time.Second),
	}
}

func (d *Dashboard) applyHistory(key string, row *DashboardPairData) {
	d.historyMu.Lock()
	defer d.historyMu.Unlock()

	history := d.histories[key]
	if history == nil {
		history = &dashboardHistory{}
		d.histories[key] = history
	}

	cutoff := time.Now().Add(-DASHBOARD_CHART_WINDOW)
	incomingPairs := zipBidAskAligned(row.BidPrices, row.AskPrices)
	history.PairedQuotes = trimDashboardQuotePairs(
		mergeLatestQuotePairs(history.PairedQuotes, incomingPairs),
		cutoff,
	)
	history.MarkPrices = trimDashboardPrices(mergeLatestPrices(history.MarkPrices, row.MarkPrices), cutoff)
	history.SMA20Prices = trimDashboardPrices(mergeLatestPrices(history.SMA20Prices, row.SMA20Prices), cutoff)
	history.Trades = trimDashboardPrices(mergeLatestPrices(history.Trades, row.Trades), cutoff)

	bids, asks := splitBidAskPricePairs(history.PairedQuotes)
	row.BidPrices = copyLatestPrices(bids)
	row.AskPrices = copyLatestPrices(asks)
	row.MarkPrices = copyLatestPrices(history.MarkPrices)
	row.SMA20Prices = copyLatestPrices(history.SMA20Prices)
	row.Trades = copyLatestPrices(history.Trades)
}

func rowKey(row DashboardPairData) string {
	return row.Exchange + ":" + row.Symbol
}

func copyLatestPrices(prices []exchange.Price) []exchange.Price {
	return append([]exchange.Price(nil), prices...)
}

func copyLatestPricePairs(prices [][2]exchange.Price) [][2]exchange.Price {
	return append([][2]exchange.Price(nil), prices...)
}

func trimDashboardPrices(prices []exchange.Price, cutoff time.Time) []exchange.Price {
	filtered := prices[:0]
	for _, price := range prices {
		if !price.Time.IsZero() && price.Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, price)
	}
	return filtered
}

func dashboardPricePoint(price float64, ts time.Time) []exchange.Price {
	if price <= 0 {
		return nil
	}
	if ts.IsZero() {
		ts = time.Now()
	}
	return []exchange.Price{{
		Price: price,
		Time:  ts,
	}}
}

func latestPriceTime(priceGroups ...[]exchange.Price) time.Time {
	var latest time.Time
	for _, prices := range priceGroups {
		for _, price := range prices {
			if price.Time.After(latest) {
				latest = price.Time
			}
		}
	}
	return latest
}

func splitBidAskPricePairs(prices [][2]exchange.Price) ([]exchange.Price, []exchange.Price) {
	bids := make([]exchange.Price, 0, len(prices))
	asks := make([]exchange.Price, 0, len(prices))

	for _, price := range prices {
		bid := price[0]
		ask := price[1]
		if bid.Price <= 0 || ask.Price <= 0 || ask.Price < bid.Price {
			continue
		}
		bids = append(bids, bid)
		asks = append(asks, ask)
	}

	return bids, asks
}

// zipBidAskAligned builds one row per index from parallel bid/ask slices (same snapshot order).
func zipBidAskAligned(bids, asks []exchange.Price) [][2]exchange.Price {
	n := len(bids)
	if len(asks) < n {
		n = len(asks)
	}
	if n == 0 {
		return nil
	}
	out := make([][2]exchange.Price, n)
	for i := 0; i < n; i++ {
		out[i] = [2]exchange.Price{bids[i], asks[i]}
	}
	return out
}

type quotePairKey struct {
	Time  int64
	Bid   float64
	Ask   float64
	BidSz float64
	AskSz float64
}

func mergeLatestQuotePairs(existing, incoming [][2]exchange.Price) [][2]exchange.Price {
	seen := make(map[quotePairKey]struct{}, len(existing)+len(incoming))
	merged := make([][2]exchange.Price, 0, len(existing)+len(incoming))

	add := func(pair [2]exchange.Price) {
		bid, ask := pair[0], pair[1]
		if bid.Price <= 0 || ask.Price <= 0 || ask.Price < bid.Price {
			return
		}
		key := quotePairKey{
			Time:  bid.Time.UnixNano(),
			Bid:   bid.Price,
			Ask:   ask.Price,
			BidSz: bid.Size,
			AskSz: ask.Size,
		}
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		merged = append(merged, pair)
	}

	for _, p := range incoming {
		add(p)
	}
	for _, p := range existing {
		add(p)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i][0].Time.After(merged[j][0].Time)
	})

	return merged
}

func trimDashboardQuotePairs(pairs [][2]exchange.Price, cutoff time.Time) [][2]exchange.Price {
	filtered := pairs[:0]
	for _, pair := range pairs {
		if !pair[0].Time.IsZero() && pair[0].Time.Before(cutoff) {
			continue
		}
		filtered = append(filtered, pair)
	}
	return filtered
}

type priceKey struct {
	Time  int64
	Price float64
	Size  float64
}

func mergeLatestPrices(existing, incoming []exchange.Price) []exchange.Price {
	seen := make(map[priceKey]struct{}, len(existing)+len(incoming))
	merged := make([]exchange.Price, 0, len(existing)+len(incoming))

	add := func(price exchange.Price) {
		if price.Price <= 0 {
			return
		}

		key := priceKey{
			Time:  price.Time.UnixNano(),
			Price: price.Price,
			Size:  price.Size,
		}
		if _, ok := seen[key]; ok {
			return
		}

		seen[key] = struct{}{}
		merged = append(merged, price)
	}

	for _, price := range incoming {
		add(price)
	}
	for _, price := range existing {
		add(price)
	}

	sort.SliceStable(merged, func(i, j int) bool {
		return merged[i].Time.After(merged[j].Time)
	})

	return merged
}

func tradePricePoints(trades []exchange.Trade) []exchange.Price {
	points := make([]exchange.Price, 0, len(trades))
	for _, trade := range trades {
		point, ok := tradePricePoint(trade)
		if ok {
			points = append(points, point)
		}
	}
	return points
}

func tradePricePoint(trade exchange.Trade) (exchange.Price, bool) {
	var price float64
	var size float64
	var ts time.Time

	if trade.Order != nil {
		price = trade.Order.Price
		size = trade.Order.Size
		ts = trade.Order.Time
	}

	if len(trade.Fills) > 0 {
		var weightedPrice float64
		var totalSize float64
		for _, fill := range trade.Fills {
			if fill == nil || fill.Price <= 0 || fill.Size <= 0 {
				continue
			}
			if ts.IsZero() {
				ts = fill.Time
			}
			totalSize += fill.Size
			weightedPrice += fill.Price * fill.Size
		}
		if totalSize > 0 {
			price = weightedPrice / totalSize
			size = totalSize
		}
	}

	if price <= 0 {
		return exchange.Price{}, false
	}
	if ts.IsZero() {
		ts = time.Now()
	}

	return exchange.Price{
		Price: price,
		Size:  size,
		Time:  ts,
	}, true
}

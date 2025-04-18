package glance

import (
	"context"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"sort"
	"strings"
	"time"
)

var marketsWidgetTemplate = mustParseTemplate("markets.html", "widget-base.html")

type marketsWidget struct {
	widgetBase         `yaml:",inline"`
	StocksRequests     []marketRequest `yaml:"stocks"`
	MarketRequests     []marketRequest `yaml:"markets"`
	ChartLinkTemplate  string          `yaml:"chart-link-template"`
	SymbolLinkTemplate string          `yaml:"symbol-link-template"`
	Sort               string          `yaml:"sort-by"`
	Markets            marketList      `yaml:"-"`
	MarketDuration     MarketDuration  `yaml:"duration"` // 1d, 1w, 1m, 3m, 6m, 1y, 2y, 5y, max
}

type MarketDuration string // 1d, 1w, 1m, 3m, 6m, 1y, 2y, 5y, max

func (md MarketDuration) Valid() bool {
	return slices.Contains([]MarketDuration{"1d", "1w", "1m", "3m", "6m", "1y", "2y", "5y", "max"}, md)
}

func (widget *marketsWidget) initialize() error {
	widget.withTitle("Markets").withCacheDuration(time.Hour)

	// legacy support, remove in v0.10.0
	if len(widget.MarketRequests) == 0 {
		widget.MarketRequests = widget.StocksRequests
	}

	for i := range widget.MarketRequests {
		m := &widget.MarketRequests[i]

		if widget.ChartLinkTemplate != "" && m.ChartLink == "" {
			m.ChartLink = strings.ReplaceAll(widget.ChartLinkTemplate, "{SYMBOL}", m.Symbol)
		}

		if widget.SymbolLinkTemplate != "" && m.SymbolLink == "" {
			m.SymbolLink = strings.ReplaceAll(widget.SymbolLinkTemplate, "{SYMBOL}", m.Symbol)
		}
	}

	if !widget.MarketDuration.Valid() {
		widget.MarketDuration = "1d"
	}

	return nil
}

func (widget *marketsWidget) update(ctx context.Context) {
	markets, err := fetchMarketsDataFromYahoo(widget.MarketRequests, widget.MarketDuration)

	if !widget.canContinueUpdateAfterHandlingErr(err) {
		return
	}

	if widget.Sort == "absolute-change" {
		markets.sortByAbsChange()
	} else if widget.Sort == "change" {
		markets.sortByChange()
	}

	widget.Markets = markets
}

func (widget *marketsWidget) Render() template.HTML {
	return widget.renderTemplate(widget, marketsWidgetTemplate)
}

type marketRequest struct {
	CustomName string `yaml:"name"`
	Symbol     string `yaml:"symbol"`
	ChartLink  string `yaml:"chart-link"`
	SymbolLink string `yaml:"symbol-link"`
}

type market struct {
	marketRequest
	Name           string
	Currency       string
	Price          float64
	PercentChange  float64
	SvgChartPoints string
}

type marketList []market

func (t marketList) sortByAbsChange() {
	sort.Slice(t, func(i, j int) bool {
		return math.Abs(t[i].PercentChange) > math.Abs(t[j].PercentChange)
	})
}

func (t marketList) sortByChange() {
	sort.Slice(t, func(i, j int) bool {
		return t[i].PercentChange > t[j].PercentChange
	})
}

type marketResponseJson struct {
	Chart struct {
		Result []struct {
			Meta struct {
				Currency           string  `json:"currency"`
				Symbol             string  `json:"symbol"`
				RegularMarketPrice float64 `json:"regularMarketPrice"`
				ChartPreviousClose float64 `json:"chartPreviousClose"`
				ShortName          string  `json:"shortName"`
			} `json:"meta"`
			Indicators struct {
				Quote []struct {
					Close []float64 `json:"close,omitempty"`
				} `json:"quote"`
			} `json:"indicators"`
		} `json:"result"`
	} `json:"chart"`
}

// TODO: allow changing chart time frame
const marketChartDays = 21

func fetchMarketsDataFromYahoo(marketRequests []marketRequest, interval MarketDuration) (marketList, error) {
	requests := make([]*http.Request, 0, len(marketRequests))

	for i := range marketRequests {
		request, _ := http.NewRequest("GET", fmt.Sprintf("https://query1.finance.yahoo.com/v8/finance/chart/%s?range=1mo&interval=%s", marketRequests[i].Symbol, interval), nil)
		setBrowserUserAgentHeader(request)
		requests = append(requests, request)
	}

	job := newJob(decodeJsonFromRequestTask[marketResponseJson](defaultHTTPClient), requests)
	responses, errs, err := workerPoolDo(job)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errNoContent, err)
	}

	markets := make(marketList, 0, len(responses))
	var failed int

	for i := range responses {
		if errs[i] != nil {
			failed++
			slog.Error("Failed to fetch market data", "symbol", marketRequests[i].Symbol, "error", errs[i])
			continue
		}

		response := responses[i]

		if len(response.Chart.Result) == 0 {
			failed++
			slog.Error("Market response contains no data", "symbol", marketRequests[i].Symbol)
			continue
		}

		prices := response.Chart.Result[0].Indicators.Quote[0].Close

		if len(prices) > marketChartDays {
			prices = prices[len(prices)-marketChartDays:]
		}

		previous := response.Chart.Result[0].Meta.RegularMarketPrice

		if len(prices) >= 2 && prices[len(prices)-2] != 0 {
			previous = prices[len(prices)-2]
		}

		points := svgPolylineCoordsFromYValues(100, 50, maybeCopySliceWithoutZeroValues(prices))

		currency, exists := currencyToSymbol[strings.ToUpper(response.Chart.Result[0].Meta.Currency)]
		if !exists {
			currency = response.Chart.Result[0].Meta.Currency
		}

		markets = append(markets, market{
			marketRequest: marketRequests[i],
			Price:         response.Chart.Result[0].Meta.RegularMarketPrice,
			Currency:      currency,
			Name: ternary(marketRequests[i].CustomName == "",
				response.Chart.Result[0].Meta.ShortName,
				marketRequests[i].CustomName,
			),
			PercentChange: percentChange(
				response.Chart.Result[0].Meta.RegularMarketPrice,
				previous,
			),
			SvgChartPoints: points,
		})
	}

	if len(markets) == 0 {
		return nil, errNoContent
	}

	if failed > 0 {
		return markets, fmt.Errorf("%w: could not fetch data for %d market(s)", errPartialContent, failed)
	}

	return markets, nil
}

var currencyToSymbol = map[string]string{
	"USD": "$",
	"EUR": "€",
	"JPY": "¥",
	"CAD": "C$",
	"AUD": "A$",
	"GBP": "£",
	"CHF": "Fr",
	"NZD": "N$",
	"INR": "₹",
	"BRL": "R$",
	"RUB": "₽",
	"TRY": "₺",
	"ZAR": "R",
	"CNY": "¥",
	"KRW": "₩",
	"HKD": "HK$",
	"SGD": "S$",
	"SEK": "kr",
	"NOK": "kr",
	"DKK": "kr",
	"PLN": "zł",
	"PHP": "₱",
}

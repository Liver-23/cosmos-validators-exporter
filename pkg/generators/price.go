package generators

import (
	"main/pkg/constants"
	fetchersPkg "main/pkg/fetchers"
	statePkg "main/pkg/state"

	"github.com/prometheus/client_golang/prometheus"
)

type PriceGenerator struct {
}

func NewPriceGenerator() *PriceGenerator {
	return &PriceGenerator{}
}

func (g *PriceGenerator) Generate(state *statePkg.State) []prometheus.Collector {
	data, ok := statePkg.StateGet[fetchersPkg.PriceData](state, constants.FetcherNamePrice)
	if !ok {
		return []prometheus.Collector{}
	}

	tokenPriceGauge := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: constants.MetricsPrefix + "price",
			Help: "Price of 1 token in display denom in USD",
		},
		[]string{"chain", "denom", "source", "base_currency"},
	)

	for chainName, chainPrices := range data.Prices {
		for denom, price := range chainPrices {
			tokenPriceGauge.With(prometheus.Labels{
				"chain":         chainName,
				"denom":         denom,
				"source":        string(price.Source),
				"base_currency": price.BaseCurrency,
			}).Set(price.Value)
		}
	}

	return []prometheus.Collector{tokenPriceGauge}
}

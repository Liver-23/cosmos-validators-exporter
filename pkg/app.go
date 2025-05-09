package pkg

import (
	"context"
	controllerPkg "main/pkg/controller"
	fetchersPkg "main/pkg/fetchers"
	"main/pkg/fs"
	generatorsPkg "main/pkg/generators"
	"main/pkg/tendermint"
	"main/pkg/tracing"
	"net/http"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel/attribute"

	"main/pkg/config"
	loggerPkg "main/pkg/logger"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

type App struct {
	Tracer trace.Tracer
	Config *config.Config
	Logger *zerolog.Logger
	Server *http.Server

	RPCs map[string]*tendermint.RPCWithConsumers

	// Fetcher is a class that fetch data and is later stored in state.
	// It doesn't provide any metrics, only data to generate them later.
	Fetchers []fetchersPkg.Fetcher

	// Generator is a class that takes some metrics from the state
	// that were fetcher by one or more Fetchers and generates one or more
	// metrics based on this data.
	// Example: ActiveSetTokenGenerator generates a metric
	// based on ValidatorsFetcher and StakingParamsFetcher.
	Generators []generatorsPkg.Generator

	Controller *controllerPkg.Controller
}

func NewApp(configPath string, filesystem fs.FS, version string) *App {
	appConfig, err := config.GetConfig(configPath, filesystem)
	if err != nil {
		loggerPkg.GetDefaultLogger().Panic().Err(err).Msg("Could not load config")
	}

	if err = appConfig.Validate(); err != nil {
		loggerPkg.GetDefaultLogger().Panic().Err(err).Msg("Provided config is invalid!")
	}

	logger := loggerPkg.GetLogger(appConfig.LogConfig)
	warnings := appConfig.DisplayWarnings()
	for _, warning := range warnings {
		entry := logger.Warn()
		for label, value := range warning.Labels {
			entry = entry.Str(label, value)
		}

		entry.Msg(warning.Message)
	}

	tracer := tracing.InitTracer(appConfig.TracingConfig, version)

	rpcs := make(map[string]*tendermint.RPCWithConsumers, len(appConfig.Chains))

	for _, chain := range appConfig.Chains {
		rpcs[chain.Name] = tendermint.RPCWithConsumersFromChain(chain, appConfig.Timeout, *logger, tracer)
	}

	fetchers := []fetchersPkg.Fetcher{
		fetchersPkg.NewSlashingParamsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewCommissionFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewDelegationsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewUnbondsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewSigningInfoFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewRewardsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewBalanceFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewSelfDelegationFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewValidatorsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewConsumerValidatorsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewStakingParamsFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewPriceFetcher(logger, appConfig, tracer),
		fetchersPkg.NewNodeInfoFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewConsumerInfoFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewValidatorConsumersFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewConsumerCommissionFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewInflationFetcher(logger, appConfig.Chains, rpcs, tracer),
		fetchersPkg.NewSupplyFetcher(logger, appConfig.Chains, rpcs, tracer),
	}

	generators := []generatorsPkg.Generator{
		generatorsPkg.NewSlashingParamsGenerator(),
		generatorsPkg.NewIsConsumerGenerator(appConfig.Chains),
		generatorsPkg.NewUptimeGenerator(),
		generatorsPkg.NewCommissionGenerator(appConfig.Chains),
		generatorsPkg.NewDelegationsGenerator(),
		generatorsPkg.NewUnbondsGenerator(),
		generatorsPkg.NewSigningInfoGenerator(),
		generatorsPkg.NewRewardsGenerator(appConfig.Chains),
		generatorsPkg.NewBalanceGenerator(appConfig.Chains),
		generatorsPkg.NewSelfDelegationGenerator(appConfig.Chains),
		generatorsPkg.NewValidatorsInfoGenerator(appConfig.Chains),
		generatorsPkg.NewSingleValidatorInfoGenerator(appConfig.Chains, logger),
		generatorsPkg.NewValidatorRankGenerator(appConfig.Chains, logger),
		generatorsPkg.NewActiveSetTokensGenerator(appConfig.Chains),
		generatorsPkg.NewNodeInfoGenerator(),
		generatorsPkg.NewStakingParamsGenerator(),
		generatorsPkg.NewPriceGenerator(),
		generatorsPkg.NewConsumerInfoGenerator(appConfig.Chains),
		generatorsPkg.NewConsumerNeedsToSignGenerator(appConfig.Chains),
		generatorsPkg.NewValidatorActiveGenerator(appConfig.Chains, logger),
		generatorsPkg.NewValidatorCommissionRateGenerator(appConfig.Chains, logger),
		generatorsPkg.NewInflationGenerator(),
		generatorsPkg.NewSupplyGenerator(appConfig.Chains),
	}

	controller := controllerPkg.NewController(fetchers, logger)

	server := &http.Server{Addr: appConfig.ListenAddress, Handler: nil}

	return &App{
		Logger:     logger,
		Config:     appConfig,
		Tracer:     tracer,
		RPCs:       rpcs,
		Fetchers:   fetchers,
		Generators: generators,
		Server:     server,
		Controller: controller,
	}
}

func (a *App) Start() {
	otelHandler := otelhttp.NewHandler(http.HandlerFunc(a.Handler), "prometheus")
	handler := http.NewServeMux()
	handler.Handle("/metrics", otelHandler)
	handler.HandleFunc("/healthcheck", a.Healthcheck)
	a.Server.Handler = handler

	a.Logger.Info().Str("addr", a.Config.ListenAddress).Msg("Listening")

	err := a.Server.ListenAndServe()
	if err != nil {
		a.Logger.Panic().Err(err).Msg("Could not start application")
	}
}

func (a *App) Stop() {
	a.Logger.Info().Str("addr", a.Config.ListenAddress).Msg("Shutting down server...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = a.Server.Shutdown(ctx)
}

func (a *App) Handler(w http.ResponseWriter, r *http.Request) {
	requestID := uuid.New().String()

	span := trace.SpanFromContext(r.Context())
	span.SetAttributes(attribute.String("request-id", requestID))
	rootSpanCtx := r.Context()

	defer span.End()

	requestStart := time.Now()

	sublogger := a.Logger.With().
		Str("request-id", requestID).
		Logger()

	registry := prometheus.NewRegistry()

	state, queryInfos := a.Controller.Fetch(rootSpanCtx)

	queriesMetrics := NewQueriesMetrics(a.Config.Chains, queryInfos)
	registry.MustRegister(queriesMetrics.GetMetrics(rootSpanCtx)...)

	for _, generator := range a.Generators {
		metrics := generator.Generate(state)
		registry.MustRegister(metrics...)
	}

	h := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	h.ServeHTTP(w, r)

	sublogger.Info().
		Str("method", http.MethodGet).
		Str("endpoint", "/metrics").
		Float64("request-time", time.Since(requestStart).Seconds()).
		Msg("Request processed")
}

func (a *App) Healthcheck(w http.ResponseWriter, r *http.Request) {
	_, _ = w.Write([]byte("ok"))
}

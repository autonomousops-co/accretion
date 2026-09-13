package telemetry

import (
	"context"

	"go.uber.org/zap"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// Init sets up the OpenTelemetry TracerProvider and LoggerProvider.
// When disabled, installs noop providers and returns a noop shutdown function.
func Init(cfg config.TelemetryConfig) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		otel.SetTracerProvider(noop.NewTracerProvider())
		return func(context.Context) error { return nil }, nil
	}

	// Route OTel internal errors through zap instead of Go's default log package.
	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		logger.Logger.Warn("OpenTelemetry internal error", zap.Error(err))
	}))

	ctx := context.Background()

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
		),
	)
	if err != nil {
		return nil, err
	}

	// Build exporter options
	traceOpts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
	if cfg.Insecure {
		traceOpts = append(traceOpts, otlptracegrpc.WithInsecure())
	}

	// Trace exporter
	traceExporter, err := otlptracegrpc.New(ctx, traceOpts...)
	if err != nil {
		return nil, err
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)
	otel.SetTracerProvider(tp)

	// Log exporter (opt-in; requires collector with OTLP logs support)
	var lp *sdklog.LoggerProvider
	if cfg.LogsEnabled {
		logOpts := []otlploggrpc.Option{otlploggrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			logOpts = append(logOpts, otlploggrpc.WithInsecure())
		}

		logExporter, logErr := otlploggrpc.New(ctx, logOpts...)
		if logErr != nil {
			return nil, logErr
		}

		lp = sdklog.NewLoggerProvider(
			sdklog.WithProcessor(sdklog.NewBatchProcessor(logExporter)),
			sdklog.WithResource(res),
		)

		// Bridge zap logs to OTEL
		logger.AttachOtelBridge(lp)
	}

	logger.Logger.Info("OpenTelemetry initialized",
		zap.String("endpoint", cfg.Endpoint), zap.String("service", cfg.ServiceName),
		zap.Float64("sample_rate", cfg.SampleRate), zap.Bool("logs_enabled", cfg.LogsEnabled))

	return func(ctx context.Context) error {
		if err := tp.Shutdown(ctx); err != nil {
			return err
		}
		if lp != nil {
			return lp.Shutdown(ctx)
		}
		return nil
	}, nil
}

// Tracer returns a tracer from the global provider.
func Tracer(name string) trace.Tracer {
	return otel.Tracer(name)
}

// StartSpan starts a new span and returns the updated context and span.
func StartSpan(ctx context.Context, tracerName, spanName string) (context.Context, trace.Span) {
	return Tracer(tracerName).Start(ctx, spanName)
}

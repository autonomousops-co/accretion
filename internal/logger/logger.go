package logger

import (
	"context"

	"go.opentelemetry.io/contrib/bridges/otelzap"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/autonomousops-co/accretion/internal/config"
)

// Logger is the global zap logger used throughout the application.
var Logger *zap.Logger

type ctxKey struct{}

// Initialize sets up the global logger from the logging config.
func Initialize(cfg config.LoggingConfig) error {
	var zapLevel zapcore.Level
	switch cfg.Level {
	case "debug":
		zapLevel = zapcore.DebugLevel
	case "info":
		zapLevel = zapcore.InfoLevel
	case "warn":
		zapLevel = zapcore.WarnLevel
	case "error":
		zapLevel = zapcore.ErrorLevel
	default:
		zapLevel = zapcore.InfoLevel
	}

	encoding := cfg.Format
	if encoding == "" {
		encoding = "console"
	}

	zapCfg := zap.NewProductionConfig()
	zapCfg.Level = zap.NewAtomicLevelAt(zapLevel)
	zapCfg.Encoding = encoding
	zapCfg.DisableCaller = cfg.DisableCaller
	zapCfg.DisableStacktrace = cfg.DisableStacktrace
	zapCfg.EncoderConfig.TimeKey = "timestamp"
	zapCfg.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	zapCfg.EncoderConfig.EncodeLevel = zapcore.CapitalLevelEncoder

	l, err := zapCfg.Build()
	if err != nil {
		return err
	}

	Logger = l
	return nil
}

// AttachOtelBridge tees the existing zap core with an otelzap core so that
// all zap log records are also emitted as OTEL log records.
func AttachOtelBridge(lp *sdklog.LoggerProvider) {
	if Logger == nil {
		return
	}
	otelCore := otelzap.NewCore("github.com/autonomousops-co/accretion", otelzap.WithLoggerProvider(lp))
	Logger = Logger.WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, otelCore)
	}))
}

// WithCtx returns a child logger with the given context attached for trace correlation.
func WithCtx(ctx context.Context) *zap.Logger {
	return Logger.With(zap.Any("ctx", ctx))
}

// FromCtx retrieves the logger stored in ctx, falling back to the global Logger.
func FromCtx(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok {
		return l
	}
	return Logger
}

// NewContext stores a logger in the context.
func NewContext(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// Sync flushes any buffered log entries.
func Sync() {
	if Logger != nil {
		_ = Logger.Sync()
	}
}

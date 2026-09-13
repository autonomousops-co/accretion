package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/collector"
	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

var collectorCmd = &cobra.Command{
	Use:   "collector",
	Short: "Start the Accretion collector service",
	Long:  `Start the Accretion collector service to manage pod assignments and worker health.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadCollector()
		if err != nil {
			// Initialize logger with default level first for error logging
			bootstrapCfg := config.LoggingConfig{Level: bootstrapLogLevel, DisableStacktrace: true}
			if initErr := logger.Initialize(bootstrapCfg); initErr != nil {
				panic(fmt.Sprintf("Failed to initialize logger: %v", initErr))
			}
			logger.Logger.Fatal("Failed to load config", zap.Error(err))
		}

		// Initialize logger with config level
		if logErr := logger.Initialize(cfg.Logging); logErr != nil {
			logger.Logger.Fatal("Failed to initialize logger", zap.Error(logErr))
		}

		// Initialize telemetry
		cfg.Telemetry.ServiceName = "accretion-collector"
		otelShutdown, err := telemetry.Init(cfg.Telemetry)
		if err != nil {
			logger.Logger.Fatal("Failed to initialize telemetry", zap.Error(err))
		}
		defer func() {
			otelShutdown(context.Background()) //nolint:errcheck // best-effort shutdown
		}()

		logger.Logger.Info("Starting Accretion collector", zap.String("log_level", cfg.Logging.Level))

		// Create the shutdown context BEFORE the manager: the manager's namespace
		// scopes hang their background re-resolution goroutines off it (#152), so
		// it has to be the cancellable one to stop them on shutdown.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Create collector manager
		manager, err := collector.NewManager(ctx, cfg)
		if err != nil {
			logger.Logger.Fatal("Failed to create collector manager", zap.Error(err))
		}

		// Start health check server
		healthServer := NewHealthServer(8080, "accretion-api-collector")
		healthServer.Start()

		// Create a channel to listen for interrupt signals
		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

		// Start collector in a goroutine
		go func() {
			logger.Logger.Info("Starting collector manager...")
			if err := manager.Start(ctx); err != nil && err != context.Canceled {
				logger.Logger.Error("Collector error", zap.Error(err))
				// Signal main goroutine to exit
				done <- syscall.SIGTERM
			}
		}()

		// Wait for interrupt signal
		<-done
		logger.Logger.Info("Collector is shutting down...")

		// Cancel context to stop collector
		cancel()

		// Stop collector
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer shutdownCancel()

		if err := manager.Stop(shutdownCtx); err != nil {
			logger.Logger.Error("Failed to stop collector manager gracefully", zap.Error(err))
		}

		// Stop health server
		if err := healthServer.Stop(); err != nil {
			logger.Logger.Error("Failed to stop health server gracefully", zap.Error(err))
		}

		logger.Logger.Info("Collector exited")
	},
}

func init() {
	rootCmd.AddCommand(collectorCmd)
}

package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/server"
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

var disableAuthentication bool

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Start the Accretion web server",
	Long:  `Start the Accretion web server to serve the API and web UI.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.Load()
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
		cfg.Telemetry.ServiceName = "accretion-api"
		otelShutdown, err := telemetry.Init(cfg.Telemetry)
		if err != nil {
			logger.Logger.Fatal("Failed to initialize telemetry", zap.Error(err))
		}
		defer func() {
			otelShutdown(context.Background()) //nolint:errcheck // best-effort shutdown
		}()

		// Determine authentication status
		authEnabled := !disableAuthentication
		if disableAuthentication {
			logger.Logger.Error("SECURITY WARNING: Authentication is DISABLED via --disable-authentication flag")
			logger.Logger.Error("This should NEVER be used in production environments")
		}

		logger.Logger.Warn("Starting Accretion server", zap.String("log_level", cfg.Logging.Level))

		// Log full configuration
		logger.Logger.Warn("Full Configuration", zap.Any("config", cfg.Sanitize()))

		srv, err := server.New(cfg, authEnabled)
		if err != nil {
			logger.Logger.Fatal("Failed to construct server", zap.Error(err))
		}
		if err := srv.Start(); err != nil {
			logger.Logger.Fatal("Failed to start server", zap.Error(err))
		}
	},
}

func init() {
	rootCmd.AddCommand(serverCmd)

	// Add the disable authentication flag
	serverCmd.Flags().BoolVar(
		&disableAuthentication, "disable-authentication", false,
		"Disable OAuth authentication (DANGEROUS - for development only)",
	)
}

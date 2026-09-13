package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/discord"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

var discordCmd = &cobra.Command{
	Use:   "discord",
	Short: "Start the Accretion Discord bot",
	Long:  `Start the Accretion Discord bot to chat with Accretion via Discord messages.`,
	Run: func(cmd *cobra.Command, args []string) {
		cfg, err := config.LoadDiscord()
		if err != nil {
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
		cfg.Telemetry.ServiceName = "accretion-discord"
		otelShutdown, err := telemetry.Init(cfg.Telemetry)
		if err != nil {
			logger.Logger.Fatal("Failed to initialize telemetry", zap.Error(err))
		}
		defer func() {
			otelShutdown(context.Background()) //nolint:errcheck // best-effort shutdown
		}()

		if cfg.Token == "" {
			logger.Logger.Fatal("Discord bot token is required. Set DISCORD_BOT_TOKEN or configure discord.token in config")
		}

		logger.Logger.Info("Starting Accretion Discord bot", zap.String("log_level", cfg.Logging.Level))
		logger.Logger.Info("Full Configuration", zap.Any("config", cfg.Sanitize()))

		bot, err := discord.New(cfg)
		if err != nil {
			logger.Logger.Fatal("Failed to create Discord bot", zap.Error(err))
		}

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		done := make(chan os.Signal, 1)
		signal.Notify(done, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			if err := bot.Start(ctx); err != nil {
				logger.Logger.Error("Discord bot error", zap.Error(err))
				done <- syscall.SIGTERM
			}
		}()

		<-done
		logger.Logger.Info("Discord bot is shutting down...")

		cancel()

		if err := bot.Stop(); err != nil {
			logger.Logger.Error("Failed to stop Discord bot gracefully", zap.Error(err))
		}

		logger.Logger.Info("Discord bot exited")
	},
}

func init() {
	rootCmd.AddCommand(discordCmd)
}

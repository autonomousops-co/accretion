package discord

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	apiclient "github.com/autonomousops-co/accretion/internal/mcp/accretion"
	"github.com/autonomousops-co/accretion/internal/telemetry"
)

const tracerName = "discord"

// Bot represents the Discord bot that bridges Discord messages to Accretion's chat backend.
type Bot struct {
	session              *discordgo.Session
	toolUser             llm.ToolUser
	apiClient            *apiclient.Client
	systemPromptTemplate string
	systemConfig         string
	timeExtractionPrompt string
	guildID              string
	history              *conversationStore
}

// New creates a new Discord bot with the given configuration.
func New(cfg *config.DiscordConfig) (*Bot, error) {
	session, err := discordgo.New("Bot " + cfg.Token)
	if err != nil {
		return nil, fmt.Errorf("failed to create discord session: %w", err)
	}

	base, err := llm.NewClient(&cfg.LLM)
	if err != nil {
		return nil, fmt.Errorf("failed to create LLM client: %w", err)
	}

	promptBytes, err := os.ReadFile("prompts/system-prompt.md")
	if err != nil {
		return nil, fmt.Errorf("failed to read prompts/system-prompt.md: %w", err)
	}

	timePromptBytes, err := os.ReadFile("prompts/time-extraction.md")
	if err != nil {
		return nil, fmt.Errorf("failed to read prompts/time-extraction.md: %w", err)
	}

	systemConfig := fmt.Sprintf(
		"LLM Provider: %s\nChat Model: %s\nEmbedding Model: %s\nTelemetry Enabled: %v",
		cfg.LLM.Provider,
		cfg.LLM.ChatModel,
		cfg.LLM.EmbeddingModel,
		cfg.Telemetry.Enabled,
	)

	// Built before the Bot so a CA that cannot be read, or a skip-verify the
	// client refuses, fails here rather than on the first Discord message.
	apiClient, err := apiclient.NewClientWithTLS(cfg.API.URL, cfg.API.Key, apiclient.TLSOptions{
		CAFile:             cfg.API.CAFile,
		InsecureSkipVerify: cfg.API.InsecureSkipVerify,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to configure the accretion-api client: %w", err)
	}

	// Decide native vs. ReAct tool-use on the raw provider client, then wrap the
	// resulting ToolUser in tracing. Passing base (not a tracing-wrapped client)
	// is essential so AsToolUser can statically detect native tool support.
	b := &Bot{
		session:              session,
		toolUser:             llm.NewTracingToolUser(llm.AsToolUser(base)),
		apiClient:            apiClient,
		systemPromptTemplate: string(promptBytes),
		systemConfig:         systemConfig,
		timeExtractionPrompt: string(timePromptBytes),
		guildID:              cfg.GuildID,
		history:              newConversationStore(),
	}

	// Register the message handler
	session.AddHandler(b.onMessageCreate)

	// We only need to receive message events
	session.Identify.Intents = discordgo.IntentsGuildMessages |
		discordgo.IntentsDirectMessages | discordgo.IntentsMessageContent

	return b, nil
}

// Start opens the Discord websocket connection and blocks until the provided context is canceled.
func (b *Bot) Start(ctx context.Context) error {
	if err := b.session.Open(); err != nil {
		return fmt.Errorf("failed to open discord session: %w", err)
	}

	logger.Logger.Info("Discord bot is running", zap.String("user", b.session.State.User.Username))

	// Block until context is canceled
	<-ctx.Done()
	return nil
}

// Stop gracefully closes the Discord session.
func (b *Bot) Stop() error {
	return b.session.Close()
}

// onMessageCreate handles incoming Discord messages.
func (b *Bot) onMessageCreate(s *discordgo.Session, m *discordgo.MessageCreate) {
	// Ignore messages from the bot itself
	if m.Author.ID == s.State.User.ID {
		return
	}

	// Ignore messages from other guilds if a guild ID is configured
	if b.guildID != "" && m.GuildID != "" && m.GuildID != b.guildID {
		return
	}

	// Only respond when the bot is mentioned or in a DM
	isMentioned := false
	for _, mention := range m.Mentions {
		if mention.ID == s.State.User.ID {
			isMentioned = true
			break
		}
	}

	isDM := m.GuildID == ""
	if !isMentioned && !isDM {
		return
	}

	// Strip the bot mention from the message content
	userMessage := m.Content
	if isMentioned {
		userMessage = strings.TrimSpace(
			strings.ReplaceAll(userMessage, "<@"+s.State.User.ID+">", ""),
		)
	}

	if userMessage == "" {
		return
	}

	// Create a root span for the message handling pipeline
	ctx, span := telemetry.StartSpan(context.Background(), tracerName, "discord.handleMessage")
	defer span.End()

	span.SetAttributes(
		attribute.String("discord.author", m.Author.Username),
		attribute.String("discord.channel", m.ChannelID),
		attribute.Bool("discord.is_dm", isDM),
		attribute.Bool("discord.is_mention", isMentioned),
	)

	// Attach a context-aware logger for trace correlation
	l := logger.WithCtx(ctx)
	ctx = logger.NewContext(ctx, l)
	log := logger.FromCtx(ctx)

	log.Info("Discord message received",
		zap.String("author", m.Author.Username),
		zap.String("channel", m.ChannelID),
		zap.String("message_preview", userMessage[:minInt(50, len(userMessage))]),
	)

	// Show typing indicator continuously while processing.
	// Discord's typing indicator expires after ~10s, so we refresh every 5s.
	stopTyping := b.startTyping(s, m.ChannelID)

	// Process the message through the chat pipeline
	response, err := b.processMessage(ctx, m.ChannelID, userMessage)
	stopTyping()
	if err != nil {
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Error("Failed to process discord message", zap.Error(err))
		b.sendMessage(s, m.ChannelID, "Sorry, I encountered an error processing your message. Please try again.")
		return
	}

	span.SetAttributes(attribute.Int("discord.response_length", len(response)))
	b.sendMessage(s, m.ChannelID, response)
}

// startTyping sends a typing indicator immediately and then every 5 seconds
// until the returned stop function is called. This keeps the "bot is typing…"
// indicator visible in Discord for the entire duration of message processing.
func (b *Bot) startTyping(s *discordgo.Session, channelID string) func() {
	done := make(chan struct{})

	go func() {
		// Send immediately
		if err := s.ChannelTyping(channelID); err != nil {
			logger.Logger.Warn("Failed to send typing indicator", zap.Error(err))
		}

		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if err := s.ChannelTyping(channelID); err != nil {
					logger.Logger.Warn("Failed to send typing indicator", zap.Error(err))
				}
			}
		}
	}()

	return func() { close(done) }
}

// processMessage runs the user message through the agentic tool-use loop. The
// bot's ToolUser is always usable — native tool-use when the provider supports
// it, or a ReAct adapter otherwise — so the loop runs regardless of provider.
// If the loop fails, it falls back to the accretion-api's server-side RAG.
func (b *Bot) processMessage(ctx context.Context, channelID, userMessage string) (string, error) {
	ctx, span := telemetry.StartSpan(ctx, tracerName, "discord.processMessage")
	defer span.End()

	log := logger.FromCtx(ctx)

	now := time.Now().UTC().Format(time.RFC3339)
	promptContent := strings.ReplaceAll(b.systemPromptTemplate, "{{current_utc_time}}", now)
	promptContent = strings.ReplaceAll(promptContent, "{{system_config}}", b.systemConfig)
	// Remove the cluster_context placeholder — the agent fetches its own data.
	promptContent = strings.ReplaceAll(promptContent, "{{cluster_context}}",
		"Use the provided tools to search for relevant cluster data before answering.")

	// Prior turns for this channel, so a follow-up like "yes, please" resolves
	// against what was actually said. Empty for a first message.
	history := b.history.history(channelID)

	log.Info("Running agentic tool-use loop",
		zap.Int("history_messages", len(history)),
	)
	span.SetAttributes(
		attribute.Bool("discord.agentic", true),
		attribute.Int("discord.history_messages", len(history)),
	)

	response, err := b.runAgentLoop(b.toolUser, promptContent, history, userMessage)
	if err != nil {
		// Fall back to the accretion-api's server-side RAG (embedding, vector
		// search, and context assembly happen behind /api/chat).
		span.RecordError(err)
		span.SetStatus(codes.Error, err.Error())
		log.Warn("Agent loop failed; falling back to server-side RAG", zap.Error(err))
		fallback, ferr := b.apiClient.ChatWithHistory(ctx, toChatTurns(history), userMessage)
		if ferr != nil {
			return "", ferr
		}
		b.history.record(channelID, userMessage, fallback)
		return fallback, nil
	}

	log.Info("Agent loop completed", zap.Int("response_length", len(response)))
	b.history.record(channelID, userMessage, response)
	return response, nil
}

// sendMessage is the bot's only outbound path, so it owns the two transforms
// every message needs: markdown tables become aligned monospace, which Discord
// can actually render, and the result is split at the 2000 character limit
// fence-aware, so a code block broken across chunks stays monospace in each.
// Mentions are never parsed — model output carries log-derived text, and a
// stray @everyone in a log line must not become a real ping.
func (b *Bot) sendMessage(s *discordgo.Session, channelID, content string) {
	for _, chunk := range splitMessage(formatTables(content), discordMaxMessageLen) {
		msg := &discordgo.MessageSend{
			Content:         chunk,
			AllowedMentions: &discordgo.MessageAllowedMentions{Parse: []discordgo.AllowedMentionType{}},
		}
		if _, err := s.ChannelMessageSendComplex(channelID, msg); err != nil {
			logger.Logger.Error("Failed to send discord message",
				zap.String("channel", channelID),
				zap.Error(err),
			)
			return
		}
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

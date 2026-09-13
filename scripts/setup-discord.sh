#!/usr/bin/env bash
set -euo pipefail

# setup-discord.sh — Interactive Discord bot credential configuration
# Writes DISCORD_BOT_TOKEN and DISCORD_GUILD_ID to .env

ENV_FILE=".env"

echo "=== Accretion Discord Bot Setup ==="
echo ""

# Prompt for bot token
echo "Enter your Discord bot token (from Developer Portal > Bot > Token):"
read -rsp "Bot Token: " BOT_TOKEN
echo ""

if [[ -z "$BOT_TOKEN" ]]; then
    echo "Error: Bot token cannot be empty."
    exit 1
fi

# Basic format check — Discord tokens contain two dots
if [[ ! "$BOT_TOKEN" == *.*.* ]]; then
    echo "Warning: Token format looks unusual (expected two dots). Proceeding anyway."
fi

# Prompt for guild ID (optional)
echo ""
echo "Enter your Discord Guild (Server) ID (optional — leave blank to allow all servers):"
echo "  Tip: Enable Developer Mode in Discord Settings > Advanced, then right-click your server > Copy Server ID"
read -rp "Guild ID: " GUILD_ID

# Create .env if it doesn't exist
if [[ ! -f "$ENV_FILE" ]]; then
    echo "Creating $ENV_FILE..."
    touch "$ENV_FILE"
fi

# Update or append DISCORD_BOT_TOKEN
# Avoid sed substitution — token may contain delimiter characters.
# Instead, remove the existing line (if any) and append the new value.
if grep -q "^DISCORD_BOT_TOKEN=" "$ENV_FILE" 2>/dev/null; then
    grep -v "^DISCORD_BOT_TOKEN=" "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
    printf '%s\n' "DISCORD_BOT_TOKEN=$BOT_TOKEN" >> "$ENV_FILE"
    echo "Updated DISCORD_BOT_TOKEN in $ENV_FILE"
else
    printf '%s\n' "DISCORD_BOT_TOKEN=$BOT_TOKEN" >> "$ENV_FILE"
    echo "Added DISCORD_BOT_TOKEN to $ENV_FILE"
fi

# Update or append DISCORD_GUILD_ID
if [[ -n "$GUILD_ID" ]]; then
    if [[ ! "$GUILD_ID" =~ ^[0-9]+$ ]]; then
        echo "Warning: Guild ID '$GUILD_ID' does not look like a valid Discord snowflake (expected numeric). Proceeding anyway."
    fi
    if grep -q "^DISCORD_GUILD_ID=" "$ENV_FILE" 2>/dev/null; then
        grep -v "^DISCORD_GUILD_ID=" "$ENV_FILE" > "${ENV_FILE}.tmp" && mv "${ENV_FILE}.tmp" "$ENV_FILE"
        printf '%s\n' "DISCORD_GUILD_ID=$GUILD_ID" >> "$ENV_FILE"
        echo "Updated DISCORD_GUILD_ID in $ENV_FILE"
    else
        printf '%s\n' "DISCORD_GUILD_ID=$GUILD_ID" >> "$ENV_FILE"
        echo "Added DISCORD_GUILD_ID to $ENV_FILE"
    fi
fi

echo ""
echo "=== Setup Complete ==="
echo ""
echo "Next steps:"
echo "  1. Make sure the bot is invited to your server (see docs/discord-setup.md)"
echo "  2. Enable Message Content Intent in the Discord Developer Portal"
echo "  3. Run 'tilt up' to start the local stack with Discord integration"
echo ""

/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 */

package discord

import (
	"fmt"
	"net/http"
	"strings"
	"time"
	"os"

	"github.com/bwmarrin/discordgo"
	"github.com/lucasduport/stream-share/pkg/types"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Integration manages Discord integration components (bot only)
type Integration struct {
	Bot         *Bot
	Enabled     bool
	initialized bool
}

// NewIntegration creates a new Discord integration (bot only)
func NewIntegration() (*Integration, error) {
	utils.InfoLog("Initializing Discord integration")

	enabled := os.Getenv("DISCORD_BOT_ENABLED") == "true"
	if !enabled {
		utils.InfoLog("Discord integration disabled by configuration")
		return &Integration{Enabled: false}, nil
	}

	integration := &Integration{Enabled: true}

	// Initialize bot
	token := os.Getenv("DISCORD_BOT_TOKEN")
	if token == "" {
		utils.WarnLog("Discord bot token not provided - bot functionality disabled")
	} else {
	adminRole := os.Getenv("DISCORD_ADMIN_ROLE_ID")
	apiURL := os.Getenv("DISCORD_API_URL")
	apiKey := os.Getenv("INTERNAL_API_KEY")
		if apiKey == "" {
			utils.ErrorLog("INTERNAL_API_KEY not set, Discord bot will not be able to communicate with API")
		}
	bot, err := NewBot(token, adminRole, apiURL, apiKey)
		if err != nil {
			utils.ErrorLog("Failed to initialize Discord bot: %v", err)
			return nil, err
		}
		integration.Bot = bot
	utils.InfoLog("Discord bot initialized")
	}

	integration.initialized = true
	return integration, nil
}

// Start starts the Discord integration components
func (i *Integration) Start() error {
	if !i.Enabled || !i.initialized {
		return nil
	}
	utils.InfoLog("Starting Discord integration")
	if i.Bot != nil {
		utils.InfoLog("Starting Discord bot")
		if err := i.Bot.Start(); err != nil {
			utils.ErrorLog("Failed to start Discord bot: %v", err)
			return err
		}
	}
	return nil
}

// Stop stops the Discord integration components
func (i *Integration) Stop() {
	if !i.Enabled || !i.initialized {
		return
	}
	utils.InfoLog("Stopping Discord integration")
	if i.Bot != nil {
		utils.InfoLog("Stopping Discord bot")
		i.Bot.Stop()
	}
}

// NewBot creates a new Discord bot
func NewBot(token, adminRoleID, apiURL, apiKey string) (*Bot, error) {
	dg, err := discordgo.New("Bot " + token)
	if err != nil {
		return nil, err
	}

	bot := &Bot{
		session:          dg,
		token:            token,
		adminRoleID:      adminRoleID,
		apiURL:           strings.TrimSuffix(apiURL, "/"),
		apiKey:           apiKey,
		cleanupInterval:  30 * time.Minute,
		client:           &http.Client{Timeout: 10 * time.Second},
		pendingVODSelect: make(map[string]*vodSelectContext),
		stopChan:         make(chan struct{}),
	}

	// Optional: dev guild for registering guild-scoped commands during development
	bot.devGuildID = os.Getenv("DISCORD_DEV_GUILD_ID")

	// Register handlers
	// Legacy messageCreate kept for now but can be removed once slash migration is complete.
	// Commented out to prioritize slash commands migration.
	// dg.AddHandler(bot.messageCreate)
	// Handle interactions (components + application commands)
	dg.AddHandler(bot.handleInteractionCreate)
	dg.AddHandler(bot.handleApplicationCommand)
	dg.AddHandler(func(s *discordgo.Session, r *discordgo.Ready) {
		// Polished ready log
		if s != nil && s.State != nil && s.State.User != nil {
			utils.InfoLog("Discord ready: %s#%s (%s)", s.State.User.Username, s.State.User.Discriminator, s.State.User.ID)
		} else {
			utils.InfoLog("Discord ready: session state not populated yet")
		}
	})

	// Intents: messages, DMs, message content
	dg.Identify.Intents = discordgo.IntentGuilds |
		discordgo.IntentGuildMessages |
		discordgo.IntentDirectMessages |
		discordgo.IntentMessageContent

	// Start cleanup routine
	go bot.cleanupRoutine()

	return bot, nil
}

// Start starts the Discord bot
func (b *Bot) Start() error {
	utils.InfoLog("Starting Discord bot with intents: Guilds, GuildMessages, DirectMessages, MessageContent, Reactions")
	if err := b.session.Open(); err != nil { return err }
	// Register slash commands once here to avoid duplicate registrations on reconnects
	utils.InfoLog("Slash commands: registering %s-scoped commands…", func() string { if b.devGuildID != "" { return "guild" } ; return "global" }())
	if err := b.cleanupExistingCommands(); err != nil {
		utils.WarnLog("Failed to cleanup existing commands: %v", err)
	}
	if err := b.registerSlashCommands(); err != nil {
		utils.ErrorLog("Failed to register slash commands: %v", err)
	}
	if b.devGuildID == "" {
		utils.WarnLog("Slash commands registered globally; this can take up to 1 hour to appear. Set DISCORD_DEV_GUILD_ID to register instantly in a guild during development.")
	}
	return nil
}

// Stop stops the Discord bot and its background goroutines.
func (b *Bot) Stop() {
	utils.InfoLog("Stopping Discord bot")
	close(b.stopChan)
	// Attempt to delete commands (guild-scoped for fast iteration)
	if err := b.unregisterSlashCommands(); err != nil {
		utils.WarnLog("Failed to unregister slash commands: %v", err)
	}
	b.session.Close()
}

// cleanupRoutine periodically cleans up expired data.
// It exits when b.stopChan is closed.
func (b *Bot) cleanupRoutine() {
	ticker := time.NewTicker(b.cleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-b.stopChan:
			return
		case <-ticker.C:
			b.cleanupExpiredVODSelects()
		}
	}
}

// cleanupExpiredVODSelects removes old interactive contexts to prevent leaks
func (b *Bot) cleanupExpiredVODSelects() {
	b.selectLock.Lock()
	defer b.selectLock.Unlock()
	// expire after 1 hour
	cutoff := time.Now().Add(-1 * time.Hour)
	for msgID, ctx := range b.pendingVODSelect {
		if ctx.Created.Before(cutoff) {
			delete(b.pendingVODSelect, msgID)
		}
	}
}

// Starts VOD download for the given selection and informs the user
func (b *Bot) startVODDownloadFromSelection(s *discordgo.Session, channelID, userID string, selectedVOD types.VODResult) {
	// Get LDAP username for this Discord user
	success, respData, err := b.makeAPIRequest("GET", "/discord/"+userID+"/ldap", nil)
	if err != nil || !success {
		b.fail(channelID, "❌ Download Failed", "Failed to retrieve your user information. Please try again later.")
		return
	}

	data, ok := respData.(map[string]interface{})
	if !ok {
		b.fail(channelID, "❌ Download Failed", "Failed to process server response.")
		return
	}
	ldapUser, ok := data["ldap_user"].(string)
	if !ok || ldapUser == "" {
		b.warn(channelID, "🔗 Linking Required", "Your Discord account is not linked to an IPTV user.\n\nPlease link it first:\n`!link <ldap_username>`")
		return
	}

	// Send download request to API
	downloadData := map[string]string{
		"username":  ldapUser,
		"stream_id": selectedVOD.StreamID,
		"title":     selectedVOD.Title,
		"type":      selectedVOD.StreamType,
	}
	success, respData, err = b.makeAPIRequest("POST", "/vod/download", downloadData)
	if err != nil || !success {
		errMsg := "Failed to create download"
		if err != nil {
			errMsg += ": " + err.Error()
		} else if respData != nil {
			if errData, ok := respData.(map[string]interface{}); ok {
				if errStr, ok := errData["Error"].(string); ok {
					errMsg += ": " + errStr
				}
			}
		}
		b.fail(channelID, "❌ Download Failed", errMsg)
		return
	}

	// Process download response
	data, ok = respData.(map[string]interface{})
	if !ok {
		b.fail(channelID, "❌ Download Failed", "Failed to process download response.")
		return
	}
	downloadURL, ok := data["download_url"].(string)
	if !ok || downloadURL == "" {
		b.fail(channelID, "❌ Download Failed", "Failed to get download URL.")
		return
	}

	// Format expiration time if available
	var expirationInfo string
	if expiry, ok := data["expires_at"].(string); ok && strings.TrimSpace(expiry) != "" {
		expirationInfo = fmt.Sprintf("\nThis link will expire after %s", expiry)
	}

	// Build a prettier success embed with a link button
	titleText := selectedVOD.Title
	if selectedVOD.SeriesTitle != "" && selectedVOD.Episode > 0 {
		// Prefer series formatting when available
		titleText = fmt.Sprintf("%s — S%02dE%02d %s", selectedVOD.SeriesTitle, selectedVOD.Season, selectedVOD.Episode, selectedVOD.EpisodeTitle)
	}

	desc := "Your download is ready."
	if expirationInfo != "" {
		desc += "\n" + expirationInfo
	}

	fields := []*discordgo.MessageEmbedField{}
	if selectedVOD.Year != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Year", Value: selectedVOD.Year, Inline: true})
	}
	if selectedVOD.Rating != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Rating", Value: "⭐ " + selectedVOD.Rating, Inline: true})
	}
	if selectedVOD.Size != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Size", Value: selectedVOD.Size, Inline: true})
	}
	if selectedVOD.Duration != "" {
		fields = append(fields, &discordgo.MessageEmbedField{Name: "Duration", Value: selectedVOD.Duration, Inline: true})
	}

	embed := &discordgo.MessageEmbed{
		Title:       "✅ Download Ready — " + titleText,
		Description: desc,
		Color:       colorSuccess,
		Fields:      fields,
		Timestamp:   time.Now().UTC().Format(time.RFC3339),
	}

	components := []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Style: discordgo.LinkButton, Label: "Open Download", URL: downloadURL},
		}},
	}

	if _, err := s.ChannelMessageSendComplex(channelID, &discordgo.MessageSend{Embeds: []*discordgo.MessageEmbed{embed}, Components: components}); err != nil {
		utils.ErrorLog("Discord: failed to send download embed: %v", err)
		// Fallback to plain embed without button
		b.success(channelID, "✅ Download Ready", desc, &discordgo.MessageEmbedField{Name: "Download Link", Value: fmt.Sprintf("[Click here to download](%s)", downloadURL)})
	}
}
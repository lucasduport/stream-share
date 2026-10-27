/*
 * stream-share is a project to efficiently share the use of an IPTV service.
 * Copyright (C) 2025  Lucas Duport
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License, version 3 or later.
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
	"net/url"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/lucasduport/stream-share/pkg/utils"
)

// Live share links from Discord: /share picks a channel and a window and
// returns a link a guest opens in VLC or an IPTV app; /shares lists and
// revokes them. Links (they are credentials) are only ever shown in ephemeral
// messages.

const (
	shareSelectID       = "share_select"
	shareRevokePrefix   = "share_revoke:"
	shareListMaxButtons = 20
)

// shareSelectContext is a /share request waiting for the channel to be picked.
type shareSelectContext struct {
	UserID   string
	LDAPUser string
	Start    time.Time
	End      time.Time
	Channels map[string]string // stream_id -> display label
	Created  time.Time
}

// followupEphemeral posts an ephemeral follow-up to an acknowledged
// interaction and returns its message ID ("" on failure).
func followupEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string, components []discordgo.MessageComponent) string {
	msg, err := s.FollowupMessageCreate(i.Interaction, true, &discordgo.WebhookParams{
		Content:    content,
		Components: components,
		Flags:      discordgo.MessageFlagsEphemeral,
	})
	if err != nil {
		utils.WarnLog("Discord: share follow-up failed: %v", err)
		return ""
	}
	return msg.ID
}

// ldapUserOf returns the LDAP user linked to the interaction's Discord user.
func (b *Bot) ldapUserOf(i *discordgo.InteractionCreate) (string, error) {
	uid := interactionUserID(i)
	ok, resp, err := b.makeAPIRequest("GET", "/discord/"+uid+"/ldap", nil)
	if err != nil || !ok {
		return "", fmt.Errorf("could not look up your account, try again later")
	}
	data, _ := resp.(map[string]interface{})
	if u := getString(data, "ldap_user"); u != "" {
		return u, nil
	}
	return "", fmt.Errorf("your Discord account is not linked to an IPTV user: run `/link <ldap_username>` first")
}

func interactionUserID(i *discordgo.InteractionCreate) string {
	if i.Member != nil && i.Member.User != nil {
		return i.Member.User.ID
	}
	if i.User != nil {
		return i.User.ID
	}
	return ""
}

// handleShareCommand runs /share channel:<query> until:<end> [start:<start>].
func (b *Bot) handleShareCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	query := optString(i, "channel")
	startArg := optString(i, "start")
	untilArg := optString(i, "until")
	ackEphemeral(s, i, "Looking up channels…")

	ldapUser, err := b.ldapUserOf(i)
	if err != nil {
		followupEphemeral(s, i, "❌ "+err.Error(), nil)
		return
	}
	start, end, err := parseShareWindow(startArg, untilArg, time.Now())
	if err != nil {
		followupEphemeral(s, i, "❌ "+err.Error(), nil)
		return
	}
	ok, resp, err := b.makeSlowAPIRequest("GET", "/shares/channels?q="+url.QueryEscape(query), nil)
	if err != nil || !ok {
		followupEphemeral(s, i, fmt.Sprintf("❌ Channel search failed: %v", err), nil)
		return
	}
	list, _ := resp.([]interface{})
	if len(list) == 0 {
		followupEphemeral(s, i, fmt.Sprintf("No live channel matches %q.", query), nil)
		return
	}

	options := make([]discordgo.SelectMenuOption, 0, len(list))
	channels := make(map[string]string, len(list))
	for _, it := range list {
		m, _ := it.(map[string]interface{})
		id, name, num := getString(m, "stream_id"), getString(m, "name"), getString(m, "num")
		if id == "" {
			continue
		}
		label := truncate(name, 100)
		desc := "id " + id
		if num != "" {
			desc = "n°" + num + " · " + desc
		}
		options = append(options, discordgo.SelectMenuOption{Label: label, Value: id, Description: desc})
		channels[id] = name
	}
	content := fmt.Sprintf("📺 Pick the channel to share, %s → %s:", shareFmt(start), shareFmt(end))
	msgID := followupEphemeral(s, i, content, []discordgo.MessageComponent{
		discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: shareSelectID, Placeholder: "Channel", Options: options},
		}},
	})
	if msgID == "" {
		return
	}
	b.shareLock.Lock()
	b.pendingShareSelect[msgID] = &shareSelectContext{
		UserID: interactionUserID(i), LDAPUser: ldapUser, Start: start, End: end, Channels: channels, Created: time.Now(),
	}
	b.shareLock.Unlock()
}

// handleShareSelect creates the link for the picked channel.
func (b *Bot) handleShareSelect(s *discordgo.Session, i *discordgo.InteractionCreate) {
	msgID := i.Message.ID
	b.shareLock.Lock()
	ctx, ok := b.pendingShareSelect[msgID]
	if ok && b.isSameUser(ctx.UserID, i) {
		delete(b.pendingShareSelect, msgID)
	}
	b.shareLock.Unlock()
	if !ok || !b.isSameUser(ctx.UserID, i) {
		return
	}
	values := i.MessageComponentData().Values
	if len(values) == 0 {
		return
	}
	id := values[0]
	payload := map[string]interface{}{
		"username":  ctx.LDAPUser,
		"stream_id": id,
		"starts_at": ctx.Start.Format(time.RFC3339),
		"ends_at":   ctx.End.Format(time.RFC3339),
	}
	content := ""
	ok, resp, err := b.makeSlowAPIRequest("POST", "/shares", payload)
	if err != nil || !ok {
		content = fmt.Sprintf("❌ Could not create the share link: %v", err)
	} else {
		content = shareCreatedMessage(resp, ctx.Channels[id])
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseUpdateMessage,
		Data: &discordgo.InteractionResponseData{Content: content, Components: []discordgo.MessageComponent{}},
	})
}

// shareCreatedMessage renders a new link for its creator.
func shareCreatedMessage(resp interface{}, channel string) string {
	m, _ := resp.(map[string]interface{})
	urls, _ := m["urls"].(map[string]interface{})
	start, _ := time.Parse(time.RFC3339, getString(m, "starts_at"))
	end, _ := time.Parse(time.RFC3339, getString(m, "ends_at"))
	if channel == "" {
		channel = getString(m, "title")
	}
	var b strings.Builder
	fmt.Fprintf(&b, "✅ **%s** is shared from **%s** to **%s**.\n\n", channel, shareFmt(start.Local()), shareFmt(end.Local()))
	fmt.Fprintf(&b, "Playlist (VLC, IPTV apps): %s\n", getString(urls, "playlist"))
	fmt.Fprintf(&b, "Direct stream: %s\n\n", getString(urls, "stream"))
	b.WriteString("Send one of these links to your guest. In VLC: *Media → Open Network Stream* and paste it. ")
	fmt.Fprintf(&b, "Up to 3 devices at once; it stops working at the end time. Your own viewing has priority. Revoke it any time with `/shares`.")
	return b.String()
}

// handleSharesList runs /shares: the caller's links (every link for admins),
// with a revoke button for those not over yet.
func (b *Bot) handleSharesList(s *discordgo.Session, i *discordgo.InteractionCreate) {
	ackEphemeral(s, i, "Fetching share links…")
	endpoint := "/shares"
	if !b.isAdmin(i.Member) {
		ldapUser, err := b.ldapUserOf(i)
		if err != nil {
			followupEphemeral(s, i, "❌ "+err.Error(), nil)
			return
		}
		endpoint += "?username=" + url.QueryEscape(ldapUser)
	}
	ok, resp, err := b.makeAPIRequest("GET", endpoint, nil)
	if err != nil || !ok {
		followupEphemeral(s, i, fmt.Sprintf("❌ Could not list share links: %v", err), nil)
		return
	}
	list, _ := resp.([]interface{})
	if len(list) == 0 {
		followupEphemeral(s, i, "No share links. Create one with `/share`.", nil)
		return
	}
	content, buttons := renderShareList(list)
	followupEphemeral(s, i, content, shareButtonRows(buttons))
}

// renderShareList formats links for /shares and returns revoke buttons for
// the scheduled and active ones.
func renderShareList(list []interface{}) (string, []discordgo.Button) {
	var b strings.Builder
	b.WriteString("🔗 **Share links**\n")
	var buttons []discordgo.Button
	for n, it := range list {
		m, _ := it.(map[string]interface{})
		token, state := getString(m, "token"), getString(m, "state")
		start, _ := time.Parse(time.RFC3339, getString(m, "starts_at"))
		end, _ := time.Parse(time.RFC3339, getString(m, "ends_at"))
		urls, _ := m["urls"].(map[string]interface{})
		icon := map[string]string{"active": "🟢", "scheduled": "🕒", "ended": "⚪", "revoked": "⛔"}[state]
		fmt.Fprintf(&b, "%s **%d. %s** — %s → %s (%s, by %s)\n", icon, n+1, getString(m, "title"),
			shareFmt(start.Local()), shareFmt(end.Local()), state, getString(m, "created_by"))
		if state == "active" || state == "scheduled" {
			fmt.Fprintf(&b, "   %s\n", getString(urls, "playlist"))
			if len(buttons) < shareListMaxButtons {
				buttons = append(buttons, discordgo.Button{
					Label: fmt.Sprintf("Revoke %d", n+1), Style: discordgo.DangerButton, CustomID: shareRevokePrefix + token,
				})
			}
		}
	}
	return truncate(b.String(), 1990), buttons
}

func shareButtonRows(buttons []discordgo.Button) []discordgo.MessageComponent {
	var rows []discordgo.MessageComponent
	for start := 0; start < len(buttons); start += 5 {
		end := start + 5
		if end > len(buttons) {
			end = len(buttons)
		}
		row := discordgo.ActionsRow{}
		for _, btn := range buttons[start:end] {
			row.Components = append(row.Components, btn)
		}
		rows = append(rows, row)
	}
	return rows
}

// handleShareRevoke revokes the link named by a /shares button. Admins may
// revoke any link, others only their own (the API checks the creator).
func (b *Bot) handleShareRevoke(s *discordgo.Session, i *discordgo.InteractionCreate, token string) {
	endpoint := "/shares/" + url.PathEscape(token)
	if !b.isAdmin(i.Member) {
		ldapUser, err := b.ldapUserOf(i)
		if err != nil {
			respondEphemeral(s, i, "❌ "+err.Error())
			return
		}
		endpoint += "?username=" + url.QueryEscape(ldapUser)
	}
	ok, resp, err := b.makeAPIRequest("DELETE", endpoint, nil)
	if err != nil || !ok {
		respondEphemeral(s, i, fmt.Sprintf("❌ Could not revoke: %v", err))
		return
	}
	m, _ := resp.(map[string]interface{})
	respondEphemeral(s, i, fmt.Sprintf("⛔ Revoked the share link for **%s**; its viewers were disconnected.", getString(m, "title")))
}

func respondEphemeral(s *discordgo.Session, i *discordgo.InteractionCreate, content string) {
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseChannelMessageWithSource,
		Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, Content: content},
	})
}

// cleanupExpiredShareSelects drops /share pickers left unanswered for an hour.
func (b *Bot) cleanupExpiredShareSelects() {
	b.shareLock.Lock()
	defer b.shareLock.Unlock()
	cutoff := time.Now().Add(-time.Hour)
	for id, ctx := range b.pendingShareSelect {
		if ctx.Created.Before(cutoff) {
			delete(b.pendingShareSelect, id)
		}
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

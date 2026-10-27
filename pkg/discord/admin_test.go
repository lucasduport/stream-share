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
	"testing"

	"github.com/bwmarrin/discordgo"
)

// Without an admin role, only server managers are admin: /shares has no
// Discord-side gate, so a plain member must not see or revoke every link.
func TestIsAdmin(t *testing.T) {
	member := func(perms int64, roles ...string) *discordgo.Member {
		return &discordgo.Member{Permissions: perms, Roles: roles}
	}
	cases := []struct {
		name   string
		roleID string
		member *discordgo.Member
		want   bool
	}{
		{"no role, DM", "", nil, false},
		{"no role, plain member", "", member(discordgo.PermissionSendMessages), false},
		{"no role, manage server", "", member(discordgo.PermissionManageGuild), true},
		{"no role, administrator", "", member(discordgo.PermissionAdministrator), true},
		{"role, DM", "42", nil, false},
		{"role, holder", "42", member(0, "7", "42"), true},
		{"role, non-holder", "42", member(0, "7"), false},
		{"role, administrator without it", "42", member(discordgo.PermissionAdministrator), false},
	}
	for _, c := range cases {
		b := &Bot{adminRoleID: c.roleID}
		if got := b.isAdmin(c.member); got != c.want {
			t.Errorf("%s: isAdmin = %v, want %v", c.name, got, c.want)
		}
	}
}

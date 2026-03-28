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
 
package database

import (
    "database/sql"
    "errors"
    "fmt"

    "github.com/lucasduport/stream-share/pkg/utils"
)

// LinkDiscordToLDAP maps a Discord user ID to an LDAP username
func (m *DBManager) LinkDiscordToLDAP(discordID, discordName, ldapUsername string) error {
    utils.DebugLog("Database: Linking Discord ID %s (%s) to LDAP user %s", discordID, discordName, ldapUsername)
    if m == nil || m.db == nil {
        return fmt.Errorf("database not initialized")
    }

    stmt := `
        INSERT INTO discord_ldap_mapping (discord_id, discord_name, ldap_username) 
        VALUES ($1, $2, $3)
        ON CONFLICT(discord_id) DO UPDATE SET 
          discord_name = EXCLUDED.discord_name,
          ldap_username = EXCLUDED.ldap_username,
          last_active = CURRENT_TIMESTAMP
    `
    _, err := m.db.Exec(stmt, discordID, discordName, ldapUsername)
    if err != nil {
        utils.ErrorLog("Database error linking Discord to LDAP: %v", err)
        return err
    }
    utils.InfoLog("Successfully linked Discord ID %s to LDAP user %s", discordID, ldapUsername)
    return nil
}

// GetLDAPUserByDiscordID retrieves the LDAP username for a Discord ID
func (m *DBManager) GetLDAPUserByDiscordID(discordID string) (string, error) {
    utils.DebugLog("Database: Getting LDAP user for Discord ID %s", discordID)
    if m == nil || m.db == nil {
        return "", fmt.Errorf("database not initialized")
    }

    var ldapUsername string
    err := m.db.QueryRow(`
        SELECT ldap_username FROM discord_ldap_mapping 
        WHERE discord_id = $1
    `, discordID).Scan(&ldapUsername)

    if errors.Is(err, sql.ErrNoRows) {
        utils.DebugLog("No LDAP user found for Discord ID %s", discordID)
        return "", fmt.Errorf("no LDAP user linked to Discord ID %s", discordID)
    }
    if err != nil {
        utils.ErrorLog("Database error getting LDAP user: %v", err)
        return "", err
    }
    utils.DebugLog("Found LDAP user %s for Discord ID %s", ldapUsername, discordID)
    return ldapUsername, nil
}

// GetDiscordByLDAPUser retrieves Discord info for an LDAP username
func (m *DBManager) GetDiscordByLDAPUser(ldapUsername string) (string, string, error) {
    utils.DebugLog("Database: Getting Discord info for LDAP user %s", ldapUsername)
    if m == nil || m.db == nil {
        return "", "", fmt.Errorf("database not initialized")
    }

    var discordID, discordName string
    err := m.db.QueryRow(`
        SELECT discord_id, discord_name FROM discord_ldap_mapping 
        WHERE ldap_username = $1
    `, ldapUsername).Scan(&discordID, &discordName)

    if errors.Is(err, sql.ErrNoRows) {
        utils.DebugLog("No Discord account linked to LDAP user %s", ldapUsername)
        return "", "", fmt.Errorf("no Discord account linked to LDAP user %s", ldapUsername)
    }
    if err != nil {
        utils.ErrorLog("Database error getting Discord info: %v", err)
        return "", "", err
    }
    utils.DebugLog("Found Discord ID %s (%s) for LDAP user %s", discordID, discordName, ldapUsername)
    return discordID, discordName, nil
}

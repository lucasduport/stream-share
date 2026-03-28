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

package cmd

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"

	homedir "github.com/mitchellh/go-homedir"
	"github.com/lucasduport/stream-share/pkg/config"
	"github.com/lucasduport/stream-share/pkg/server"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

var cfgFile string

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "stream-share",
	Short: "Proxy for IPTV streams with LDAP authentication",
	Long: `IPTV Proxy is a service that proxies IPTV streams and M3U playlists
with LDAP authentication support for secure access control.

It supports:
- M3U and M3U8 playlist proxying
- Xtream Codes API proxying
- LDAP-based authentication
- Caching for performance optimization`,

	Run: func(cmd *cobra.Command, args []string) {
		log.Printf("[stream-share] Server is starting...")

		// Parse M3U URL if provided
		m3uURL := viper.GetString("m3u-url")
		remoteHostURL, err := url.Parse(m3uURL)
		if err != nil {
			log.Fatal(err)
		}

		// Get Xtream configuration
		xtreamUser := viper.GetString("xtream-user")
		xtreamPassword := viper.GetString("xtream-password")
		xtreamBaseURL := viper.GetString("xtream-base-url")

		// Try to extract Xtream credentials from M3U URL if not explicitly provided
		var username, password string
		if strings.Contains(m3uURL, "/get.php") {
			username = remoteHostURL.Query().Get("username")
			password = remoteHostURL.Query().Get("password")
		}

		// Auto-detect Xtream service if credentials are present in the M3U URL
		if xtreamBaseURL == "" && xtreamPassword == "" && xtreamUser == "" {
			if username != "" && password != "" {
				log.Printf("[stream-share] INFO: It appears you are using an Xtream provider")
				xtreamUser = username
				xtreamPassword = password
				xtreamBaseURL = fmt.Sprintf("%s://%s", remoteHostURL.Scheme, remoteHostURL.Host)
				log.Printf("[stream-share] INFO: Xtream service enabled with base URL: %q, username: %q, password: %q",
					xtreamBaseURL, xtreamUser, xtreamPassword)
			}
		}

		// Initialize debug logging and cache folder
		config.DebugLoggingEnabled = viper.GetBool("debug-logging")
		config.CacheFolder = viper.GetString("cache-folder")
		if config.CacheFolder != "" && !strings.HasSuffix(config.CacheFolder, "/") {
			config.CacheFolder += "/"
		}

		// Create proxy configuration
		conf := &config.ProxyConfig{
			HostConfig: &config.HostConfiguration{
				Hostname: viper.GetString("hostname"),
				Port:     viper.GetInt("port"),
			},
			RemoteURL:            remoteHostURL,
			XtreamUser:           config.CredentialString(xtreamUser),
			XtreamPassword:       config.CredentialString(xtreamPassword),
			XtreamBaseURL:        xtreamBaseURL,
			M3UCacheExpiration:   viper.GetInt("m3u-cache-expiration"),
			User:                 config.CredentialString(viper.GetString("user")),
			Password:             config.CredentialString(viper.GetString("password")),
			AdvertisedPort:       viper.GetInt("advertised-port"),
			HTTPS:                viper.GetBool("https"),
			M3UFileName:          viper.GetString("m3u-file-name"),
			CustomEndpoint:       viper.GetString("custom-endpoint"),
			CustomId:             viper.GetString("custom-id"),
			XtreamGenerateApiGet: viper.GetBool("xtream-api-get"),
			// LDAP configuration
			LDAPEnabled:          viper.GetBool("ldap-enabled"),
			LDAPServer:           viper.GetString("ldap-server"),
			LDAPBaseDN:           viper.GetString("ldap-base-dn"),
			LDAPBindDN:           viper.GetString("ldap-bind-dn"),
			LDAPBindPassword:     viper.GetString("ldap-bind-password"),
			LDAPUserAttribute:    viper.GetString("ldap-user-attribute"),
			LDAPGroupAttribute:   viper.GetString("ldap-group-attribute"),
			LDAPRequiredGroup:    viper.GetString("ldap-required-group"),
		}

		// Use port if advertised port is not specified
		if conf.AdvertisedPort == 0 {
			conf.AdvertisedPort = conf.HostConfig.Port
		}

		// Initialize and start the server
		server, err := server.NewServer(conf)
		if err != nil {
			log.Fatal(err)
		}

		if err := server.Serve(); err != nil {
			log.Fatal(err)
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func init() {
	cobra.OnInitialize(initConfig)

	// Config file flag
	rootCmd.PersistentFlags().StringVar(&cfgFile, "config", "", "Config file (default is $HOME/.stream-share.yaml)")

	// Basic configuration flags
	rootCmd.Flags().StringP("m3u-url", "u", "", "M3U file URL or local path")
	rootCmd.Flags().StringP("m3u-file-name", "", "iptv.m3u", "Name of the generated M3U file")
	rootCmd.Flags().StringP("custom-endpoint", "", "", "Custom endpoint path")
	rootCmd.Flags().StringP("custom-id", "", "", "Custom anti-collision ID")
	rootCmd.Flags().Int("port", 8080, "Listening port")
	rootCmd.Flags().Int("advertised-port", 0, "Port to use in generated URLs (for reverse proxy)")
	rootCmd.Flags().String("hostname", "", "Hostname to use in generated URLs")
	rootCmd.Flags().BoolP("https", "", false, "Use HTTPS for generated URLs")
	rootCmd.Flags().Int("m3u-cache-expiration", 1, "M3U cache expiration in hours")

	// Authentication configuration 
	rootCmd.Flags().String("user", "usertest", "Username for basic authentication when LDAP is not enabled")
	rootCmd.Flags().String("password", "passwordtest", "Password for basic authentication when LDAP is not enabled")

	// Xtream API configuration
	rootCmd.Flags().String("xtream-user", "", "Username for accessing the upstream Xtream API")
	rootCmd.Flags().String("xtream-password", "", "Password for accessing the upstream Xtream API")
	rootCmd.Flags().String("xtream-base-url", "", "Base URL of the upstream Xtream API service")
	rootCmd.Flags().BoolP("xtream-api-get", "", false, "Generate get.php endpoint from API data")

	// LDAP authentication configuration
	rootCmd.Flags().Bool("ldap-enabled", false, "Enable LDAP authentication instead of basic auth")
	rootCmd.Flags().String("ldap-server", "", "LDAP server URL (e.g., ldap://ldap.example.com:389)")
	rootCmd.Flags().String("ldap-base-dn", "", "Base DN for LDAP user search")
	rootCmd.Flags().String("ldap-bind-dn", "", "DN for binding to LDAP server (service account)")
	rootCmd.Flags().String("ldap-bind-password", "", "Password for LDAP bind DN")
	rootCmd.Flags().String("ldap-user-attribute", "uid", "LDAP username attribute")
	rootCmd.Flags().String("ldap-group-attribute", "memberOf", "LDAP group attribute")
	rootCmd.Flags().String("ldap-required-group", "iptv", "Required LDAP group")

	// Bind all flags to viper
	if err := viper.BindPFlags(rootCmd.Flags()); err != nil {
		log.Fatal("Error binding PFlags to viper")
	}
}

// initConfig reads in config file and ENV variables if set
func initConfig() {
	if cfgFile != "" {
		// Use config file from the flag
		viper.SetConfigFile(cfgFile)
	} else {
		// Find home directory
		home, err := homedir.Dir()
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}

		// Search config in home directory and current directory
		viper.AddConfigPath(home)
		viper.AddConfigPath(".")
		viper.SetConfigName(".stream-share")
	}

	// Replace hyphens with underscores in environment variables
	viper.SetEnvKeyReplacer(strings.NewReplacer("-", "_"))

	// Read environment variables
	viper.AutomaticEnv()

	// Read in config file if found
	if err := viper.ReadInConfig(); err == nil {
		fmt.Println("Using config file:", viper.ConfigFileUsed())
	}
}

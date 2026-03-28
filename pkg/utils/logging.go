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
 
 package utils

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// LogConfig defines logging configuration
var Config = struct {
	DebugLoggingEnabled bool
	LogLevel            LogLevel
	LogToFile           bool
	LogFilePath         string
	logFile             *os.File
}{
	DebugLoggingEnabled: false,
	LogLevel:            LevelInfo,
	LogToFile:           false,
}

// LogLevel represents logging levels
type LogLevel int

const (
	LevelDebug LogLevel = iota
	LevelInfo
	LevelWarn
	LevelError
)

func init() {
	// Initialize logging configuration from environment
	Config.DebugLoggingEnabled = os.Getenv("DEBUG_LOGGING") == "true"
	
	// Set log level from environment
	logLevel := strings.ToLower(os.Getenv("LOG_LEVEL"))
	switch logLevel {
	case "debug":
		Config.LogLevel = LevelDebug
	case "info":
		Config.LogLevel = LevelInfo
	case "warn":
		Config.LogLevel = LevelWarn
	case "error":
		Config.LogLevel = LevelError
	default:
		if Config.DebugLoggingEnabled {
			Config.LogLevel = LevelDebug
		} else {
			Config.LogLevel = LevelInfo
		}
	}
	
	// Configure file logging if requested
	logFilePath := os.Getenv("LOG_FILE")
	if logFilePath != "" {
		Config.LogToFile = true
		Config.LogFilePath = logFilePath
		
		// Create log directory if it doesn't exist
		logDir := filepath.Dir(logFilePath)
		if err := os.MkdirAll(logDir, 0755); err != nil {
			log.Printf("Error creating log directory: %v", err)
		}
		
		// Open log file
		file, err := os.OpenFile(logFilePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			log.Printf("Error opening log file: %v", err)
		} else {
			Config.logFile = file
			log.SetOutput(file)
		}
	}
	
	// Log initial configuration
	InfoLog("Logging initialized - Debug: %v, Level: %s", 
		Config.DebugLoggingEnabled, levelToString(Config.LogLevel))
}

// Close closes any open log files
func Close() {
	if Config.logFile != nil {
		Config.logFile.Close()
	}
}

// InfoLog logs an info message
func InfoLog(format string, v ...interface{}) {
	if Config.LogLevel <= LevelInfo {
		logWithCaller(LevelInfo, format, v...)
	}
}

// WarnLog logs a warning message
func WarnLog(format string, v ...interface{}) {
	if Config.LogLevel <= LevelWarn {
		logWithCaller(LevelWarn, format, v...)
	}
}

// DebugLog logs a debug message if debug logging is enabled
func DebugLog(format string, v ...interface{}) {
	if Config.DebugLoggingEnabled {
		logWithCaller(LevelDebug, format, v...)
	}
}

// ErrorLog logs an error message
func ErrorLog(format string, v ...interface{}) {
	if Config.LogLevel <= LevelError {
		logWithCaller(LevelError, format, v...)
	}
}

// logWithCaller logs a message with caller information
func logWithCaller(level LogLevel, format string, v ...interface{}) {
	// Get caller information
	_, file, line, ok := runtime.Caller(2)
	caller := "unknown"
	if ok {
		// Get just the filename without the path
		caller = fmt.Sprintf("%s:%d", filepath.Base(file), line)
	}
	
	// Format message with timestamp and level
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	levelStr := levelToString(level)
	
	// Format the final message
	message := fmt.Sprintf(format, v...)
	logMessage := fmt.Sprintf("%s [%s] (%s) %s", 
		timestamp, levelStr, caller, message)
	
	// Log to standard output
	log.Println(logMessage)
}

// levelToString converts a LogLevel to its string representation
func levelToString(level LogLevel) string {
	switch level {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}

// // PrintErrorAndReturn is a convenience function for returning errors
// func PrintErrorAndReturn(err error) error {
// 	ErrorLog("%v", err)
// 	return err
// }

// CreateSampleStreamData creates sample stream data for testing
func CreateSampleStreamData() map[string]interface{} {
	// This function helps create test data for debugging
	return map[string]interface{}{
		"active_streams": 3,
		"popular_stream": "CNN",
		"peak_time":      "20:00-22:00",
		"total_bytes":    "2.3 GB",
	}
}

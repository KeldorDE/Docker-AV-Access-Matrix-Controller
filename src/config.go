package main

import (
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultMatrixHost             = "192.168.178.10"
	defaultMatrixPort             = 23
	defaultMatrixTimeout          = 2 * time.Second
	defaultMatrixCommandDelay     = 1 * time.Second
	defaultStatusPollInterval     = 3 * time.Second
	defaultFullSyncInterval       = 60 * time.Second
	defaultSSEKeepaliveInterval   = 30 * time.Second
	defaultMatrixInputs           = 4
	defaultMatrixOutputs          = 4
	defaultHTTPHost               = "0.0.0.0"
	defaultHTTPPort               = 62225
	matrixManufacturer            = "AV Access"
	legacyStatusPollIntervalEnvar = "HDMI_MATRIX_STATUS_POLL_INTERVAL"
)

type config struct {
	matrixHost             string
	matrixPort             int
	matrixTimeout          time.Duration
	matrixCommandDelay     time.Duration
	statusPollInterval     time.Duration
	fullSyncInterval       time.Duration
	sseKeepaliveInterval   time.Duration
	matrixInputs           int
	matrixOutputs          int
	matrixConfigurationURL string
	httpHost               string
	httpPort               int
	logLevel               string
}

func loadConfig() config {
	return config{
		matrixHost:             envString("HDMI_MATRIX_IP", defaultMatrixHost),
		matrixPort:             envInt("HDMI_MATRIX_PORT", defaultMatrixPort),
		matrixTimeout:          envDurationSeconds("HDMI_MATRIX_TIMEOUT", defaultMatrixTimeout),
		matrixCommandDelay:     envDurationSeconds("HDMI_MATRIX_COMMAND_DELAY", defaultMatrixCommandDelay),
		statusPollInterval:     envDurationSeconds("STATUS_POLL_INTERVAL", defaultStatusPollInterval),
		fullSyncInterval:       fullSyncInterval(),
		sseKeepaliveInterval:   envDurationSeconds("SSE_KEEPALIVE_INTERVAL", defaultSSEKeepaliveInterval),
		matrixInputs:           envInt("HDMI_MATRIX_INPUTS", 0),
		matrixOutputs:          envInt("HDMI_MATRIX_OUTPUTS", 0),
		matrixConfigurationURL: envString("HDMI_MATRIX_CONFIGURATION_URL", ""),
		httpHost:               envString("HTTP_HOST", defaultHTTPHost),
		httpPort:               envInt("HTTP_PORT", defaultHTTPPort),
		logLevel:               strings.ToUpper(envString("LOG_LEVEL", "INFO")),
	}
}

// fullSyncInterval liest das Intervall des vollständigen Matrix-Syncs. Der
// frühere Name HDMI_MATRIX_STATUS_POLL_INTERVAL bleibt als Alias gültig, damit
// bestehende Deployments unverändert weiterlaufen.
func fullSyncInterval() time.Duration {
	fallback := envDurationSeconds(legacyStatusPollIntervalEnvar, defaultFullSyncInterval)
	return envDurationSeconds("FULL_SYNC_INTERVAL", fallback)
}

func envString(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func envInt(name string, fallback int) int {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	number, err := strconv.Atoi(value)
	if err != nil {
		log.Fatalf("Invalid %s=%q: %v", name, value, err)
	}
	return number
}

// envDurationSeconds akzeptiert reine Sekundenwerte ("60", "0.5") und
// Go-Dauerangaben ("3s", "1m30s").
func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}

	if seconds, err := strconv.ParseFloat(value, 64); err == nil {
		return time.Duration(seconds * float64(time.Second))
	}

	duration, err := time.ParseDuration(value)
	if err != nil {
		log.Fatalf("Invalid %s=%q: %v", name, value, err)
	}

	return duration
}

var debugEnabled bool

func debugf(format string, args ...any) {
	if debugEnabled {
		log.Printf("DEBUG "+format, args...)
	}
}

// Package config loads Drydock configuration from environment variables.
package config

import (
	"os"
	"strconv"
)

// Config holds all runtime configuration for Drydock.
type Config struct {
	Addr           string // listen address, e.g. ":8080"
	DataDir        string // directory holding drydock.db
	RegOpen        bool   // whether /api/auth/register is enabled
	DefaultImage   string // default container image for new task containers
	AgentRuntime   string // default agent runtime for new cards ("echo", "pi", "zot", ...)
	TrustedHeader  string // e.g. "X-Remote-User": nginx-OAuth trusted header; "" disables
	SecureCookie   bool   // set session cookie Secure flag (enable behind TLS)
	DockerHost     string // resolved container runtime endpoint (informational)
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Addr:          envOr("DRYDOCK_ADDR", ":8080"),
		DataDir:       envOr("DRYDOCK_DATA_DIR", "./data"),
		RegOpen:       envBool("DRYDOCK_REG_OPEN", true),
		DefaultImage:  envOr("DRYDOCK_DEFAULT_IMAGE", "ubuntu:24.04"),
		AgentRuntime:  envOr("DRYDOCK_AGENT_RUNTIME", "echo"),
		TrustedHeader: os.Getenv("DRYDOCK_TRUSTED_USER_HEADER"),
		SecureCookie:  envBool("DRYDOCK_SECURE_COOKIE", false),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}

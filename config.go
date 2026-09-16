// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// config holds registrybot's process-level configuration, all sourced from the
// environment. It is deliberately small: how to reach ConfigHub and GitHub, and
// where the bot's own configuration document lives. Everything about *what* to
// watch comes from that document (see botconfig.go), so it can be changed
// through ConfigHub without redeploying the bot.
type config struct {
	// ConfigHub connection. Exactly one credential kind must be set: a worker
	// id+secret, a private key (JWK) registered against a worker's identity, or
	// a static bearer token (local development; it is never refreshed).
	ConfigHubURL       string
	WorkerID           string
	WorkerSecret       string
	PrivateKey         string // inline JWK or a path to one
	AssertionAudience  string
	StaticToken        string
	ConfigHubUserAgent string

	// Where the bot's configuration document lives. ConfigSpace and ConfigUnit
	// name a unit in ConfigHub; ConfigFile points at a local YAML file instead
	// and is meant for development.
	ConfigSpace string
	ConfigUnit  string
	ConfigFile  string

	// GitHub API access for polling package versions. A classic personal access
	// token with read:packages (or a GitHub App installation token) works.
	GitHubToken  string
	GitHubAPIURL string

	// Webhook receiver. The secret is what GitHub signs deliveries with; when it
	// is empty the webhook endpoint is disabled and the bot polls only.
	WebhookSecret string
	ListenAddr    string

	// PollInterval is the default poll cadence. The configuration document may
	// override it.
	PollInterval time.Duration
}

const (
	defaultConfigUnit   = "registrybot"
	defaultListenAddr   = ":8080"
	defaultPollInterval = 5 * time.Minute
	defaultGitHubAPIURL = "https://api.github.com"
)

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL:      strings.TrimRight(os.Getenv("CONFIGHUB_URL"), "/"),
		WorkerID:          os.Getenv("CONFIGHUB_WORKER_ID"),
		WorkerSecret:      os.Getenv("CONFIGHUB_WORKER_SECRET"),
		PrivateKey:        os.Getenv("CONFIGHUB_AUTH_PRIVATE_KEY"),
		AssertionAudience: os.Getenv("CONFIGHUB_ASSERTION_AUDIENCE"),
		StaticToken:       os.Getenv("CONFIGHUB_TOKEN"),
		ConfigSpace:       os.Getenv("REGISTRYBOT_CONFIG_SPACE"),
		ConfigUnit:        os.Getenv("REGISTRYBOT_CONFIG_UNIT"),
		ConfigFile:        os.Getenv("REGISTRYBOT_CONFIG_FILE"),
		GitHubToken:       os.Getenv("GITHUB_TOKEN"),
		GitHubAPIURL:      strings.TrimRight(os.Getenv("GITHUB_API_URL"), "/"),
		WebhookSecret:     os.Getenv("GITHUB_WEBHOOK_SECRET"),
		ListenAddr:        os.Getenv("REGISTRYBOT_LISTEN_ADDR"),
		PollInterval:      defaultPollInterval,
	}
	cfg.ConfigHubUserAgent = "registrybot/" + version

	if cfg.ConfigUnit == "" {
		cfg.ConfigUnit = defaultConfigUnit
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultListenAddr
	}
	if cfg.GitHubAPIURL == "" {
		cfg.GitHubAPIURL = defaultGitHubAPIURL
	}
	if v := os.Getenv("REGISTRYBOT_POLL_INTERVAL"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d <= 0 {
			return config{}, fmt.Errorf("invalid REGISTRYBOT_POLL_INTERVAL %q: want a positive duration such as 5m", v)
		}
		cfg.PollInterval = d
	}

	var missing []string
	if cfg.ConfigHubURL == "" {
		missing = append(missing, "CONFIGHUB_URL")
	}
	if cfg.GitHubToken == "" {
		missing = append(missing, "GITHUB_TOKEN")
	}
	if cfg.ConfigFile == "" && cfg.ConfigSpace == "" {
		missing = append(missing, "REGISTRYBOT_CONFIG_SPACE (or REGISTRYBOT_CONFIG_FILE)")
	}
	if len(missing) > 0 {
		return config{}, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	// Exactly one credential kind. Ranking them silently would make "who is the
	// bot running as" depend on which variables happen to be set.
	kinds := 0
	if cfg.WorkerID != "" || cfg.WorkerSecret != "" {
		if cfg.WorkerID == "" || cfg.WorkerSecret == "" {
			return config{}, fmt.Errorf("CONFIGHUB_WORKER_ID and CONFIGHUB_WORKER_SECRET must be set together")
		}
		kinds++
	}
	if cfg.PrivateKey != "" {
		kinds++
	}
	if cfg.StaticToken != "" {
		kinds++
	}
	switch kinds {
	case 0:
		return config{}, fmt.Errorf("no ConfigHub credential: set CONFIGHUB_WORKER_ID+CONFIGHUB_WORKER_SECRET, " +
			"CONFIGHUB_AUTH_PRIVATE_KEY, or CONFIGHUB_TOKEN")
	case 1:
	default:
		return config{}, fmt.Errorf("more than one ConfigHub credential kind is set; choose one of worker secret, private key, or token")
	}
	return cfg, nil
}

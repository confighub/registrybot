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
	// ConfigHub connection. Exactly one credential kind must be set: a private
	// key (JWK) registered against a service account, or a static bearer token
	// (local development; it is never refreshed).
	ConfigHubURL       string
	PrivateKey         string // inline JWK or a path to one
	StaticToken        string
	ConfigHubUserAgent string

	// Where the bot's configuration document comes from. ConfigSpace and
	// ConfigUnit name a unit the bot reads from ConfigHub; ConfigFile is a YAML
	// file instead, usually a mounted ConfigMap. Exactly one source is set.
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

	// MetricsEnabled turns the Prometheus metrics server on (the default). Set
	// METRICS_ENABLED=false and the bot opens no metrics port and its counters
	// stay no-ops.
	MetricsEnabled bool
	// MetricsAddr is the address the bot serves its Prometheus metrics on, at
	// /metrics. It is separate from ListenAddr so that exposing the webhook
	// receiver does not expose the metrics.
	MetricsAddr string
}

const (
	defaultConfigUnit   = "registrybot"
	defaultListenAddr   = ":8080"
	defaultMetricsAddr  = ":9090"
	defaultPollInterval = 5 * time.Minute
	defaultGitHubAPIURL = "https://api.github.com"
)

func loadConfig() (config, error) {
	cfg := config{
		ConfigHubURL:  strings.TrimRight(os.Getenv("CONFIGHUB_URL"), "/"),
		PrivateKey:    os.Getenv("CONFIGHUB_AUTH_PRIVATE_KEY"),
		StaticToken:   os.Getenv("CONFIGHUB_TOKEN"),
		ConfigSpace:   os.Getenv("REGISTRYBOT_CONFIG_SPACE"),
		ConfigUnit:    os.Getenv("REGISTRYBOT_CONFIG_UNIT"),
		ConfigFile:    os.Getenv("REGISTRYBOT_CONFIG_FILE"),
		GitHubToken:   os.Getenv("GITHUB_TOKEN"),
		GitHubAPIURL:  strings.TrimRight(os.Getenv("GITHUB_API_URL"), "/"),
		WebhookSecret: os.Getenv("GITHUB_WEBHOOK_SECRET"),
		ListenAddr:    os.Getenv("REGISTRYBOT_LISTEN_ADDR"),
		PollInterval:  defaultPollInterval,
		MetricsAddr:   os.Getenv("METRICS_ADDR"),
	}
	cfg.ConfigHubUserAgent = "registrybot/" + version

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
	if cfg.MetricsAddr == "" {
		cfg.MetricsAddr = defaultMetricsAddr
	}
	switch v := os.Getenv("METRICS_ENABLED"); v {
	case "", "true":
		cfg.MetricsEnabled = true
	case "false":
	default:
		return config{}, fmt.Errorf("invalid METRICS_ENABLED %q: want true or false", v)
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

	// Exactly one source for the configuration document. With both set, which
	// document the bot runs on would depend on a precedence nobody can see from
	// the deployment, and an edit to the other one would do nothing.
	if cfg.ConfigFile != "" {
		for name, v := range map[string]string{
			"REGISTRYBOT_CONFIG_SPACE": cfg.ConfigSpace,
			"REGISTRYBOT_CONFIG_UNIT":  cfg.ConfigUnit,
		} {
			if v != "" {
				return config{}, fmt.Errorf("REGISTRYBOT_CONFIG_FILE and %s are both set; the configuration document comes from the file or from a ConfigHub unit, so unset one (with a file, name the space for fact units in its defaults.space)", name)
			}
		}
	} else if cfg.ConfigUnit == "" {
		cfg.ConfigUnit = defaultConfigUnit
	}

	// Exactly one credential kind. Ranking them silently would make "who is the
	// bot running as" depend on which variables happen to be set.
	kinds := 0
	if cfg.PrivateKey != "" {
		kinds++
	}
	if cfg.StaticToken != "" {
		kinds++
	}
	switch kinds {
	case 0:
		return config{}, fmt.Errorf("no ConfigHub credential: set CONFIGHUB_AUTH_PRIVATE_KEY or CONFIGHUB_TOKEN")
	case 1:
	default:
		return config{}, fmt.Errorf("more than one ConfigHub credential kind is set; choose one of private key or token")
	}
	return cfg, nil
}

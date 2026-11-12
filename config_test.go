// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import "testing"

// setBaseEnv sets the required variables and clears the optional ones, so each
// case starts from a known state.
func setBaseEnv(t *testing.T) {
	t.Helper()
	t.Setenv("CONFIGHUB_URL", "https://hub.example.com")
	t.Setenv("CONFIGHUB_TOKEN", "token")
	t.Setenv("REGISTRYBOT_CONFIG_SPACE", "registrybot")
	t.Setenv("GITHUB_TOKEN", "ghp_x")
	for _, k := range []string{
		"CONFIGHUB_AUTH_PRIVATE_KEY", "REGISTRYBOT_CONFIG_UNIT", "REGISTRYBOT_CONFIG_FILE",
		"GITHUB_API_URL", "GITHUB_WEBHOOK_SECRET", "REGISTRYBOT_POLL_INTERVAL",
		"REGISTRYBOT_LISTEN_ADDR", "METRICS_ENABLED", "METRICS_ADDR",
	} {
		t.Setenv(k, "")
	}
}

func TestLoadConfigMetrics(t *testing.T) {
	t.Run("on by default at :9090", func(t *testing.T) {
		setBaseEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.MetricsEnabled || cfg.MetricsAddr != ":9090" {
			t.Errorf("MetricsEnabled = %v, MetricsAddr = %q; want true, \":9090\"", cfg.MetricsEnabled, cfg.MetricsAddr)
		}
	})

	t.Run("METRICS_ENABLED=false disables metrics", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("METRICS_ENABLED", "false")
		t.Setenv("METRICS_ADDR", ":9100")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.MetricsEnabled || cfg.MetricsAddr != ":9100" {
			t.Errorf("MetricsEnabled = %v, MetricsAddr = %q; want false, \":9100\"", cfg.MetricsEnabled, cfg.MetricsAddr)
		}
	})

	t.Run("other values are refused", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("METRICS_ENABLED", "no")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted METRICS_ENABLED=no")
		}
	})
}

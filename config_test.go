// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
)

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

func TestLoadConfigDocumentSource(t *testing.T) {
	t.Run("a unit, named registrybot unless told otherwise", func(t *testing.T) {
		setBaseEnv(t)
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConfigFile != "" || cfg.ConfigSpace != "registrybot" || cfg.ConfigUnit != "registrybot" {
			t.Errorf("source = file %q, unit %s/%s; want unit registrybot/registrybot", cfg.ConfigFile, cfg.ConfigSpace, cfg.ConfigUnit)
		}
	})

	t.Run("a file", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("REGISTRYBOT_CONFIG_SPACE", "")
		t.Setenv("REGISTRYBOT_CONFIG_FILE", "/etc/registrybot/registrybot.yaml")
		cfg, err := loadConfig()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.ConfigFile != "/etc/registrybot/registrybot.yaml" || cfg.ConfigSpace != "" || cfg.ConfigUnit != "" {
			t.Errorf("source = file %q, unit %q/%q; want the file only", cfg.ConfigFile, cfg.ConfigSpace, cfg.ConfigUnit)
		}
	})

	t.Run("neither is refused", func(t *testing.T) {
		setBaseEnv(t)
		t.Setenv("REGISTRYBOT_CONFIG_SPACE", "")
		if _, err := loadConfig(); err == nil {
			t.Error("loadConfig accepted no configuration source")
		}
	})

	for _, unitVar := range []string{"REGISTRYBOT_CONFIG_SPACE", "REGISTRYBOT_CONFIG_UNIT"} {
		t.Run("a file and "+unitVar+" are refused", func(t *testing.T) {
			setBaseEnv(t)
			t.Setenv("REGISTRYBOT_CONFIG_SPACE", "")
			t.Setenv("REGISTRYBOT_CONFIG_FILE", "/etc/registrybot/registrybot.yaml")
			t.Setenv(unitVar, "registry-facts")
			_, err := loadConfig()
			if err == nil {
				t.Fatalf("loadConfig accepted REGISTRYBOT_CONFIG_FILE with %s", unitVar)
			}
			if !strings.Contains(err.Error(), unitVar) {
				t.Errorf("error %q does not name %s", err, unitVar)
			}
		})
	}
}

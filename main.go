// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

// Command registrybot keeps ConfigHub informed about container repositories.
//
// For each repository it is configured to watch it maintains one ConfigHub
// unit — a fact unit — describing what the repository currently holds: its
// tags, their digests, and a few named streams such as "newest" and "semver"
// that pick the tag a consumer most likely wants. It learns about changes by
// polling the GitHub Packages API and, when a webhook is configured, by
// receiving GitHub package events that make it look sooner.
//
// That is its whole job. What happens when a fact unit changes — which
// deployments pick up the new tag, through which promotion steps — is
// ConfigHub's, expressed with links from the units that consume the fact.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println(version)
		return
	}
	log.SetFlags(log.LstdFlags | log.LUTC)

	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	hub, err := newHubClient(cfg)
	if err != nil {
		log.Fatalf("[FATAL] %v", err)
	}
	if err := hub.verify(ctx); err != nil {
		log.Fatalf("[FATAL] %v", err)
	}

	gh := newGitHubClient(cfg)
	rec := newReconciler(cfg, hub, gh)
	srv := newHTTPServer(cfg, rec)

	if cfg.WebhookSecret == "" {
		log.Printf("[WARN] GITHUB_WEBHOOK_SECRET is not set; webhook receiver disabled, polling only")
	}
	if cfg.ConfigFile != "" {
		log.Printf("[INFO] registrybot %s starting; configuration from file %s", version, cfg.ConfigFile)
	} else {
		log.Printf("[INFO] registrybot %s starting; configuration from unit %s/%s at %s",
			version, cfg.ConfigSpace, cfg.ConfigUnit, cfg.ConfigHubURL)
	}

	errc := make(chan error, 2)
	go func() {
		log.Printf("[INFO] http: listening on %s", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- fmt.Errorf("http server: %w", err)
		}
	}()
	go func() { errc <- rec.Run(ctx) }()

	select {
	case <-ctx.Done():
	case err := <-errc:
		if err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("[FATAL] %v", err)
			stop()
			shutdown(srv)
			os.Exit(1)
		}
	}
	shutdown(srv)
	log.Printf("[INFO] registrybot shutting down")
}

func shutdown(srv *http.Server) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

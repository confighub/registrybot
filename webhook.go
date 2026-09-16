// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// newHTTPServer serves the webhook receiver and two read-only endpoints.
//
//	POST /webhooks/github  GitHub package events; requires GITHUB_WEBHOOK_SECRET
//	GET  /healthz          liveness
//	GET  /status           what the bot is watching and when it last succeeded
//
// A webhook does not carry facts into ConfigHub. It names a repository, and if
// that repository is watched the bot reconciles it now rather than at the next
// poll. Anything a payload says about tags or digests is ignored, so a forged
// or malformed delivery can at most trigger an early look.
func newHTTPServer(cfg config, r *reconciler) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": version})
	})
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, r.snapshot())
	})
	mux.HandleFunc("POST /webhooks/github", githubWebhookHandler(cfg.WebhookSecret, r))
	return &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

const maxWebhookBody = 5 << 20

// githubPackagePayload is the slice of a package / registry_package event the
// bot reads. GitHub recommends the package event; registry_package is the
// older name for the same thing and is accepted too.
type githubPackagePayload struct {
	Action          string          `json:"action"`
	Package         *packageDetails `json:"package"`
	RegistryPackage *packageDetails `json:"registry_package"`
}

type packageDetails struct {
	Name           string `json:"name"`
	Namespace      string `json:"namespace"`
	PackageType    string `json:"package_type"`
	Ecosystem      string `json:"ecosystem"`
	PackageVersion struct {
		PackageURL string `json:"package_url"`
	} `json:"package_version"`
}

func githubWebhookHandler(secret string, r *reconciler) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		if secret == "" {
			http.NotFound(w, req)
			return
		}
		body, err := io.ReadAll(io.LimitReader(req.Body, maxWebhookBody+1))
		if err != nil || len(body) > maxWebhookBody {
			http.Error(w, "body too large or unreadable", http.StatusRequestEntityTooLarge)
			return
		}
		if !validSignature(secret, req.Header.Get("X-Hub-Signature-256"), body) {
			log.Printf("[WARN] webhook: rejected delivery %s with a bad signature", req.Header.Get("X-GitHub-Delivery"))
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}

		event := req.Header.Get("X-GitHub-Event")
		switch event {
		case "ping":
			writeJSON(w, http.StatusOK, map[string]string{"status": "pong"})
			return
		case "package", "registry_package":
		default:
			writeJSON(w, http.StatusAccepted, map[string]string{"ignored": "event " + event})
			return
		}

		var payload githubPackagePayload
		if err := json.Unmarshal(body, &payload); err != nil {
			http.Error(w, "malformed JSON", http.StatusBadRequest)
			return
		}
		details := payload.Package
		if details == nil {
			details = payload.RegistryPackage
		}
		if details == nil {
			http.Error(w, "payload has no package", http.StatusBadRequest)
			return
		}
		key, ok := repositoryFromPayload(details)
		if !ok {
			writeJSON(w, http.StatusAccepted, map[string]string{"ignored": "not a container package"})
			return
		}
		if _, watched := r.lookup(key); !watched {
			log.Printf("[INFO] webhook: %s %s for %s, which is not watched; ignored", event, payload.Action, key)
			writeJSON(w, http.StatusAccepted, map[string]any{"repository": key, "queued": false})
			return
		}
		log.Printf("[INFO] webhook: %s %s for %s; reconciling", event, payload.Action, key)
		r.enqueue(key)
		writeJSON(w, http.StatusAccepted, map[string]any{"repository": key, "queued": true})
	}
}

// repositoryFromPayload derives the canonical repository key from a package
// event. package_url ("ghcr.io/owner/name:tag") is preferred because it is
// exactly the repository; namespace and name are the fallback.
func repositoryFromPayload(d *packageDetails) (string, bool) {
	kind := strings.ToLower(d.PackageType + " " + d.Ecosystem)
	if !strings.Contains(kind, "container") && !strings.Contains(kind, "docker") {
		return "", false
	}
	candidate := d.PackageVersion.PackageURL
	if candidate == "" {
		candidate = "ghcr.io/" + strings.Trim(d.Namespace, "/") + "/" + strings.Trim(d.Name, "/")
	}
	ref, err := parseRepository(candidate)
	if err != nil {
		return "", false
	}
	return ref.String(), true
}

// validSignature checks GitHub's HMAC-SHA256 signature over the raw body.
func validSignature(secret, header string, body []byte) bool {
	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	want, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

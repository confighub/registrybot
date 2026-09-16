// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newTestReconciler(t *testing.T) *reconciler {
	t.Helper()
	r := newReconciler(config{PollInterval: defaultPollInterval, ConfigSpace: "facts"}, nil, nil)
	_, watches, err := parseBotConfig([]byte("repositories:\n  - repository: ghcr.io/confighub/argobot\n"), "facts")
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range watches {
		r.watches[w.Key] = w
	}
	return r
}

func deliver(t *testing.T, h http.Handler, secret, event string, body []byte, signed bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", strings.NewReader(string(body)))
	req.Header.Set("X-GitHub-Event", event)
	req.Header.Set("X-GitHub-Delivery", "d-1")
	if signed {
		req.Header.Set("X-Hub-Signature-256", sign(secret, body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

const packageEvent = `{"action":"published","package":{"name":"argobot","namespace":"confighub",
"package_type":"CONTAINER","ecosystem":"CONTAINER",
"package_version":{"name":"sha256:abc","package_url":"ghcr.io/confighub/argobot:v0.4.0",
"container_metadata":{"tag":{"name":"v0.4.0","digest":"sha256:abc"}}}}}`

func TestWebhookQueuesWatchedRepository(t *testing.T) {
	r := newTestReconciler(t)
	h := githubWebhookHandler("s3cret", r)

	rec := deliver(t, h, "s3cret", "package", []byte(packageEvent), true)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp["queued"] != true || resp["repository"] != "ghcr.io/confighub/argobot" {
		t.Errorf("resp: %v", resp)
	}
	select {
	case key := <-r.queue:
		if key != "ghcr.io/confighub/argobot" {
			t.Errorf("queued %q", key)
		}
	default:
		t.Error("nothing queued")
	}
}

func TestWebhookIgnoresUnwatchedAndNonContainer(t *testing.T) {
	r := newTestReconciler(t)
	h := githubWebhookHandler("s3cret", r)

	other := strings.ReplaceAll(packageEvent, "confighub/argobot", "someone/else")
	rec := deliver(t, h, "s3cret", "registry_package", []byte(strings.ReplaceAll(other, `"package":`, `"registry_package":`)), true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued":false`) {
		t.Errorf("unwatched: %d %s", rec.Code, rec.Body)
	}
	npm := strings.ReplaceAll(packageEvent, "CONTAINER", "npm")
	rec = deliver(t, h, "s3cret", "package", []byte(npm), true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "not a container") {
		t.Errorf("npm: %d %s", rec.Code, rec.Body)
	}
	if len(r.queue) != 0 {
		t.Errorf("queue should be empty, has %d", len(r.queue))
	}
}

func TestWebhookRejectsBadSignatureAndDisabled(t *testing.T) {
	r := newTestReconciler(t)
	h := githubWebhookHandler("s3cret", r)
	if rec := deliver(t, h, "s3cret", "package", []byte(packageEvent), false); rec.Code != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", rec.Code)
	}
	if rec := deliver(t, h, "wrong", "package", []byte(packageEvent), true); rec.Code != http.StatusUnauthorized {
		t.Errorf("wrong secret: %d", rec.Code)
	}
	disabled := githubWebhookHandler("", r)
	if rec := deliver(t, disabled, "", "package", []byte(packageEvent), true); rec.Code != http.StatusNotFound {
		t.Errorf("disabled: %d", rec.Code)
	}
	if rec := deliver(t, h, "s3cret", "ping", []byte(`{"zen":"x"}`), true); rec.Code != http.StatusOK {
		t.Errorf("ping: %d", rec.Code)
	}
	if rec := deliver(t, h, "s3cret", "push", []byte(`{}`), true); rec.Code != http.StatusAccepted {
		t.Errorf("other event: %d", rec.Code)
	}
}

func TestRepositoryFromPayloadFallsBackToNamespace(t *testing.T) {
	d := &packageDetails{Name: "configs/argobot", Namespace: "ConfigHub", PackageType: "CONTAINER"}
	key, ok := repositoryFromPayload(d)
	if !ok || key != "ghcr.io/confighub/configs/argobot" {
		t.Errorf("%q %v", key, ok)
	}
}

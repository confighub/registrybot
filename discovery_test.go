// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"net/http"
	"strings"
	"testing"
)

const discoveryDoc = `
defaults:
  space: facts
  exclude: ["^sha-"]
  streams:
    stable: { semver: ">=0.0.0" }
discovery:
  fromWebhooks: true
  owners: [ConfigHub, confighubai]
  exclude: ["^confighub/ui-preview", "-preview$"]
repositories:
  - repository: ghcr.io/confighub/argobot
    unit: argobot-explicit
`

func TestParseDiscovery(t *testing.T) {
	p, err := parseBotConfig([]byte(discoveryDoc), "")
	if err != nil {
		t.Fatal(err)
	}
	d := p.Discovery
	if !d.Enabled {
		t.Fatal("discovery should be enabled")
	}
	allowed := []string{"ghcr.io/confighub/cubbychat", "ghcr.io/confighubai/bizops", "ghcr.io/confighub/configs/argobot"}
	denied := []string{"ghcr.io/someone/else", "ghcr.io/confighub/ui-preview-pr-1", "ghcr.io/confighubai/hub-preview"}
	for _, s := range allowed {
		ref, _ := parseRepository(s)
		if !d.allows(ref) {
			t.Errorf("%s should be allowed", s)
		}
	}
	for _, s := range denied {
		ref, _ := parseRepository(s)
		if d.allows(ref) {
			t.Errorf("%s should be denied", s)
		}
	}
	ref, _ := parseRepository("ghcr.io/confighubai/cubbychat/backend")
	w := d.watchFor(ref, "")
	if w.Space != "facts" || w.Unit != "confighubai-cubbychat-backend" || !w.Discovered {
		t.Errorf("watchFor: %+v", w)
	}
	if names(w.Streams) != "newest semver stable" || len(w.Exclude) != 1 {
		t.Errorf("watchFor should carry defaults: %s %d", names(w.Streams), len(w.Exclude))
	}
	if w := d.watchFor(ref, "existing-slug"); w.Unit != "existing-slug" {
		t.Errorf("watchFor should reuse an existing unit slug")
	}

	// Disabled by default, and any owner is allowed when owners is empty.
	p2, _ := parseBotConfig([]byte("defaults: {space: s}\nrepositories: []\n"), "")
	if p2.Discovery.Enabled || p2.Discovery.allows(ref) {
		t.Error("discovery should be off by default")
	}
	p3, _ := parseBotConfig([]byte("discovery: {fromWebhooks: true}\nrepositories: []\n"), "cfgspace")
	if !p3.Discovery.allows(ref) || p3.Discovery.space != "cfgspace" {
		t.Errorf("empty owners should allow any owner in the fallback space: %+v", p3.Discovery)
	}
	if _, err := parseBotConfig([]byte("discovery: {fromWebhooks: true}\nrepositories: []\n"), ""); err == nil {
		t.Error("discovery without any space should be rejected")
	}
	if _, err := parseBotConfig([]byte("discovery: {fromWebhooks: true, exclude: ['(']}\nrepositories: []\n"), "s"); err == nil {
		t.Error("bad discovery pattern should be rejected")
	}
}

func newDiscoveryReconciler(t *testing.T) *reconciler {
	t.Helper()
	r := newReconciler(config{PollInterval: defaultPollInterval, ConfigSpace: "facts"}, nil, nil)
	p, err := parseBotConfig([]byte(discoveryDoc), "facts")
	if err != nil {
		t.Fatal(err)
	}
	r.discovery = p.Discovery
	for _, w := range p.Watches {
		r.explicit[w.Key] = w
	}
	r.rebuildLocked()
	return r
}

func TestDiscoverAddsAllowedRepositoryOnce(t *testing.T) {
	r := newDiscoveryReconciler(t)

	w, ok := r.discover("ghcr.io/confighubai/bizops")
	if !ok || !w.Discovered || w.Unit != "confighubai-bizops" {
		t.Fatalf("discover: %+v %v", w, ok)
	}
	if _, ok := r.lookup("ghcr.io/confighubai/bizops"); !ok {
		t.Error("discovered repository should be watched")
	}
	if _, ok := r.pendingDisc["ghcr.io/confighubai/bizops"]; !ok {
		t.Error("discovered repository should be pending until recovered from its unit")
	}
	// Explicit entry wins and is returned unchanged.
	if w, ok := r.discover("ghcr.io/confighub/argobot"); !ok || w.Discovered || w.Unit != "argobot-explicit" {
		t.Errorf("explicit: %+v %v", w, ok)
	}
	// Policy holds.
	if _, ok := r.discover("ghcr.io/stranger/thing"); ok {
		t.Error("unlisted owner should be refused")
	}
	if _, ok := r.discover("ghcr.io/confighub/ui-preview-pr-9"); ok {
		t.Error("excluded repository should be refused")
	}
	if _, ok := r.discover("not a repo"); ok {
		t.Error("garbage should be refused")
	}
}

func TestRebuildDropsDiscoveredWhenPolicyTightens(t *testing.T) {
	r := newDiscoveryReconciler(t)
	r.discover("ghcr.io/confighubai/bizops")

	p, err := parseBotConfig([]byte(strings.Replace(discoveryDoc, "owners: [ConfigHub, confighubai]", "owners: [confighub]", 1)), "facts")
	if err != nil {
		t.Fatal(err)
	}
	r.mu.Lock()
	r.discovery = p.Discovery
	r.rebuildLocked()
	r.mu.Unlock()
	if _, ok := r.lookup("ghcr.io/confighubai/bizops"); ok {
		t.Error("a discovered repository outside the new policy should be dropped")
	}
	if _, ok := r.lookup("ghcr.io/confighub/argobot"); !ok {
		t.Error("explicit repositories are unaffected")
	}
}

func TestWebhookDiscoversUnlistedRepository(t *testing.T) {
	r := newDiscoveryReconciler(t)
	h := githubWebhookHandler("s3cret", r)

	body := []byte(strings.ReplaceAll(packageEvent, "confighub/argobot", "confighubai/bizops"))
	rec := deliver(t, h, "s3cret", "package", body, true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"discovered":true`) {
		t.Fatalf("discovered delivery: %d %s", rec.Code, rec.Body)
	}
	if key := <-r.queue; key != "ghcr.io/confighubai/bizops" {
		t.Errorf("queued %q", key)
	}

	denied := []byte(strings.ReplaceAll(packageEvent, "confighub/argobot", "stranger/thing"))
	rec = deliver(t, h, "s3cret", "package", denied, true)
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"queued":false`) {
		t.Errorf("denied delivery: %d %s", rec.Code, rec.Body)
	}
	if len(r.queue) != 0 {
		t.Error("denied repository must not be queued")
	}
}

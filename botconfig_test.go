// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"strings"
	"testing"
	"time"
)

func TestParseRepository(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/confighub/argobot":                  "ghcr.io/confighub/argobot",
		"ghcr.io/ConfigHub/ArgoBot:v1":               "ghcr.io/confighub/argobot",
		"ghcr.io/confighub/configs/argobot@sha256:x": "ghcr.io/confighub/configs/argobot",
		" ghcr.io/confighub/argobot/ ":               "ghcr.io/confighub/argobot",
	}
	for in, want := range cases {
		ref, err := parseRepository(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if ref.String() != want {
			t.Errorf("%q: got %s, want %s", in, ref, want)
		}
	}
	for _, bad := range []string{"argobot", "confighub/argobot", "docker.io/library/nginx", "ghcr.io//argobot"} {
		if _, err := parseRepository(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	ref, _ := parseRepository("ghcr.io/confighub/configs/argobot")
	if ref.Owner != "confighub" || ref.Name != "configs/argobot" {
		t.Errorf("nested name: %+v", ref)
	}
}

func TestParseBotConfigDefaultsAndOverrides(t *testing.T) {
	doc := `
pollInterval: 90s
defaults:
  space: shared-facts
  limit: 10
  exclude: ["^sha-"]
  streams:
    stable: {semver: ">=1.0.0"}
repositories:
  - repository: ghcr.io/confighub/argobot
  - repository: ghcr.io/confighub/configs/argobot
    space: other
    unit: argobot-bundle
    exclude: []
    streams:
      stable: {pattern: "^latest$"}
      semver: {semver: "<1.0.0"}
`
	p, err := parseBotConfig([]byte(doc), "fallback")
	interval, watches := p.Interval, p.Watches
	if err != nil {
		t.Fatal(err)
	}
	if interval != 90*time.Second {
		t.Errorf("interval: %s", interval)
	}
	if len(watches) != 2 {
		t.Fatalf("watches: %d", len(watches))
	}
	a, b := watches[0], watches[1]
	if a.Space != "shared-facts" || a.Unit != "confighub-argobot" || a.Limit != 10 || len(a.Exclude) != 1 {
		t.Errorf("a: %+v", a)
	}
	if names(a.Streams) != "newest semver stable" {
		t.Errorf("a streams: %s", names(a.Streams))
	}
	if a.Streams[2].Constraint == nil || !a.Streams[2].Semver {
		t.Errorf("a.stable should be a semver stream with a constraint")
	}
	if b.Space != "other" || b.Unit != "argobot-bundle" || len(b.Exclude) != 0 || b.Limit != 10 {
		t.Errorf("b: %+v", b)
	}
	if b.Streams[2].Semver || b.Streams[2].Pattern == nil {
		t.Errorf("b.stable should be a pattern-only stream (override), got %+v", b.Streams[2])
	}
	if b.Streams[1].Name != "semver" || b.Streams[1].Constraint == nil {
		t.Errorf("b.semver built-in should be overridden with a constraint")
	}
}

func names(streams []stream) string {
	var n []string
	for _, s := range streams {
		n = append(n, s.Name)
	}
	return strings.Join(n, " ")
}

func TestParseBotConfigFallbackSpaceAndSlug(t *testing.T) {
	p, err := parseBotConfig([]byte("repositories:\n  - repository: ghcr.io/confighub/configs/argobot\n"), "cfgspace")
	watches := p.Watches
	if err != nil {
		t.Fatal(err)
	}
	if watches[0].Space != "cfgspace" || watches[0].Unit != "confighub-configs-argobot" {
		t.Errorf("%+v", watches[0])
	}
	if watches[0].Limit != defaultLimit {
		t.Errorf("limit: %d", watches[0].Limit)
	}
}

func TestParseBotConfigErrors(t *testing.T) {
	cases := map[string]string{
		"no space":       "repositories:\n  - repository: ghcr.io/a/b\n",
		"dup repo":       "defaults: {space: s}\nrepositories:\n  - repository: ghcr.io/a/b\n  - repository: ghcr.io/A/B\n",
		"unit clash":     "defaults: {space: s}\nrepositories:\n  - {repository: ghcr.io/a/b, unit: x}\n  - {repository: ghcr.io/a/c, unit: x}\n",
		"empty stream":   "defaults: {space: s}\nrepositories:\n  - repository: ghcr.io/a/b\n    streams: {x: {}}\n",
		"bad constraint": "defaults: {space: s}\nrepositories:\n  - repository: ghcr.io/a/b\n    streams: {x: {semver: '>>1'}}\n",
		"bad pattern":    "defaults: {space: s}\nrepositories:\n  - repository: ghcr.io/a/b\n    exclude: ['(']\n",
		"bad interval":   "pollInterval: soon\ndefaults: {space: s}\nrepositories: []\n",
		"other registry": "defaults: {space: s}\nrepositories:\n  - repository: docker.io/library/nginx\n",
		"missing repo":   "defaults: {space: s}\nrepositories:\n  - unit: x\n",
	}
	for name, doc := range cases {
		if _, err := parseBotConfig([]byte(doc), ""); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestSlugify(t *testing.T) {
	if got := slugify("ConfigHub", "configs/argobot"); got != "confighub-configs-argobot" {
		t.Errorf("got %q", got)
	}
}

func TestDisplayName(t *testing.T) {
	if got := displayName(repoRef{Registry: "ghcr.io", Owner: "confighub", Name: "configs/argobot"}); got != "confighub configs argobot" {
		t.Errorf("got %q", got)
	}
}

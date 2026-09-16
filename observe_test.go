// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"regexp"
	"strings"
	"testing"
	"time"
)

func testWatch(t *testing.T, yamlDoc string) watch {
	t.Helper()
	_, watches, err := parseBotConfig([]byte(yamlDoc), "facts")
	if err != nil {
		t.Fatalf("parseBotConfig: %v", err)
	}
	if len(watches) != 1 {
		t.Fatalf("want 1 watch, got %d", len(watches))
	}
	return watches[0]
}

func at(day int) time.Time { return time.Date(2026, 9, day, 12, 0, 0, 0, time.UTC) }

func sampleVersions() []packageVersion {
	return []packageVersion{
		{Digest: "sha256:a", Tags: []string{"v0.3.1", "latest"}, CreatedAt: at(10)},
		{Digest: "sha256:b", Tags: []string{"v0.3.0"}, CreatedAt: at(5)},
		{Digest: "sha256:c", Tags: []string{"main", "sha-abc123"}, CreatedAt: at(12)},
		{Digest: "sha256:d", Tags: []string{"v1.0.0-rc.1"}, CreatedAt: at(11)},
		{Digest: "sha256:e", Tags: nil, CreatedAt: at(12)}, // attestation: untagged
		{Digest: "sha256:f", Tags: []string{"v0.2.9"}, CreatedAt: at(1)},
	}
}

func TestBuildFactDocDefaultStreams(t *testing.T) {
	w := testWatch(t, `
repositories:
  - repository: ghcr.io/confighub/argobot
`)
	doc := buildFactDoc(w, sampleVersions(), "https://example", at(15))

	if got := doc.Streams["newest"]; got == nil || got.Tag != "main" {
		t.Errorf("newest: want main, got %+v", got)
	}
	// semver excludes prereleases by default, so v1.0.0-rc.1 loses to v0.3.1.
	if got := doc.Streams["semver"]; got == nil || got.Tag != "v0.3.1" || got.Digest != "sha256:a" {
		t.Errorf("semver: want v0.3.1@sha256:a, got %+v", got)
	}
	if got := doc.Streams["semver"].ImageByDigest; got != "ghcr.io/confighub/argobot@sha256:a" {
		t.Errorf("imageByDigest: got %q", got)
	}
	// 7 tags across 5 tagged manifests; untagged manifest contributes none.
	if len(doc.Tags) != 7 {
		t.Errorf("tags: want 7, got %d: %+v", len(doc.Tags), doc.Tags)
	}
	// Newest first, tag name as tiebreaker.
	if doc.Tags[0].Tag != "main" || doc.Tags[1].Tag != "sha-abc123" || doc.Tags[2].Tag != "v1.0.0-rc.1" {
		t.Errorf("ordering: %v %v %v", doc.Tags[0].Tag, doc.Tags[1].Tag, doc.Tags[2].Tag)
	}
	if doc.ObservedAt != "2026-09-15T12:00:00Z" {
		t.Errorf("observedAt: %q", doc.ObservedAt)
	}
}

func TestBuildFactDocCustomStreamsAndExclude(t *testing.T) {
	w := testWatch(t, `
defaults:
  exclude: ["^sha-"]
repositories:
  - repository: ghcr.io/confighub/argobot
    limit: 3
    streams:
      stable: {semver: "<0.3.1"}
      rc:     {semver: ">=1.0.0-0", pattern: "-rc"}
      branch: {pattern: "^main$"}
      none:   {pattern: "^nothing-matches$"}
`)
	doc := buildFactDoc(w, sampleVersions(), "", at(15))

	cases := map[string]string{"stable": "v0.3.0", "rc": "v1.0.0-rc.1", "branch": "main", "newest": "main", "semver": "v0.3.1"}
	for name, want := range cases {
		got := doc.Streams[name]
		if got == nil || got.Tag != want {
			t.Errorf("stream %s: want %s, got %+v", name, want, got)
		}
	}
	if s, ok := doc.Streams["none"]; !ok || s != nil {
		t.Errorf("stream none: want present and null, got %v (present=%v)", s, ok)
	}
	if len(doc.Tags) != 3 {
		t.Errorf("limit: want 3 tags, got %d", len(doc.Tags))
	}
	for _, tf := range doc.Tags {
		if strings.HasPrefix(tf.Tag, "sha-") {
			t.Errorf("excluded tag leaked: %s", tf.Tag)
		}
	}
}

func TestRenderIsStableAndIgnoresObservedAt(t *testing.T) {
	w := testWatch(t, "repositories:\n  - repository: ghcr.io/confighub/argobot\n")
	a, err := renderFactDoc(buildFactDoc(w, sampleVersions(), "u", at(15)))
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderFactDoc(buildFactDoc(w, sampleVersions(), "u", at(16)))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) == string(b) {
		t.Fatal("observedAt should differ between renders")
	}
	if !sameFacts(a, b) {
		t.Errorf("sameFacts should ignore observedAt:\n%s\n---\n%s", a, b)
	}
	if !strings.HasPrefix(string(a), "# Facts about ghcr.io/confighub/argobot") {
		t.Errorf("missing header: %s", a[:80])
	}
	if !strings.Contains(string(a), "schema: registrybot.confighub.com/v1alpha1\n") {
		t.Errorf("missing schema line")
	}

	changed := sampleVersions()
	changed[0].Tags = []string{"v0.3.2"}
	c, _ := renderFactDoc(buildFactDoc(w, changed, "u", at(15)))
	if sameFacts(a, c) {
		t.Errorf("a tag change must be a fact change")
	}
}

func TestParseReleaseVersion(t *testing.T) {
	ok := []string{"v1.2.3", "1.2.3", "v1.2.3-rc.1", "0.0.1"}
	bad := []string{"latest", "main", "1.2", "v1", "20260916", "sha-abc", "v1.2.3.4"}
	for _, s := range ok {
		if parseReleaseVersion(s) == nil {
			t.Errorf("%q should parse", s)
		}
	}
	for _, s := range bad {
		if parseReleaseVersion(s) != nil {
			t.Errorf("%q should not parse", s)
		}
	}
}

func TestExcluded(t *testing.T) {
	pats := []*regexp.Regexp{regexp.MustCompile("^sha-"), regexp.MustCompile("^pr-")}
	if !excluded(pats, "sha-1") || !excluded(pats, "pr-7") || excluded(pats, "v1.0.0") {
		t.Error("exclusion mismatch")
	}
}

func TestDescribeChange(t *testing.T) {
	w := testWatch(t, "repositories:\n  - repository: ghcr.io/confighub/argobot\n")
	got := describeChange(buildFactDoc(w, sampleVersions(), "", at(15)))
	want := "registrybot observed ghcr.io/confighub/argobot: newest=main semver=v0.3.1 (7 tags)"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

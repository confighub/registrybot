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
	p, err := parseBotConfig([]byte(yamlDoc), "facts")
	if err != nil {
		t.Fatalf("parseBotConfig: %v", err)
	}
	if len(p.Watches) != 1 {
		t.Fatalf("want 1 watch, got %d", len(p.Watches))
	}
	return p.Watches[0]
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
	doc := buildFactDoc(w, sampleVersions(), "https://example")

	if len(doc.Streams) != 1 {
		t.Errorf("only the built-in stable stream: got %v", doc.Streams)
	}
	// stable excludes prereleases, so v1.0.0-rc.1 loses to v0.3.1.
	if got := doc.Streams["stable"]; got == nil || *got != (streamFact{Tag: "v0.3.1", Digest: "sha256:a"}) {
		t.Errorf("stable: want v0.3.1@sha256:a, got %+v", got)
	}
	// 7 tags across 5 tagged manifests; the untagged manifest contributes none.
	entries := flattenTags(w, sampleVersions())
	if len(entries) != 7 {
		t.Errorf("entries: want 7, got %d: %+v", len(entries), entries)
	}
	// Newest first, tag name as tiebreaker.
	if entries[0].Tag != "main" || entries[1].Tag != "sha-abc123" || entries[2].Tag != "v1.0.0-rc.1" {
		t.Errorf("ordering: %v %v %v", entries[0].Tag, entries[1].Tag, entries[2].Tag)
	}
}

func TestBuildFactDocCustomStreamsAndExclude(t *testing.T) {
	w := testWatch(t, `
defaults:
  exclude: ["^sha-"]
repositories:
  - repository: ghcr.io/confighub/argobot
    streams:
      stable: {semver: "<0.3.1"}
      rc:     {semver: ">=1.0.0-0", pattern: "-rc"}
      branch: {pattern: "^main$"}
      none:   {pattern: "^nothing-matches$"}
`)
	doc := buildFactDoc(w, sampleVersions(), "")

	cases := map[string]string{"stable": "v0.3.0", "rc": "v1.0.0-rc.1", "branch": "main"}
	for name, want := range cases {
		got := doc.Streams[name]
		if got == nil || got.Tag != want {
			t.Errorf("stream %s: want %s, got %+v", name, want, got)
		}
	}
	if s, ok := doc.Streams["none"]; !ok || s != nil {
		t.Errorf("stream none: want present and null, got %v (present=%v)", s, ok)
	}
	for _, e := range flattenTags(w, sampleVersions()) {
		if strings.HasPrefix(e.Tag, "sha-") {
			t.Errorf("excluded tag leaked: %s", e.Tag)
		}
	}
}

func TestRenderIsDeterministic(t *testing.T) {
	w := testWatch(t, "repositories:\n  - repository: ghcr.io/confighub/argobot\n")
	a, err := renderFactDoc(buildFactDoc(w, sampleVersions(), "u"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := renderFactDoc(buildFactDoc(w, sampleVersions(), "u"))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Errorf("an unchanged repository must render the same bytes:\n%s\n---\n%s", a, b)
	}
	if !strings.HasPrefix(string(a), "# Facts about ghcr.io/confighub/argobot") {
		t.Errorf("missing header: %s", a[:80])
	}
	if !strings.Contains(string(a), "configHub:\n    configSchema: registrybot.confighub.com/v1alpha4\n    configName: argobot\n") {
		t.Errorf("missing configHub metadata:\n%s", a)
	}
	if strings.Contains(string(a), "\nschema:") {
		t.Errorf("the schema belongs in configHub.configSchema, not a top-level field:\n%s", a)
	}
	if strings.Contains(string(a), "\nname:") {
		t.Errorf("the name belongs in configHub.configName, not a top-level field:\n%s", a)
	}
	if strings.Contains(string(a), "\ntags:") {
		t.Errorf("the document must not carry a tag list:\n%s", a)
	}
	if !strings.HasSuffix(string(a), "streams:\n    stable:\n        tag: v0.3.1\n        digest: sha256:a\n") {
		t.Errorf("a stream should be its tag and digest and nothing else:\n%s", a)
	}

	changed := sampleVersions()
	changed[0].Tags = []string{"v0.3.2"}
	c, _ := renderFactDoc(buildFactDoc(w, changed, "u"))
	if string(a) == string(c) {
		t.Errorf("a tag change must be a fact change")
	}

	moved := sampleVersions()
	moved[0].Digest = "sha256:a2"
	m, _ := renderFactDoc(buildFactDoc(w, moved, "u"))
	if string(a) == string(m) {
		t.Errorf("a digest change under the same tag must be a fact change")
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
	got := describeChange(buildFactDoc(w, sampleVersions(), ""))
	want := "registrybot observed ghcr.io/confighub/argobot: stable=v0.3.1@a"
	if got != want {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

func TestShortDigest(t *testing.T) {
	cases := map[string]string{
		"sha256:9d0df3df21e047e81dd449c9e31dbd29c6795aafb305ec07b0a4d55bd77b7075": "9d0df3df21e0",
		"sha256:abc": "abc",
		"abcdef":     "abcdef",
		"":           "",
	}
	for in, want := range cases {
		if got := shortDigest(in); got != want {
			t.Errorf("shortDigest(%q) = %q, want %q", in, got, want)
		}
	}
}

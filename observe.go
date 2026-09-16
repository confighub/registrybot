// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Masterminds/semver/v3"
	"gopkg.in/yaml.v3"
)

// factSchema versions the fact document so a consumer can tell what it is
// reading when the shape evolves.
const factSchema = "registrybot.confighub.com/v1alpha1"

// factDoc is what registrybot writes into a fact unit: the observed state of
// one container repository. It is a fact, not an intent — nothing here asks
// ConfigHub to do anything. Downstream units link to paths in it (typically
// streams.<name>.tag or streams.<name>.digest) and ConfigHub's own machinery
// takes it from there.
//
// The document is rendered with sorted keys and a fixed field order so that an
// unchanged repository renders byte-for-byte the same and produces no revision.
type factDoc struct {
	Schema     string                 `yaml:"schema"`
	Repository string                 `yaml:"repository"`
	Registry   string                 `yaml:"registry"`
	Owner      string                 `yaml:"owner"`
	Name       string                 `yaml:"name"`
	Source     string                 `yaml:"source"`
	URL        string                 `yaml:"url,omitempty"`
	ObservedAt string                 `yaml:"observedAt"`
	Streams    map[string]*streamFact `yaml:"streams"`
	Tags       []tagFact              `yaml:"tags"`
}

// streamFact is the tag a stream currently selects. A stream with no matching
// tag is rendered as null so its absence is visible rather than silent.
type streamFact struct {
	Tag           string `yaml:"tag"`
	Digest        string `yaml:"digest"`
	Image         string `yaml:"image"`
	ImageByDigest string `yaml:"imageByDigest"`
	CreatedAt     string `yaml:"createdAt"`
}

// tagFact is one tag of the repository and the manifest it points at.
type tagFact struct {
	Tag       string `yaml:"tag"`
	Digest    string `yaml:"digest"`
	Image     string `yaml:"image"`
	CreatedAt string `yaml:"createdAt"`
}

// taggedVersion is the working shape: one (tag, digest) pair. A manifest with
// several tags contributes one entry per tag; an untagged manifest (signature,
// attestation, dangling layer) contributes nothing.
type taggedVersion struct {
	Tag       string
	Digest    string
	CreatedAt time.Time
	version   *semver.Version // parsed release version, or nil
}

const sourceGitHubPackages = "github-packages"

// buildFactDoc turns the versions GitHub reported into the fact document for
// one watch.
func buildFactDoc(w watch, versions []packageVersion, pkgURL string, now time.Time) factDoc {
	entries := flattenTags(w, versions)

	doc := factDoc{
		Schema:     factSchema,
		Repository: w.Key,
		Registry:   w.Repo.Registry,
		Owner:      w.Repo.Owner,
		Name:       w.Repo.Name,
		Source:     sourceGitHubPackages,
		URL:        pkgURL,
		ObservedAt: now.UTC().Format(time.RFC3339),
		Streams:    map[string]*streamFact{},
		Tags:       []tagFact{},
	}
	for _, s := range w.Streams {
		doc.Streams[s.Name] = selectStream(s, entries, w.Key)
	}
	for i, e := range entries {
		if i >= w.Limit {
			break
		}
		doc.Tags = append(doc.Tags, tagFact{
			Tag:       e.Tag,
			Digest:    e.Digest,
			Image:     w.Key + ":" + e.Tag,
			CreatedAt: e.CreatedAt.UTC().Format(time.RFC3339),
		})
	}
	return doc
}

// flattenTags expands versions into per-tag entries, drops excluded tags, and
// orders them newest first (tag name as a tiebreaker for determinism).
func flattenTags(w watch, versions []packageVersion) []taggedVersion {
	var entries []taggedVersion
	for _, v := range versions {
		for _, tag := range v.Tags {
			if tag == "" || excluded(w.Exclude, tag) {
				continue
			}
			entries = append(entries, taggedVersion{
				Tag:       tag,
				Digest:    v.Digest,
				CreatedAt: v.CreatedAt,
				version:   parseReleaseVersion(tag),
			})
		}
	}
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].CreatedAt.Equal(entries[j].CreatedAt) {
			return entries[i].CreatedAt.After(entries[j].CreatedAt)
		}
		return entries[i].Tag < entries[j].Tag
	})
	return entries
}

func excluded(patterns []*regexp.Regexp, tag string) bool {
	for _, re := range patterns {
		if re.MatchString(tag) {
			return true
		}
	}
	return false
}

// parseReleaseVersion parses tags like v1.2.3, 1.2.3, or 1.2.3-rc.1 strictly
// (MAJOR.MINOR.PATCH required), so a date-ish tag like 20260916 or a branch
// build like main-abc123 is not mistaken for a release.
func parseReleaseVersion(tag string) *semver.Version {
	v, err := semver.StrictNewVersion(strings.TrimPrefix(tag, "v"))
	if err != nil {
		return nil
	}
	return v
}

// anyRelease admits every non-prerelease version; it is the constraint a
// semver stream with no explicit constraint uses.
var anyRelease = mustConstraint(">=0.0.0")

func mustConstraint(s string) *semver.Constraints {
	c, err := semver.NewConstraint(s)
	if err != nil {
		panic(err)
	}
	return c
}

// selectStream picks the entry a stream names, or nil when nothing matches.
func selectStream(s stream, entries []taggedVersion, repo string) *streamFact {
	var pick *taggedVersion
	for i := range entries {
		e := &entries[i]
		if s.Pattern != nil && !s.Pattern.MatchString(e.Tag) {
			continue
		}
		if !s.Semver {
			// entries are newest first: the first pattern match is the answer.
			pick = e
			break
		}
		if e.version == nil {
			continue
		}
		cons := s.Constraint
		if cons == nil {
			cons = anyRelease
		}
		if !cons.Check(e.version) {
			continue
		}
		if pick == nil || e.version.GreaterThan(pick.version) {
			pick = e
		}
	}
	if pick == nil {
		return nil
	}
	return &streamFact{
		Tag:           pick.Tag,
		Digest:        pick.Digest,
		Image:         repo + ":" + pick.Tag,
		ImageByDigest: repo + "@" + pick.Digest,
		CreatedAt:     pick.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// renderFactDoc serializes the document with a header that says where it came
// from and that hand edits will not survive.
func renderFactDoc(doc factDoc) ([]byte, error) {
	body, err := yaml.Marshal(doc)
	if err != nil {
		return nil, err
	}
	header := fmt.Sprintf("# Facts about %s, observed by registrybot from the GitHub Packages API.\n"+
		"# Do not edit: the next observation overwrites this unit. Link to it instead.\n", doc.Repository)
	return append([]byte(header), body...), nil
}

// sameFacts reports whether two rendered documents describe the same state,
// ignoring the observation timestamp. A repository that has not changed must
// not produce a new revision every poll.
func sameFacts(a, b []byte) bool {
	return stripObservedAt(string(a)) == stripObservedAt(string(b))
}

func stripObservedAt(s string) string {
	lines := strings.Split(s, "\n")
	out := lines[:0]
	for _, l := range lines {
		if strings.HasPrefix(l, "observedAt:") {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// describeChange is the revision description on the fact unit: a one-line
// summary of what the streams now point at.
func describeChange(doc factDoc) string {
	names := make([]string, 0, len(doc.Streams))
	for name := range doc.Streams {
		names = append(names, name)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, name := range names {
		if s := doc.Streams[name]; s != nil {
			parts = append(parts, name+"="+s.Tag)
		} else {
			parts = append(parts, name+"=none")
		}
	}
	return fmt.Sprintf("registrybot observed %s: %s (%d tags)", doc.Repository, strings.Join(parts, " "), len(doc.Tags))
}

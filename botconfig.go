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

// botConfig is the configuration document registrybot is driven by. It lives
// in a ConfigHub unit (AppConfig/YAML) so that what the bot watches is itself
// configuration under ConfigHub's control, with history and review; a local
// file stands in for it during development. The bot re-reads it every poll
// cycle, so a change takes effect without a restart.
//
//	pollInterval: 5m
//	defaults:
//	  space: registry-facts
//	  exclude: ["^sha-", "^pr-"]
//	repositories:
//	  - repository: ghcr.io/confighub/argobot
//	    streams:
//	      stable: {semver: ">=0.1.0 <1.0.0"}
//	      main:   {pattern: "^main$"}
type botConfig struct {
	PollInterval string       `yaml:"pollInterval"`
	Defaults     repoDefaults `yaml:"defaults"`
	Repositories []repoConfig `yaml:"repositories"`
}

// repoDefaults are applied to every repository that does not set the field.
type repoDefaults struct {
	Space   string                  `yaml:"space"`
	Streams map[string]streamConfig `yaml:"streams"`
	Exclude []string                `yaml:"exclude"`
	Limit   int                     `yaml:"limit"`
}

// repoConfig is one watched container repository and the fact unit it feeds.
type repoConfig struct {
	// Repository is the image repository, e.g. ghcr.io/confighub/argobot.
	Repository string `yaml:"repository"`
	// Space is the slug of the space holding the fact unit. Defaults to
	// defaults.space, then to the space the configuration unit itself is in.
	Space string `yaml:"space"`
	// Unit is the fact unit's slug. Defaults to <owner>-<name>, slugified.
	Unit string `yaml:"unit"`
	// Streams are named selections over the repository's tags, published under
	// streams.<name> in the fact unit. See streamConfig.
	Streams map[string]streamConfig `yaml:"streams"`
	// Exclude drops tags matching any of these regular expressions before
	// anything else is computed.
	Exclude []string `yaml:"exclude"`
	// Limit caps how many tags the fact unit lists (newest first). Streams are
	// computed over all tags regardless. Default 50.
	Limit int `yaml:"limit"`
}

// streamConfig selects one tag out of many, which is what a downstream unit
// wants to link to: "the current stable release", "the newest main build".
//
// With semver set, the stream is the highest tag that parses as a semantic
// version and satisfies the constraint ("" or "*" means any release version;
// prereleases are included only when the constraint names one, e.g.
// ">=1.0.0-0"). With only pattern set, the stream is the most recently pushed
// tag matching the pattern. Both may be set, in which case the pattern narrows
// the candidates first.
type streamConfig struct {
	Semver  *string `yaml:"semver"`
	Pattern string  `yaml:"pattern"`
}

// Built-in streams, added to every repository that does not define them.
const (
	streamNewest = "newest" // most recently pushed tag of any kind
	streamSemver = "semver" // highest release version
	defaultLimit = 50
)

// watch is a repoConfig after defaults are applied and patterns compiled.
type watch struct {
	Key     string // canonical repository, e.g. ghcr.io/confighub/argobot
	Repo    repoRef
	Space   string
	Unit    string
	Streams []stream
	Exclude []*regexp.Regexp
	Limit   int
}

type stream struct {
	Name       string
	Semver     bool
	Constraint *semver.Constraints // nil with Semver=true means any release version
	Pattern    *regexp.Regexp
}

// repoRef is a parsed image repository reference.
type repoRef struct {
	Registry string // ghcr.io
	Owner    string // confighub
	Name     string // argobot, or configs/argobot
}

func (r repoRef) String() string { return r.Registry + "/" + r.Owner + "/" + r.Name }

// parseRepository accepts "ghcr.io/owner/name[/more][:tag|@digest]" and returns
// the canonical, lowercased repository. Only ghcr.io is supported in this
// version because observation goes through the GitHub Packages API; the shape
// is kept general so another registry backend can slot in.
func parseRepository(s string) (repoRef, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.Index(s, "@"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, ":"); i >= 0 && !strings.Contains(s[i:], "/") {
		s = s[:i]
	}
	s = strings.Trim(s, "/")
	parts := strings.Split(s, "/")
	if len(parts) < 3 {
		return repoRef{}, fmt.Errorf("repository %q must be of the form ghcr.io/<owner>/<name>", s)
	}
	ref := repoRef{Registry: parts[0], Owner: parts[1], Name: strings.Join(parts[2:], "/")}
	if ref.Registry != "ghcr.io" {
		return repoRef{}, fmt.Errorf("repository %q: only ghcr.io is supported (GitHub Packages API)", s)
	}
	if ref.Owner == "" || ref.Name == "" {
		return repoRef{}, fmt.Errorf("repository %q: owner and name must be non-empty", s)
	}
	return ref, nil
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// slugify derives a unit slug from an owner and name: lowercase, hyphenated,
// no leading or trailing hyphen.
func slugify(parts ...string) string {
	s := strings.ToLower(strings.Join(parts, "-"))
	s = nonSlug.ReplaceAllString(s, "-")
	return strings.Trim(s, "-")
}

// parseBotConfig parses and resolves the configuration document. fallbackSpace
// is used when neither the repository nor defaults name a space. It returns the
// poll interval (zero when the document does not set one) and the watches in a
// stable order.
func parseBotConfig(data []byte, fallbackSpace string) (time.Duration, []watch, error) {
	var doc botConfig
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return 0, nil, fmt.Errorf("parsing configuration: %w", err)
	}

	var interval time.Duration
	if doc.PollInterval != "" {
		d, err := time.ParseDuration(doc.PollInterval)
		if err != nil || d <= 0 {
			return 0, nil, fmt.Errorf("pollInterval %q: want a positive duration such as 5m", doc.PollInterval)
		}
		interval = d
	}

	defaultExclude, err := compilePatterns(doc.Defaults.Exclude)
	if err != nil {
		return 0, nil, fmt.Errorf("defaults.exclude: %w", err)
	}
	defaultStreams, err := compileStreams(doc.Defaults.Streams)
	if err != nil {
		return 0, nil, fmt.Errorf("defaults.streams: %w", err)
	}

	seen := map[string]int{}     // repository -> index, to reject duplicates
	units := map[string]string{} // space/unit -> repository, to reject collisions
	var watches []watch
	for i, rc := range doc.Repositories {
		where := fmt.Sprintf("repositories[%d]", i)
		if strings.TrimSpace(rc.Repository) == "" {
			return 0, nil, fmt.Errorf("%s: repository is required", where)
		}
		ref, err := parseRepository(rc.Repository)
		if err != nil {
			return 0, nil, fmt.Errorf("%s: %w", where, err)
		}
		key := ref.String()
		if j, dup := seen[key]; dup {
			return 0, nil, fmt.Errorf("%s: repository %s already listed at repositories[%d]", where, key, j)
		}
		seen[key] = i

		w := watch{Key: key, Repo: ref}
		w.Space = firstNonEmpty(rc.Space, doc.Defaults.Space, fallbackSpace)
		if w.Space == "" {
			return 0, nil, fmt.Errorf("%s: no space: set space, defaults.space, or run with REGISTRYBOT_CONFIG_SPACE", where)
		}
		w.Unit = rc.Unit
		if w.Unit == "" {
			w.Unit = slugify(ref.Owner, ref.Name)
		}
		if prev, clash := units[w.Space+"/"+w.Unit]; clash {
			return 0, nil, fmt.Errorf("%s: unit %s in space %s is already used by %s", where, w.Unit, w.Space, prev)
		}
		units[w.Space+"/"+w.Unit] = key

		w.Limit = rc.Limit
		if w.Limit == 0 {
			w.Limit = doc.Defaults.Limit
		}
		if w.Limit <= 0 {
			w.Limit = defaultLimit
		}

		if rc.Exclude != nil {
			w.Exclude, err = compilePatterns(rc.Exclude)
			if err != nil {
				return 0, nil, fmt.Errorf("%s.exclude: %w", where, err)
			}
		} else {
			w.Exclude = defaultExclude
		}

		own, err := compileStreams(rc.Streams)
		if err != nil {
			return 0, nil, fmt.Errorf("%s.streams: %w", where, err)
		}
		w.Streams = mergeStreams(defaultStreams, own)
		watches = append(watches, w)
	}
	return interval, watches, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	var out []*regexp.Regexp
	for _, p := range patterns {
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("pattern %q: %w", p, err)
		}
		out = append(out, re)
	}
	return out, nil
}

func compileStreams(configs map[string]streamConfig) (map[string]stream, error) {
	out := map[string]stream{}
	for name, sc := range configs {
		if strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("stream name must be non-empty")
		}
		if sc.Semver == nil && sc.Pattern == "" {
			return nil, fmt.Errorf("stream %q: set semver, pattern, or both", name)
		}
		s := stream{Name: name}
		if sc.Pattern != "" {
			re, err := regexp.Compile(sc.Pattern)
			if err != nil {
				return nil, fmt.Errorf("stream %q: pattern %q: %w", name, sc.Pattern, err)
			}
			s.Pattern = re
		}
		if sc.Semver != nil {
			s.Semver = true
			if c := strings.TrimSpace(*sc.Semver); c != "" && c != "*" {
				cons, err := semver.NewConstraint(c)
				if err != nil {
					return nil, fmt.Errorf("stream %q: semver constraint %q: %w", name, c, err)
				}
				s.Constraint = cons
			}
		}
		out[name] = s
	}
	return out, nil
}

// mergeStreams layers a repository's own streams over the defaults and the
// built-ins (newest, semver), the more specific definition winning, and returns
// them sorted by name so the fact unit renders deterministically.
func mergeStreams(defaults, own map[string]stream) []stream {
	merged := map[string]stream{
		streamNewest: {Name: streamNewest},
		streamSemver: {Name: streamSemver, Semver: true},
	}
	for name, s := range defaults {
		merged[name] = s
	}
	for name, s := range own {
		merged[name] = s
	}
	out := make([]stream, 0, len(merged))
	for _, s := range merged {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

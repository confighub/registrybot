// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/confighub/sdk/core/workerapi"
)

// reconciler owns the loop. Two things feed it — the poll timer, which
// enqueues every watched repository, and the webhook receiver, which enqueues
// the one a delivery named — and both converge on reconcile(), which reads the
// repository's versions from GitHub and rewrites its fact unit if anything
// changed. There is no other path to a write, which keeps the two sources from
// disagreeing: a webhook only makes the bot look sooner.
type reconciler struct {
	cfg config
	hub *hubClient
	gh  *githubClient

	mu           sync.RWMutex
	watches      map[string]watch // key: canonical repository
	pollInterval time.Duration
	configHash   string
	configError  string
	status       map[string]*repoStatus

	queue   chan string
	pending map[string]bool
	pendMu  sync.Mutex
}

// repoStatus is what /status reports per repository. It is diagnostics, not
// state: the bot keeps nothing it cannot rebuild from GitHub and ConfigHub.
type repoStatus struct {
	Repository  string            `json:"repository"`
	Space       string            `json:"space"`
	Unit        string            `json:"unit"`
	LastAttempt time.Time         `json:"lastAttempt,omitempty"`
	LastSuccess time.Time         `json:"lastSuccess,omitempty"`
	LastWrite   time.Time         `json:"lastWrite,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
	Streams     map[string]string `json:"streams,omitempty"`
	TagCount    int               `json:"tagCount"`
}

func newReconciler(cfg config, hub *hubClient, gh *githubClient) *reconciler {
	return &reconciler{
		cfg:          cfg,
		hub:          hub,
		gh:           gh,
		watches:      map[string]watch{},
		pollInterval: cfg.PollInterval,
		status:       map[string]*repoStatus{},
		queue:        make(chan string, 1024),
		pending:      map[string]bool{},
	}
}

// Run blocks until ctx is cancelled. Each cycle re-reads the configuration
// document and enqueues every repository; a single worker drains the queue so
// reconciles never run concurrently against the same unit.
func (r *reconciler) Run(ctx context.Context) error {
	go r.worker(ctx)
	for {
		r.cycle(ctx)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(r.interval()):
		}
	}
}

func (r *reconciler) interval() time.Duration {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.pollInterval
}

func (r *reconciler) cycle(ctx context.Context) {
	if err := r.reloadConfig(ctx); err != nil {
		// Keep running on the last good configuration. Polling stale watches is
		// harmless; polling nothing because the document has a typo is not.
		log.Printf("[ERROR] config: %v (continuing with the previous configuration)", err)
		r.mu.Lock()
		r.configError = err.Error()
		r.mu.Unlock()
	}
	r.mu.RLock()
	keys := make([]string, 0, len(r.watches))
	for k := range r.watches {
		keys = append(keys, k)
	}
	r.mu.RUnlock()
	sort.Strings(keys)
	if len(keys) == 0 {
		log.Printf("[WARN] poll: no repositories configured; waiting for the configuration document")
	}
	for _, k := range keys {
		r.enqueue(k)
	}
}

// reloadConfig fetches the configuration document and applies it when its
// content changed. A missing unit is reported and the bot idles until it
// appears, which lets the deployment go first and the configuration follow.
func (r *reconciler) reloadConfig(ctx context.Context) error {
	data, hash, err := r.readConfigDocument(ctx)
	if err != nil {
		return err
	}
	r.mu.RLock()
	unchanged := hash == r.configHash
	r.mu.RUnlock()
	if unchanged {
		return nil
	}
	interval, watches, err := parseBotConfig(data, r.cfg.ConfigSpace)
	if err != nil {
		return err
	}
	if interval == 0 {
		interval = r.cfg.PollInterval
	}
	m := make(map[string]watch, len(watches))
	for _, w := range watches {
		m[w.Key] = w
	}
	r.mu.Lock()
	r.watches = m
	r.pollInterval = interval
	r.configHash = hash
	r.configError = ""
	for k := range r.status {
		if _, still := m[k]; !still {
			delete(r.status, k)
		}
	}
	r.mu.Unlock()
	log.Printf("[INFO] config: loaded %d repositories, poll interval %s", len(watches), interval)
	for _, w := range watches {
		names := make([]string, 0, len(w.Streams))
		for _, s := range w.Streams {
			names = append(names, s.Name)
		}
		log.Printf("[INFO] config: watching %s -> unit %s in space %s (streams: %v)", w.Key, w.Unit, w.Space, names)
	}
	return nil
}

// readConfigDocument returns the document and a hash identifying its content.
// From ConfigHub the unit's DataHash is that hash, so an unchanged document
// costs one list call and no download.
func (r *reconciler) readConfigDocument(ctx context.Context) ([]byte, string, error) {
	if r.cfg.ConfigFile != "" {
		data, err := os.ReadFile(r.cfg.ConfigFile)
		if err != nil {
			return nil, "", fmt.Errorf("reading %s: %w", r.cfg.ConfigFile, err)
		}
		sum := sha256.Sum256(data)
		return data, hex.EncodeToString(sum[:]), nil
	}
	spaceID, err := r.hub.spaceID(ctx, r.cfg.ConfigSpace)
	if err != nil {
		return nil, "", err
	}
	unit, err := r.hub.findUnit(ctx, spaceID, r.cfg.ConfigUnit)
	if err != nil {
		return nil, "", err
	}
	if unit == nil {
		return nil, "", fmt.Errorf("configuration unit %s/%s does not exist yet", r.cfg.ConfigSpace, r.cfg.ConfigUnit)
	}
	r.mu.RLock()
	unchanged := unit.DataHash != "" && unit.DataHash == r.configHash
	r.mu.RUnlock()
	if unchanged {
		return nil, unit.DataHash, nil
	}
	data, err := r.hub.downloadUnitData(ctx, spaceID, unit.UnitID)
	if err != nil {
		return nil, "", err
	}
	hash := unit.DataHash
	if hash == "" {
		sum := sha256.Sum256(data)
		hash = hex.EncodeToString(sum[:])
	}
	return data, hash, nil
}

// enqueue schedules one repository, collapsing duplicates already waiting.
func (r *reconciler) enqueue(key string) {
	r.pendMu.Lock()
	defer r.pendMu.Unlock()
	if r.pending[key] {
		return
	}
	select {
	case r.queue <- key:
		r.pending[key] = true
	default:
		log.Printf("[WARN] queue full; dropping reconcile of %s (the next poll picks it up)", key)
	}
}

// lookup returns the watch for a repository key and whether it is watched.
func (r *reconciler) lookup(key string) (watch, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	w, ok := r.watches[key]
	return w, ok
}

func (r *reconciler) worker(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case key := <-r.queue:
			r.pendMu.Lock()
			delete(r.pending, key)
			r.pendMu.Unlock()
			w, ok := r.lookup(key)
			if !ok {
				continue
			}
			if err := r.reconcile(ctx, w); err != nil && ctx.Err() == nil {
				log.Printf("[ERROR] %s: %v", key, err)
			}
		}
	}
}

// reconcile observes one repository and brings its fact unit up to date.
func (r *reconciler) reconcile(ctx context.Context, w watch) error {
	st := r.statusFor(w)
	r.mu.Lock()
	st.LastAttempt = time.Now()
	r.mu.Unlock()

	err := r.reconcileOnce(ctx, w, st)
	r.mu.Lock()
	if err != nil {
		st.LastError = err.Error()
	} else {
		st.LastError = ""
		st.LastSuccess = time.Now()
	}
	r.mu.Unlock()
	return err
}

func (r *reconciler) reconcileOnce(ctx context.Context, w watch, st *repoStatus) error {
	versions, pkgURL, err := r.gh.listContainerVersions(ctx, w.Repo)
	if err != nil {
		return err
	}
	doc := buildFactDoc(w, versions, pkgURL, time.Now())
	rendered, err := renderFactDoc(doc)
	if err != nil {
		return fmt.Errorf("rendering facts: %w", err)
	}

	spaceID, err := r.hub.spaceID(ctx, w.Space)
	if err != nil {
		return err
	}
	unit, err := r.hub.findUnit(ctx, spaceID, w.Unit)
	if err != nil {
		return err
	}
	var existing []byte
	if unit == nil {
		unit, err = r.hub.createUnit(ctx, spaceID, factUnit(w))
		if err != nil {
			return err
		}
		log.Printf("[INFO] %s: created fact unit %s in space %s", w.Key, w.Unit, w.Space)
	} else if unit.DataSize > 0 {
		existing, err = r.hub.downloadUnitData(ctx, spaceID, unit.UnitID)
		if err != nil {
			return err
		}
	}

	r.mu.Lock()
	st.TagCount = len(doc.Tags)
	st.Streams = map[string]string{}
	for name, s := range doc.Streams {
		if s != nil {
			st.Streams[name] = s.Tag
		}
	}
	r.mu.Unlock()

	if existing != nil && sameFacts(existing, rendered) {
		return nil
	}
	desc := describeChange(doc)
	if err := r.hub.uploadUnitData(ctx, spaceID, unit.UnitID, rendered, desc); err != nil {
		return err
	}
	r.mu.Lock()
	st.LastWrite = time.Now()
	r.mu.Unlock()
	log.Printf("[INFO] %s", desc)
	return nil
}

// factUnit is the shape of a fact unit the bot creates. The labels let a
// filter or query find every fact unit, or the one for a given repository,
// without parsing data.
func factUnit(w watch) goclientnew.Unit {
	return goclientnew.Unit{
		Slug:          w.Unit,
		DisplayName:   displayName(w.Repo),
		ToolchainType: string(workerapi.ToolchainAppConfigYAML),
		Labels: map[string]string{
			"registrybot.confighub.com/managed":    "true",
			"registrybot.confighub.com/registry":   w.Repo.Registry,
			"registrybot.confighub.com/repository": w.Key,
		},
		Annotations: map[string]string{
			"registrybot.confighub.com/schema": factSchema,
		},
		LastChangeDescription: "registrybot created fact unit for " + w.Key,
	}
}

func (r *reconciler) statusFor(w watch) *repoStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.status[w.Key]
	if !ok {
		st = &repoStatus{Repository: w.Key}
		r.status[w.Key] = st
	}
	st.Space, st.Unit = w.Space, w.Unit
	return st
}

// snapshot is the /status payload.
type snapshot struct {
	Version      string       `json:"version"`
	PollInterval string       `json:"pollInterval"`
	ConfigSource string       `json:"configSource"`
	ConfigError  string       `json:"configError,omitempty"`
	Webhooks     bool         `json:"webhooksEnabled"`
	Repositories []repoStatus `json:"repositories"`
}

func (r *reconciler) snapshot() snapshot {
	r.mu.RLock()
	defer r.mu.RUnlock()
	source := r.cfg.ConfigFile
	if source == "" {
		source = "confighub:" + r.cfg.ConfigSpace + "/" + r.cfg.ConfigUnit
	}
	s := snapshot{
		Version:      version,
		PollInterval: r.pollInterval.String(),
		ConfigSource: source,
		ConfigError:  r.configError,
		Webhooks:     r.cfg.WebhookSecret != "",
		Repositories: []repoStatus{},
	}
	keys := make([]string, 0, len(r.status))
	for k := range r.status {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		s.Repositories = append(s.Repositories, *r.status[k])
	}
	return s
}

// displayName is the human-facing name of a fact unit. ConfigHub's DisplayName
// rejects "/" and ":", so the repository is spelled with spaces
// ("confighub argobot"); the exact repository is on the unit's labels.
func displayName(ref repoRef) string {
	return strings.ReplaceAll(ref.Owner+" "+ref.Name, "/", " ")
}

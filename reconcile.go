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

// Labels the bot stamps on every fact unit it creates. They are how a
// discovered repository is recovered after a restart and how a query finds
// every fact unit without parsing data.
const (
	labelManaged    = "registrybot.confighub.com/managed"
	labelRegistry   = "registrybot.confighub.com/registry"
	labelRepository = "registrybot.confighub.com/repository"
)

// reconciler owns the loop. Two things feed it — the poll timer, which
// enqueues every watched repository, and the webhook receiver, which enqueues
// the one a delivery named — and both converge on reconcile(), which reads the
// repository's versions from GitHub and rewrites its fact unit if anything
// changed. There is no other path to a write, which keeps the two sources from
// disagreeing: a webhook only makes the bot look sooner.
//
// Watches come from two places. Explicit ones are the configuration document's
// repositories. Discovered ones exist because a signed webhook named a
// repository the discovery policy admits; they are recovered each cycle from
// the labeled fact units in the discovery space, so the units are the record
// and the bot never edits its own configuration. An explicit entry always wins
// over a discovered one for the same repository.
type reconciler struct {
	cfg config
	hub *hubClient
	gh  *githubClient

	mu           sync.RWMutex
	explicit     map[string]watch // from the configuration document
	discovered   map[string]watch // recovered from fact units + added by webhooks since
	pendingDisc  map[string]watch // added by webhooks since the last recovery listing
	watches      map[string]watch // explicit ∪ discovered, explicit winning
	discovery    discovery
	pollInterval time.Duration
	settle       time.Duration // webhooks.settle
	maxDelay     time.Duration // webhooks.maxDelay
	configHash   string
	configError  string
	status       map[string]*repoStatus

	queue   chan string
	pending map[string]bool
	pendMu  sync.Mutex

	// settling holds the repositories a webhook named whose settle window has
	// not closed yet. The poll leaves them alone; the timer enqueues them.
	settling map[string]*settleState
	settleMu sync.Mutex
}

// settleState is one open settle window.
type settleState struct {
	timer      *time.Timer
	first      time.Time // when the window opened
	deliveries int       // webhook deliveries folded into it
}

// repoStatus is what /status reports per repository. It is diagnostics, not
// state: the bot keeps nothing it cannot rebuild from GitHub and ConfigHub.
type repoStatus struct {
	Repository  string            `json:"repository"`
	Space       string            `json:"space"`
	Unit        string            `json:"unit"`
	Discovered  bool              `json:"discovered"`
	LastAttempt time.Time         `json:"lastAttempt,omitempty"`
	LastSuccess time.Time         `json:"lastSuccess,omitempty"`
	LastWrite   time.Time         `json:"lastWrite,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
	Streams     map[string]string `json:"streams,omitempty"`
}

func newReconciler(cfg config, hub *hubClient, gh *githubClient) *reconciler {
	return &reconciler{
		cfg:          cfg,
		hub:          hub,
		gh:           gh,
		explicit:     map[string]watch{},
		discovered:   map[string]watch{},
		pendingDisc:  map[string]watch{},
		watches:      map[string]watch{},
		pollInterval: cfg.PollInterval,
		status:       map[string]*repoStatus{},
		queue:        make(chan string, 1024),
		pending:      map[string]bool{},
		settling:     map[string]*settleState{},
	}
}

// Run blocks until ctx is cancelled. Each cycle re-reads the configuration
// document, recovers discovered repositories, and enqueues every repository; a
// single worker drains the queue so reconciles never run concurrently against
// the same unit.
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
	if err := r.recoverDiscovered(ctx); err != nil {
		log.Printf("[ERROR] discovery: %v (keeping the previously known repositories)", err)
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
		if r.isSettling(k) {
			// A webhook named it moments ago and more deliveries may follow;
			// the settle timer reconciles it once they stop.
			continue
		}
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
	parsed, err := parseBotConfig(data, r.cfg.ConfigSpace)
	if err != nil {
		return err
	}
	interval := parsed.Interval
	if interval == 0 {
		interval = r.cfg.PollInterval
	}
	m := make(map[string]watch, len(parsed.Watches))
	for _, w := range parsed.Watches {
		m[w.Key] = w
	}
	r.mu.Lock()
	r.explicit = m
	r.discovery = parsed.Discovery
	r.pollInterval = interval
	r.settle = parsed.Settle
	r.maxDelay = parsed.MaxDelay
	r.configHash = hash
	r.configError = ""
	r.rebuildLocked()
	r.mu.Unlock()
	log.Printf("[INFO] config: loaded %d repositories, poll interval %s, webhook settle %s (max delay %s), discovery from webhooks: %v",
		len(parsed.Watches), interval, parsed.Settle, parsed.MaxDelay, parsed.Discovery.Enabled)
	for _, w := range parsed.Watches {
		names := make([]string, 0, len(w.Streams))
		for _, s := range w.Streams {
			names = append(names, s.Name)
		}
		log.Printf("[INFO] config: watching %s -> unit %s in space %s (streams: %v)", w.Key, w.Unit, w.Space, names)
	}
	return nil
}

// recoverDiscovered rebuilds the discovered set from the fact units in the
// discovery space, so a restart forgets nothing and a deleted fact unit ends
// the watch. Repositories a webhook added since the last listing are kept
// until their first reconcile has created the unit.
func (r *reconciler) recoverDiscovered(ctx context.Context) error {
	r.mu.RLock()
	d := r.discovery
	r.mu.RUnlock()
	if !d.Enabled {
		r.mu.Lock()
		if len(r.discovered) > 0 || len(r.pendingDisc) > 0 {
			r.discovered = map[string]watch{}
			r.pendingDisc = map[string]watch{}
			r.rebuildLocked()
		}
		r.mu.Unlock()
		return nil
	}
	spaceID, err := r.hub.spaceID(ctx, d.space)
	if err != nil {
		return err
	}
	units, err := r.hub.listUnits(ctx, spaceID)
	if err != nil {
		return err
	}
	found := map[string]watch{}
	for _, u := range units {
		if u.Labels[labelManaged] != "true" || u.Labels[labelRepository] == "" {
			continue
		}
		ref, err := parseRepository(u.Labels[labelRepository])
		if err != nil || !d.allows(ref) {
			continue
		}
		found[ref.String()] = d.watchFor(ref, u.Slug)
	}
	r.mu.Lock()
	for key := range r.explicit {
		// An explicit repository's fact unit carries the labels too; it is
		// not discovered, and counting it as such misreports the number.
		delete(found, key)
	}
	for key, w := range r.pendingDisc {
		if _, ok := found[key]; !ok {
			found[key] = w
		}
	}
	for key, w := range r.discovered {
		// Still settling from the webhook that discovered it, so its fact
		// unit does not exist yet; forgetting it now would lose the reconcile.
		if _, ok := found[key]; !ok && r.isSettling(key) {
			found[key] = w
		}
	}
	r.pendingDisc = map[string]watch{}
	before := len(r.discovered)
	r.discovered = found
	r.rebuildLocked()
	r.mu.Unlock()
	if len(found) != before {
		log.Printf("[INFO] discovery: %d discovered repositories in space %s", len(found), d.space)
	}
	return nil
}

// rebuildLocked recomputes the merged watch map. Caller holds r.mu.
func (r *reconciler) rebuildLocked() {
	m := make(map[string]watch, len(r.explicit)+len(r.discovered))
	for k, w := range r.discovered {
		if r.discovery.allows(w.Repo) {
			m[k] = w
		}
	}
	for k, w := range r.explicit {
		m[k] = w
	}
	r.watches = m
	for k := range r.status {
		if _, still := m[k]; !still {
			delete(r.status, k)
		}
	}
}

// discover admits a repository named by a webhook, when the policy allows it,
// and returns its watch. An explicit repository is returned as is.
func (r *reconciler) discover(key string) (watch, bool) {
	ref, err := parseRepository(key)
	if err != nil {
		return watch{}, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if w, ok := r.watches[ref.String()]; ok {
		return w, true
	}
	if !r.discovery.allows(ref) {
		return watch{}, false
	}
	w := r.discovery.watchFor(ref, "")
	r.discovered[w.Key] = w
	r.pendingDisc[w.Key] = w
	r.watches[w.Key] = w
	log.Printf("[INFO] discovery: now watching %s -> unit %s in space %s", w.Key, w.Unit, w.Space)
	return w, true
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

// enqueueSettled schedules a repository a webhook named. Rather than
// reconciling now, it opens (or extends) the repository's settle window: the
// reconcile runs once settle has passed with no further delivery, or when the
// window has been open for maxDelay, whichever comes first. With settle
// disabled it enqueues immediately.
func (r *reconciler) enqueueSettled(key string) {
	r.mu.RLock()
	settle, maxDelay := r.settle, r.maxDelay
	r.mu.RUnlock()
	if settle <= 0 {
		r.enqueue(key)
		return
	}
	now := time.Now()
	r.settleMu.Lock()
	defer r.settleMu.Unlock()
	st, open := r.settling[key]
	if !open {
		st = &settleState{first: now}
		r.settling[key] = st
	} else {
		st.timer.Stop()
	}
	st.deliveries++
	delay := settle
	if maxDelay > 0 {
		if remaining := maxDelay - now.Sub(st.first); remaining < delay {
			delay = max(remaining, 0)
		}
	}
	st.timer = time.AfterFunc(delay, func() { r.settled(key) })
}

// settled closes a repository's settle window and enqueues it.
func (r *reconciler) settled(key string) {
	r.settleMu.Lock()
	st, open := r.settling[key]
	delete(r.settling, key)
	r.settleMu.Unlock()
	if !open {
		return
	}
	log.Printf("[INFO] webhook: %s settled after %d deliveries in %s; reconciling",
		key, st.deliveries, time.Since(st.first).Round(time.Second))
	r.enqueue(key)
}

// isSettling reports whether a settle window is open for the repository.
func (r *reconciler) isSettling(key string) bool {
	r.settleMu.Lock()
	defer r.settleMu.Unlock()
	_, open := r.settling[key]
	return open
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
// without parsing data, and are what discovery recovers from.
func factUnit(w watch) goclientnew.Unit {
	return goclientnew.Unit{
		Slug:          w.Unit,
		DisplayName:   displayName(w.Repo),
		ToolchainType: string(workerapi.ToolchainAppConfigYAML),
		Labels: map[string]string{
			labelManaged:    "true",
			labelRegistry:   w.Repo.Registry,
			labelRepository: w.Key,
		},
		Annotations: map[string]string{
			"registrybot.confighub.com/schema": factSchema,
		},
		LastChangeDescription: "registrybot created fact unit for " + w.Key,
	}
}

// displayName is the human-facing name of a fact unit. ConfigHub's DisplayName
// rejects "/" and ":", so the repository is spelled with spaces
// ("confighub argobot"); the exact repository is on the unit's labels.
func displayName(ref repoRef) string {
	return strings.ReplaceAll(ref.Owner+" "+ref.Name, "/", " ")
}

func (r *reconciler) statusFor(w watch) *repoStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	st, ok := r.status[w.Key]
	if !ok {
		st = &repoStatus{Repository: w.Key}
		r.status[w.Key] = st
	}
	st.Space, st.Unit, st.Discovered = w.Space, w.Unit, w.Discovered
	return st
}

// snapshot is the /status payload.
type snapshot struct {
	Version      string       `json:"version"`
	PollInterval string       `json:"pollInterval"`
	Settle       string       `json:"webhookSettle"`
	ConfigSource string       `json:"configSource"`
	ConfigError  string       `json:"configError,omitempty"`
	Webhooks     bool         `json:"webhooksEnabled"`
	Discovery    bool         `json:"discoveryEnabled"`
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
		Settle:       r.settle.String(),
		ConfigSource: source,
		ConfigError:  r.configError,
		Webhooks:     r.cfg.WebhookSecret != "",
		Discovery:    r.discovery.Enabled,
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

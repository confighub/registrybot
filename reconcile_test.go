// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
)

// fakeHub is the slice of the ConfigHub API reconcileOnce uses: one space,
// whose units are listed, created, read and written.
type fakeHub struct {
	mu      sync.Mutex
	space   goclientnew.Space
	units   map[uuid.UUID]*goclientnew.Unit
	data    map[uuid.UUID][]byte
	uploads int
}

func newFakeHub(t *testing.T, slug string) (*fakeHub, *httptest.Server) {
	t.Helper()
	f := &fakeHub{
		space: goclientnew.Space{SpaceID: uuid.New(), Slug: slug},
		units: map[uuid.UUID]*goclientnew.Unit{},
		data:  map[uuid.UUID][]byte{},
	}
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeHub) serve(w http.ResponseWriter, req *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/api"), "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "space" && req.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, []goclientnew.ExtendedSpace{{Space: &f.space}})
	case len(parts) == 3 && parts[2] == "unit" && req.Method == http.MethodGet:
		var out []goclientnew.ExtendedUnit
		for _, u := range f.units {
			c := *u
			out = append(out, goclientnew.ExtendedUnit{Unit: &c})
		}
		writeJSON(w, http.StatusOK, out)
	case len(parts) == 3 && parts[2] == "unit" && req.Method == http.MethodPost:
		var u goclientnew.Unit
		if err := json.NewDecoder(req.Body).Decode(&u); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.UnitID, u.SpaceID = uuid.New(), f.space.SpaceID
		f.units[u.UnitID] = &u
		writeJSON(w, http.StatusOK, goclientnew.UnitCreateOrUpdateResponse{Unit: &u})
	case len(parts) == 5 && parts[4] == "data":
		id, err := uuid.Parse(parts[3])
		u, ok := f.units[id]
		if err != nil || !ok {
			http.NotFound(w, req)
			return
		}
		if req.Method == http.MethodGet {
			_, _ = w.Write(f.data[id])
			return
		}
		body, _ := io.ReadAll(req.Body)
		f.data[id] = body
		u.DataSize = int64(len(body))
		f.uploads++
		writeJSON(w, http.StatusOK, goclientnew.UnitCreateOrUpdateResponse{Unit: u})
	default:
		http.Error(w, "unexpected "+req.Method+" "+req.URL.Path, http.StatusNotImplemented)
	}
}

// fakeGitHub serves one organization's container package. Its tag can be
// changed, and it can be made to fail.
type fakeGitHub struct {
	mu   sync.Mutex
	tag  string
	fail bool
}

func newFakeGitHub(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	f := &fakeGitHub{tag: "v0.1.0"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case f.fail:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		case req.URL.Path == "/users/confighub":
			writeJSON(w, http.StatusOK, map[string]string{"type": "Organization"})
		case req.URL.Path == "/orgs/confighub/packages/container/argobot/versions":
			writeJSON(w, http.StatusOK, []map[string]any{{
				"id":         1,
				"name":       "sha256:" + strings.Repeat("a", 64),
				"created_at": "2026-11-01T00:00:00Z",
				"updated_at": "2026-11-01T00:00:00Z",
				"metadata":   map[string]any{"container": map[string]any{"tags": []string{f.tag}}},
			}})
		default:
			http.NotFound(w, req)
		}
	}))
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fakeGitHub) set(tag string, fail bool) {
	f.mu.Lock()
	f.tag, f.fail = tag, fail
	f.mu.Unlock()
}

// TestReconcileCountsWhatItDid runs reconcile against fake GitHub and
// ConfigHub servers, so it fails if reconcile stops counting, or counts a
// write as unchanged or the reverse.
func TestReconcileCountsWhatItDid(t *testing.T) {
	reader := useTestMetrics(t)
	hub, hubSrv := newFakeHub(t, "facts")
	gh, ghSrv := newFakeGitHub(t)
	cfg := config{ConfigHubURL: hubSrv.URL, StaticToken: "token", ConfigHubUserAgent: "test",
		GitHubAPIURL: ghSrv.URL, GitHubToken: "token", PollInterval: defaultPollInterval, ConfigSpace: "facts"}
	hc, err := newHubClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	r := newReconciler(cfg, hc, newGitHubClient(cfg))
	w, ok := lookupOnly(t, "ghcr.io/confighub/argobot")
	if !ok {
		t.Fatal("no watch")
	}
	ctx := context.Background()

	step := func(name string, wantErr bool, want map[string]int64, wantUploads int) {
		t.Helper()
		if err := r.reconcile(ctx, w); (err != nil) != wantErr {
			t.Fatalf("%s: reconcile error = %v, want error %v", name, err, wantErr)
		}
		assertCounts(t, collectCounts(t, reader), want)
		hub.mu.Lock()
		uploads := hub.uploads
		hub.mu.Unlock()
		if uploads != wantUploads {
			t.Errorf("%s: %d uploads, want %d", name, uploads, wantUploads)
		}
	}

	step("new unit", false, map[string]int64{"registrybot.reconciles/written": 1}, 1)
	step("same versions", false, map[string]int64{
		"registrybot.reconciles/written": 1, "registrybot.reconciles/unchanged": 1}, 1)
	gh.set("v0.2.0", false)
	step("new tag", false, map[string]int64{
		"registrybot.reconciles/written": 2, "registrybot.reconciles/unchanged": 1}, 2)
	gh.set("v0.2.0", true)
	step("GitHub fails", true, map[string]int64{
		"registrybot.reconciles/written": 2, "registrybot.reconciles/unchanged": 1,
		"registrybot.reconciles/failure": 1}, 2)

	// A failure while shutting down is not counted.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := r.reconcile(cancelled, w); err == nil {
		t.Fatal("reconcile with a cancelled context succeeded")
	}
	assertCounts(t, collectCounts(t, reader), map[string]int64{
		"registrybot.reconciles/written": 2, "registrybot.reconciles/unchanged": 1,
		"registrybot.reconciles/failure": 1})
}

// lookupOnly returns the watch the one-repository test configuration gives.
func lookupOnly(t *testing.T, key string) (watch, bool) {
	t.Helper()
	return newTestReconciler(t).lookup(key)
}

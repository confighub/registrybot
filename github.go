// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// githubClient reads container package versions through the GitHub Packages
// REST API. This is the observation backend: whether a poll timer or a webhook
// asked, the versions listed here are what the fact unit is rebuilt from. A
// webhook payload is never trusted as the source of truth on its own, so the
// bot cannot be talked into recording a tag that does not exist.
type githubClient struct {
	base  string
	token string
	agent string
	http  *http.Client

	// ownerKinds caches whether an owner is an organization or a user, which
	// selects the API path. It changes essentially never.
	ownerKinds sync.Map
}

// packageVersion is one container manifest as GitHub records it: a digest that
// zero or more tags point at.
type packageVersion struct {
	ID        int64
	Digest    string
	Tags      []string
	CreatedAt time.Time
	UpdatedAt time.Time
	HTMLURL   string
}

const (
	githubPerPage  = 100
	githubMaxPages = 5 // 500 most recent versions; older ones are not facts anyone deploys from
)

func newGitHubClient(cfg config) *githubClient {
	return &githubClient{
		base:  cfg.GitHubAPIURL,
		token: cfg.GitHubToken,
		agent: "registrybot/" + version,
		http:  &http.Client{Timeout: 30 * time.Second},
	}
}

func (g *githubClient) get(ctx context.Context, path string, query url.Values, out any) (*http.Response, error) {
	u := g.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", g.agent)

	res, err := g.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 16<<20))
	if err != nil {
		return res, fmt.Errorf("reading response: %w", err)
	}
	if res.StatusCode != http.StatusOK {
		msg := githubErrorMessage(body)
		if res.StatusCode == http.StatusForbidden && res.Header.Get("X-RateLimit-Remaining") == "0" {
			reset := res.Header.Get("X-RateLimit-Reset")
			if n, err := strconv.ParseInt(reset, 10, 64); err == nil {
				return res, fmt.Errorf("GitHub rate limit exhausted; resets at %s", time.Unix(n, 0).UTC().Format(time.RFC3339))
			}
		}
		return res, fmt.Errorf("GET %s: HTTP %d: %s", path, res.StatusCode, msg)
	}
	if remaining, err := strconv.Atoi(res.Header.Get("X-RateLimit-Remaining")); err == nil && remaining < 100 {
		log.Printf("[WARN] github: only %d API requests left in this rate-limit window", remaining)
	}
	if out != nil {
		if err := json.Unmarshal(body, out); err != nil {
			return res, fmt.Errorf("GET %s: decoding response: %w", path, err)
		}
	}
	return res, nil
}

func githubErrorMessage(body []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &e) == nil && e.Message != "" {
		return e.Message
	}
	if len(body) > 200 {
		body = body[:200]
	}
	return string(body)
}

// ownerKind returns "orgs" or "users" for the owner, the two path roots the
// Packages API distinguishes.
func (g *githubClient) ownerKind(ctx context.Context, owner string) (string, error) {
	if v, ok := g.ownerKinds.Load(owner); ok {
		return v.(string), nil
	}
	var u struct {
		Type string `json:"type"`
	}
	if _, err := g.get(ctx, "/users/"+url.PathEscape(owner), nil, &u); err != nil {
		return "", fmt.Errorf("resolving owner %s: %w", owner, err)
	}
	kind := "users"
	if u.Type == "Organization" {
		kind = "orgs"
	}
	g.ownerKinds.Store(owner, kind)
	return kind, nil
}

// packageURL is the web page for the package, derived rather than fetched so
// listing a repository costs no extra request.
func packageURL(kind, owner, name string) string {
	return fmt.Sprintf("https://github.com/%s/%s/packages/container/package/%s", kind, owner, url.PathEscape(name))
}

// listContainerVersions returns the active versions of a container package,
// newest pages first, up to githubMaxPages pages.
func (g *githubClient) listContainerVersions(ctx context.Context, ref repoRef) ([]packageVersion, string, error) {
	kind, err := g.ownerKind(ctx, ref.Owner)
	if err != nil {
		return nil, "", err
	}
	// PathEscape turns the "/" of a nested package name (configs/argobot) into
	// %2F, which is how the API expects it.
	path := fmt.Sprintf("/%s/%s/packages/container/%s/versions", kind, url.PathEscape(ref.Owner), url.PathEscape(ref.Name))

	type apiVersion struct {
		ID        int64     `json:"id"`
		Name      string    `json:"name"`
		CreatedAt time.Time `json:"created_at"`
		UpdatedAt time.Time `json:"updated_at"`
		HTMLURL   string    `json:"html_url"`
		Metadata  struct {
			PackageType string `json:"package_type"`
			Container   struct {
				Tags []string `json:"tags"`
			} `json:"container"`
		} `json:"metadata"`
	}

	var out []packageVersion
	for page := 1; page <= githubMaxPages; page++ {
		q := url.Values{}
		q.Set("per_page", strconv.Itoa(githubPerPage))
		q.Set("page", strconv.Itoa(page))
		q.Set("state", "active")
		var batch []apiVersion
		if _, err := g.get(ctx, path, q, &batch); err != nil {
			if page == 1 {
				return nil, "", fmt.Errorf("listing versions of %s: %w (is the token allowed read:packages on this package?)", ref, err)
			}
			return nil, "", fmt.Errorf("listing versions of %s (page %d): %w", ref, page, err)
		}
		for _, v := range batch {
			out = append(out, packageVersion{
				ID:        v.ID,
				Digest:    v.Name,
				Tags:      v.Metadata.Container.Tags,
				CreatedAt: v.CreatedAt,
				UpdatedAt: v.UpdatedAt,
				HTMLURL:   v.HTMLURL,
			})
		}
		if len(batch) < githubPerPage {
			break
		}
	}
	return out, packageURL(kind, ref.Owner, ref.Name), nil
}

// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/confighub/sdk/core/cubapi"
	goclientnew "github.com/confighub/sdk/core/openapi/goclient-new"
	"github.com/google/uuid"
)

// hubClient is registrybot's view of ConfigHub: an authenticated API client
// that renews its own session, plus the handful of unit and space operations
// the bot needs. It is the only file that talks to the ConfigHub API.
//
// The bot authenticates as a worker identity (by secret or by private key), the
// same way argobot does. A worker credential mints an ordinary session token
// carrying the bot user's organization role, so the same client can list
// spaces, create units, and write unit data. The static-token mode exists for
// local development against a session you already hold; it is never refreshed.
type hubClient struct {
	url   string
	agent string

	// authenticate mints a fresh session. nil when a static token is in use.
	authenticate func() (*cubapi.AuthSession, error)

	mu      sync.RWMutex
	api     *goclientnew.ClientWithResponses
	expires time.Time // proactive re-authentication deadline; zero means never
	spaces  map[string]uuid.UUID
}

func newHubClient(cfg config) (*hubClient, error) {
	h := &hubClient{url: cfg.ConfigHubURL, agent: cfg.ConfigHubUserAgent, spaces: map[string]uuid.UUID{}}

	switch {
	case cfg.StaticToken != "":
		api, err := buildAPI(h.url, h.agent, cfg.StaticToken)
		if err != nil {
			return nil, err
		}
		h.api = api
	case cfg.PrivateKey != "":
		jwk, source, err := loadPrivateJWK(cfg.PrivateKey)
		if err != nil {
			return nil, err
		}
		signer, err := cubapi.NewAssertionSigner(jwk, "", cfg.AssertionAudience)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", source, err)
		}
		h.authenticate = func() (*cubapi.AuthSession, error) {
			return cubapi.PerformAssertionAuth(h.url, signer)
		}
	default:
		workerID, secret := cfg.WorkerID, cfg.WorkerSecret
		h.authenticate = func() (*cubapi.AuthSession, error) {
			return cubapi.PerformWorkerAuth(h.url, workerID, secret)
		}
	}

	if h.authenticate != nil {
		if err := h.refresh(); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// loadPrivateJWK resolves CONFIGHUB_AUTH_PRIVATE_KEY, which holds either the
// JWK itself or a path to a file containing it, mirroring what `cub auth login
// --private-key` accepts. A mounted Secret gives a path; a plain env var gives
// the key inline.
func loadPrivateJWK(ref string) ([]byte, string, error) {
	trimmed := strings.TrimSpace(ref)
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), "$CONFIGHUB_AUTH_PRIVATE_KEY", nil
	}
	data, err := os.ReadFile(trimmed)
	if err != nil {
		return nil, "", fmt.Errorf("reading private key %s: %w", trimmed, err)
	}
	return data, trimmed, nil
}

func buildAPI(url, agent, token string) (*goclientnew.ClientWithResponses, error) {
	c, err := cubapi.NewClient(cubapi.ClientOptions{ServerURL: url, Token: token, UserAgent: agent})
	if err != nil {
		return nil, err
	}
	return c.API, nil
}

// refresh mints a new session and swaps in a client that carries it. The next
// proactive refresh is scheduled from the token's own exp claim so the bot
// never runs on a session about to lapse, whatever lifetime the server chose.
func (h *hubClient) refresh() error {
	session, err := h.authenticate()
	if err != nil {
		return fmt.Errorf("authenticating to ConfigHub at %s: %w", h.url, err)
	}
	api, err := buildAPI(h.url, h.agent, session.AccessToken)
	if err != nil {
		return err
	}
	h.mu.Lock()
	h.api = api
	h.expires = refreshDeadline(session.AccessToken, time.Now())
	h.mu.Unlock()
	log.Printf("[INFO] confighub: authenticated as %s (organization %s); next refresh at %s",
		session.User.ID, session.OrganizationID, h.expires.UTC().Format(time.RFC3339))
	return nil
}

// refreshDeadline picks when to renew a session: 80%% of the way to the JWT's
// exp, with a floor so a pathologically short token does not turn into a busy
// loop, and a 12h fallback when the token carries no readable exp.
func refreshDeadline(token string, now time.Time) time.Time {
	const fallback = 12 * time.Hour
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return now.Add(fallback)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return now.Add(fallback)
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return now.Add(fallback)
	}
	remaining := time.Unix(claims.Exp, 0).Sub(now)
	lead := time.Duration(float64(remaining) * 0.8)
	if lead < 5*time.Minute {
		lead = 5 * time.Minute
	}
	return now.Add(lead)
}

// client returns the current API client, renewing the session first when its
// deadline has passed.
func (h *hubClient) client() (*goclientnew.ClientWithResponses, error) {
	h.mu.RLock()
	api, expires := h.api, h.expires
	h.mu.RUnlock()
	if h.authenticate != nil && !expires.IsZero() && time.Now().After(expires) {
		if err := h.refresh(); err != nil {
			return nil, err
		}
		h.mu.RLock()
		api = h.api
		h.mu.RUnlock()
	}
	return api, nil
}

// do runs one API call, re-authenticating and retrying once on a 401. The
// closure captures its own typed response; do only sees the status.
func (h *hubClient) do(fn func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error)) error {
	api, err := h.client()
	if err != nil {
		return err
	}
	res, err := fn(api)
	if err != nil {
		return err
	}
	if res != nil && res.StatusCode() == http.StatusUnauthorized && h.authenticate != nil {
		if err := h.refresh(); err != nil {
			return err
		}
		api, err = h.client()
		if err != nil {
			return err
		}
		if _, err = fn(api); err != nil {
			return err
		}
	}
	return nil
}

// verify proves the credentials are accepted right now and reports the identity
// they resolve to, so a misconfigured deployment fails at startup with a clear
// message instead of on its first write.
func (h *hubClient) verify(ctx context.Context) error {
	var who string
	err := h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.GetMeWithResponse(ctx)
		if cubapi.IsAPIError(err, res) {
			return res, fmt.Errorf("verifying ConfigHub credentials: %w", cubapi.InterpretErrorGeneric(err, res))
		}
		who = fmt.Sprintf("%s (%s)", res.JSON200.Username, res.JSON200.UserID)
		return res, nil
	})
	if err != nil {
		return err
	}
	log.Printf("[INFO] confighub: acting as %s", who)
	return nil
}

// spaceID resolves a space slug, caching the answer: spaces are not renamed
// under a running bot in practice, and a stale cache surfaces as a 404 on the
// next write, which is logged and retried on the following cycle.
func (h *hubClient) spaceID(ctx context.Context, slug string) (uuid.UUID, error) {
	h.mu.RLock()
	id, ok := h.spaces[slug]
	h.mu.RUnlock()
	if ok {
		return id, nil
	}
	where := fmt.Sprintf("Slug = '%s'", slug)
	var found *uuid.UUID
	err := h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.ListSpacesWithResponse(ctx, &goclientnew.ListSpacesParams{Where: &where})
		if cubapi.IsAPIError(err, res) {
			return res, fmt.Errorf("listing spaces: %w", cubapi.InterpretErrorGeneric(err, res))
		}
		for _, es := range *res.JSON200 {
			if es.Space != nil && es.Space.Slug == slug {
				id := es.Space.SpaceID
				found = &id
				break
			}
		}
		return res, nil
	})
	if err != nil {
		return uuid.Nil, err
	}
	if found == nil {
		return uuid.Nil, fmt.Errorf("space %q not found (or not visible to this identity)", slug)
	}
	h.mu.Lock()
	h.spaces[slug] = *found
	h.mu.Unlock()
	return *found, nil
}

// findUnit returns the unit with the given slug in the space, or nil when there
// is none. The listing carries DataHash, which is enough to decide whether the
// body needs fetching.
func (h *hubClient) findUnit(ctx context.Context, spaceID uuid.UUID, slug string) (*goclientnew.Unit, error) {
	where := fmt.Sprintf("Slug = '%s'", slug)
	var unit *goclientnew.Unit
	err := h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.ListUnitsWithResponse(ctx, spaceID, &goclientnew.ListUnitsParams{Where: &where})
		if cubapi.IsAPIError(err, res) {
			return res, fmt.Errorf("listing units: %w", cubapi.InterpretErrorGeneric(err, res))
		}
		for _, eu := range *res.JSON200 {
			if eu.Unit != nil && eu.Unit.Slug == slug {
				u := *eu.Unit
				unit = &u
				break
			}
		}
		return res, nil
	})
	return unit, err
}

func (h *hubClient) downloadUnitData(ctx context.Context, spaceID, unitID uuid.UUID) ([]byte, error) {
	var body []byte
	err := h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.DownloadUnitDataWithResponse(ctx, spaceID, unitID)
		if err != nil {
			return res, fmt.Errorf("downloading unit data: %w", err)
		}
		if res.StatusCode() != http.StatusOK {
			return res, fmt.Errorf("downloading unit data: HTTP %d: %s", res.StatusCode(), strings.TrimSpace(string(res.Body)))
		}
		body = res.Body
		return res, nil
	})
	return body, err
}

// createUnit creates an empty unit; configuration is written separately, the
// same two-step shape `cub unit create` uses.
func (h *hubClient) createUnit(ctx context.Context, spaceID uuid.UUID, unit goclientnew.Unit) (*goclientnew.Unit, error) {
	var created *goclientnew.Unit
	err := h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.CreateUnitWithResponse(ctx, spaceID, &goclientnew.CreateUnitParams{}, unit)
		if cubapi.IsAPIError(err, res) {
			return res, fmt.Errorf("creating unit %s: %w", unit.Slug, cubapi.InterpretErrorGeneric(err, res))
		}
		if res.JSON200.Unit == nil {
			return res, fmt.Errorf("creating unit %s: response carried no unit", unit.Slug)
		}
		created = res.JSON200.Unit
		return res, nil
	})
	return created, err
}

func (h *hubClient) uploadUnitData(ctx context.Context, spaceID, unitID uuid.UUID, data []byte, description string) error {
	params := &goclientnew.UploadUnitDataParams{}
	if description != "" {
		params.LastChangeDescription = &description
	}
	return h.do(func(api *goclientnew.ClientWithResponses) (cubapi.APIResponse, error) {
		res, err := api.UploadUnitDataWithBodyWithResponse(ctx, spaceID, unitID, params,
			"application/octet-stream", strings.NewReader(string(data)))
		if cubapi.IsAPIError(err, res) {
			return res, fmt.Errorf("writing unit data: %w", cubapi.InterpretErrorGeneric(err, res))
		}
		return res, nil
	})
}

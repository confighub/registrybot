// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	otelprom "go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Outcome counters, recorded with OpenTelemetry and served in the Prometheus
// text format at /metrics by the OpenTelemetry Prometheus exporter, which
// turns an instrument named registrybot.reconciles into
// registrybot_reconciles_total.
//
// registrybot.reconciles counts reconciles of one repository, from a poll or a
// settled webhook: written when the fact unit got a new revision (including
// the one a new unit is created with), unchanged when the facts were already
// current and nothing was written, and failure when the reconcile returned an
// error; the next poll retries it. A reconcile that fails because registrybot
// is shutting down is not counted.
//
// registrybot.webhook_deliveries counts deliveries to POST /webhooks/github,
// one per request, by what the bot did with it: queued when it named a
// watched repository, discovered when it named one the bot was not watching
// and discovery admitted it, ignored when it was acknowledged with nothing to
// reconcile (a ping, another event, a package that is not a container, or an
// unwatched repository discovery did not admit), and rejected when it was
// answered with an error (unreadable, too large, a bad signature, or a payload
// without a package). It counts deliveries, not repositories: one image push
// is many deliveries for the same repository. Nothing is counted while the
// receiver is disabled (no GITHUB_WEBHOOK_SECRET).
const meterName = "github.com/confighub/registrybot"

// Values of the result attribute.
const (
	resultWritten    = "written"
	resultUnchanged  = "unchanged"
	resultFailure    = "failure"
	resultQueued     = "queued"
	resultDiscovered = "discovered"
	resultIgnored    = "ignored"
	resultRejected   = "rejected"
)

// instruments holds the counters. OpenTelemetry instruments are safe to use
// from the worker and the webhook handler at once.
type instruments struct {
	reconciles metric.Int64Counter
	deliveries metric.Int64Counter
}

// meters holds the package's counters. Until main installs real ones with
// setMetrics they record to a no-op provider, so code paths that count work
// without metrics being set up (as in most tests). It is an atomic pointer so
// that installing the counters is safe whenever it happens, not only before
// the reconciler and the HTTP server start.
var meters atomic.Pointer[instruments]

func init() {
	meters.Store(mustNewInstruments(noop.NewMeterProvider()))
}

func newInstruments(mp metric.MeterProvider) (*instruments, error) {
	m := mp.Meter(meterName)
	reconciles, err := m.Int64Counter("registrybot.reconciles",
		metric.WithDescription("Reconciles of one repository's fact unit, by result."),
		metric.WithUnit("{reconcile}"))
	if err != nil {
		return nil, err
	}
	deliveries, err := m.Int64Counter("registrybot.webhook_deliveries",
		metric.WithDescription("GitHub webhook deliveries, by what registrybot did with them."),
		metric.WithUnit("{delivery}"))
	if err != nil {
		return nil, err
	}
	return &instruments{reconciles: reconciles, deliveries: deliveries}, nil
}

func mustNewInstruments(mp metric.MeterProvider) *instruments {
	i, err := newInstruments(mp)
	if err != nil {
		panic(err)
	}
	return i
}

// setMetrics points the package's counters at mp. Every series is created at
// zero, so each is exported from the start: a series Prometheus first sees at
// 1 hides that increment from increase() and rate(), and an alert on a
// failure series would otherwise see no data until the first failure.
func setMetrics(mp metric.MeterProvider) error {
	i, err := newInstruments(mp)
	if err != nil {
		return err
	}
	ctx := context.Background()
	for _, r := range []string{resultWritten, resultUnchanged, resultFailure} {
		i.reconciles.Add(ctx, 0, result(r))
	}
	for _, r := range []string{resultQueued, resultDiscovered, resultIgnored, resultRejected} {
		i.deliveries.Add(ctx, 0, result(r))
	}
	meters.Store(i)
	return nil
}

func result(r string) metric.AddOption {
	return metric.WithAttributes(attribute.String("result", r))
}

// countReconcile records the result of one reconcile.
func countReconcile(ctx context.Context, written bool, err error) {
	m := meters.Load()
	switch {
	case err != nil:
		if ctx.Err() == nil {
			m.reconciles.Add(ctx, 1, result(resultFailure))
		}
	case written:
		m.reconciles.Add(ctx, 1, result(resultWritten))
	default:
		m.reconciles.Add(ctx, 1, result(resultUnchanged))
	}
}

// countDelivery records what was done with one webhook delivery: one of
// resultQueued, resultDiscovered, resultIgnored or resultRejected.
func countDelivery(r string) {
	meters.Load().deliveries.Add(context.Background(), 1, result(r))
}

// newPrometheusMetrics builds a MeterProvider whose metrics are exported to a
// fresh Prometheus registry, and a handler serving that registry. The registry
// holds only registrybot's own counters: not the Go runtime collectors of the
// default registry, and not target_info. target_info would export the
// MeterProvider's resource, which the SDK always merges with
// OTEL_RESOURCE_ATTRIBUTES and OTEL_SERVICE_NAME; every variable registrybot
// reads is listed in manifests/registrybot.yaml, and these are not.
// Prometheus identifies the target by its job and instance labels instead.
func newPrometheusMetrics() (*sdkmetric.MeterProvider, http.Handler, error) {
	reg := prometheus.NewRegistry()
	exporter, err := otelprom.New(otelprom.WithRegisterer(reg),
		otelprom.WithoutScopeInfo(), otelprom.WithoutTargetInfo())
	if err != nil {
		return nil, nil, fmt.Errorf("prometheus exporter: %w", err)
	}
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	return mp, promhttp.HandlerFor(reg, promhttp.HandlerOpts{}), nil
}

// setupMetrics points the package's counters at a Prometheus-exported
// MeterProvider and returns the handler that serves them. main calls it before
// anything counts, so no count is lost to the no-op counters. It is
// best-effort: on failure it warns, leaves the counters as no-ops, and returns
// nil.
func setupMetrics() http.Handler {
	mp, handler, err := newPrometheusMetrics()
	if err == nil {
		err = setMetrics(mp)
	}
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return nil
	}
	return handler
}

// serveMetrics serves handler at /metrics on addr until ctx is cancelled. A nil
// handler (metrics disabled) or a listener that cannot be opened returns at
// once rather than stopping registrybot.
func serveMetrics(ctx context.Context, addr string, handler http.Handler) {
	if handler == nil {
		return
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		log.Printf("[WARN] metrics disabled: %v", err)
		return
	}
	serveMetricsOn(ctx, ln, handler)
}

// serveMetricsOn serves handler at /metrics on ln until ctx is cancelled, then
// closes it.
func serveMetricsOn(ctx context.Context, ln net.Listener, handler http.Handler) {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", handler)
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Printf("[INFO] metrics: serving on %s/metrics", ln.Addr())

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Printf("[WARN] metrics server stopped: %v", err)
	}
}

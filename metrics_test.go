// Copyright (C) ConfigHub, Inc.
// SPDX-License-Identifier: MIT

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// useTestMetrics points the package's counters at a fresh in-memory reader for
// the duration of the test. The counters are package state, so tests that use
// it must not call t.Parallel.
func useTestMetrics(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	prev := meters.Load()
	if err := setMetrics(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { meters.Store(prev) })
	return reader
}

// collectCounts returns every counter data point, keyed "name/result".
func collectCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok || !sum.IsMonotonic {
				t.Fatalf("%s is %T, want a monotonic int64 sum", m.Name, m.Data)
			}
			for _, dp := range sum.DataPoints {
				key := m.Name
				if r, ok := dp.Attributes.Value("result"); ok {
					key += "/" + r.AsString()
				}
				counts[key] += dp.Value
			}
		}
	}
	return counts
}

func assertCounts(t *testing.T, got, want map[string]int64) {
	t.Helper()
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %d, want %d", k, got[k], w)
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok && g != 0 {
			t.Errorf("%s = %d, want 0", k, g)
		}
	}
}

func TestReconcileMetrics(t *testing.T) {
	reader := useTestMetrics(t)
	ctx := context.Background()

	countReconcile(ctx, true, nil)
	countReconcile(ctx, false, nil)
	countReconcile(ctx, false, nil)
	countReconcile(ctx, false, errors.New("boom"))

	// A reconcile that fails because the bot is shutting down is not a failure.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	countReconcile(cancelled, false, context.Canceled)

	assertCounts(t, collectCounts(t, reader), map[string]int64{
		"registrybot.reconciles/written":   1,
		"registrybot.reconciles/unchanged": 2,
		"registrybot.reconciles/failure":   1,
	})
}

// TestWebhookDeliveryMetrics delivers through the handler, so it fails if a
// path stops counting or counts as the wrong result. It counts deliveries:
// the same unwatched repository delivered three times is three.
func TestWebhookDeliveryMetrics(t *testing.T) {
	reader := useTestMetrics(t)
	r := newDiscoveryReconciler(t)
	h := githubWebhookHandler("s3cret", r)
	withRepo := func(repo string) []byte {
		return []byte(strings.ReplaceAll(packageEvent, "confighub/argobot", repo))
	}

	deliver(t, h, "s3cret", "package", []byte(packageEvent), true) // explicit: queued
	deliver(t, h, "s3cret", "package", withRepo("acme/api"), true) // discovered
	deliver(t, h, "s3cret", "package", withRepo("acme/api"), true) // now watched: queued
	for i := 0; i < 3; i++ {
		deliver(t, h, "s3cret", "package", withRepo("stranger/thing"), true) // not admitted: ignored
	}
	deliver(t, h, "s3cret", "ping", []byte(`{}`), true)                        // ignored
	deliver(t, h, "s3cret", "issues", []byte(`{}`), true)                      // ignored
	deliver(t, h, "s3cret", "package", []byte(packageEvent), false)            // bad signature: rejected
	deliver(t, h, "s3cret", "package", []byte(`{"action":"published"}`), true) // no package: rejected

	// With the receiver disabled nothing is counted.
	deliver(t, githubWebhookHandler("", r), "", "package", []byte(packageEvent), false)

	assertCounts(t, collectCounts(t, reader), map[string]int64{
		"registrybot.webhook_deliveries/queued":     2,
		"registrybot.webhook_deliveries/discovered": 1,
		"registrybot.webhook_deliveries/ignored":    5,
		"registrybot.webhook_deliveries/rejected":   2,
	})
}

// TestPrometheusExposition checks the names and labels the exporter gives the
// counters, which dashboards and alerts depend on.
func TestPrometheusExposition(t *testing.T) {
	// OTel's own environment variables, which registrybot does not declare,
	// must not reach /metrics.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "leaked=yes")
	t.Setenv("OTEL_SERVICE_NAME", "leaked")
	prev := meters.Load()
	t.Cleanup(func() { meters.Store(prev) })
	handler := setupMetrics()
	if handler == nil {
		t.Fatal("setupMetrics = nil")
	}

	// Every series is exported at zero before anything is counted.
	fresh := scrape(t, handler)
	for _, want := range []string{
		`registrybot_reconciles_total{result="written"} 0` + "\n",
		`registrybot_reconciles_total{result="unchanged"} 0` + "\n",
		`registrybot_reconciles_total{result="failure"} 0` + "\n",
		`registrybot_webhook_deliveries_total{result="queued"} 0` + "\n",
		`registrybot_webhook_deliveries_total{result="discovered"} 0` + "\n",
		`registrybot_webhook_deliveries_total{result="ignored"} 0` + "\n",
		`registrybot_webhook_deliveries_total{result="rejected"} 0` + "\n",
	} {
		if !strings.Contains(fresh, want) {
			t.Errorf("metrics before any count missing %q:\n%s", want, fresh)
		}
	}
	if strings.Contains(fresh, "leaked") || strings.Contains(fresh, "target_info") {
		t.Errorf("metrics carry target_info or OTEL_* environment attributes:\n%s", fresh)
	}

	ctx := context.Background()
	countReconcile(ctx, true, nil)
	countReconcile(ctx, false, nil)
	countReconcile(ctx, false, errors.New("boom"))
	countReconcile(ctx, false, errors.New("boom"))
	countDelivery(resultDiscovered)
	countDelivery(resultIgnored)
	countDelivery(resultIgnored)

	text := scrape(t, handler)
	for _, want := range []string{
		"# TYPE registrybot_reconciles_total counter\n",
		`registrybot_reconciles_total{result="written"} 1` + "\n",
		`registrybot_reconciles_total{result="unchanged"} 1` + "\n",
		`registrybot_reconciles_total{result="failure"} 2` + "\n",
		"# TYPE registrybot_webhook_deliveries_total counter\n",
		`registrybot_webhook_deliveries_total{result="discovered"} 1` + "\n",
		`registrybot_webhook_deliveries_total{result="ignored"} 2` + "\n",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("metrics missing %q:\n%s", want, text)
		}
	}
}

// scrape fetches /metrics from handler in the Prometheus text format.
func scrape(t *testing.T, handler http.Handler) string {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Errorf("Content-Type = %q, want the Prometheus text format", ct)
	}
	body, _ := io.ReadAll(rec.Body)
	return string(body)
}

func TestServeMetrics(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	go func() { serveMetricsOn(ctx, ln, handler); close(done) }()

	resp, err := http.Get("http://" + ln.Addr().String() + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Errorf("GET /metrics = %d:\n%s", resp.StatusCode, body)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveMetricsOn did not return after cancel")
	}
}

func TestServeMetricsNotServing(t *testing.T) {
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer taken.Close()
	handler := http.NotFoundHandler()

	// Best-effort: an address that cannot be opened, or metrics that could not
	// be set up (a nil handler), return at once rather than blocking.
	for name, call := range map[string]func(){
		"address in use":   func() { serveMetrics(context.Background(), taken.Addr().String(), handler) },
		"metrics disabled": func() { serveMetrics(context.Background(), "127.0.0.1:0", nil) },
	} {
		done := make(chan struct{})
		go func() { call(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: serveMetrics blocked", name)
		}
	}
}

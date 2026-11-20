package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

func TestLoggerRedactsSecretsAndDocumentContent(t *testing.T) {
	var output bytes.Buffer
	logger := NewLogger(LogConfig{Writer: &output, Level: slog.LevelDebug, Service: "test", NodeID: "node-a", Incarnation: "process-a"})
	logger.Info("request",
		"authorization", "Bearer access-secret",
		"refresh_token", "refresh-secret",
		"mysql_dsn", "user:password@tcp(database)",
		"document_content", "private document",
		slog.Group("nested", "password", "password-secret", "safe", "visible"),
	)
	encoded := output.String()
	for _, secret := range []string{"access-secret", "refresh-secret", "user:password", "private document", "password-secret"} {
		if strings.Contains(encoded, secret) {
			t.Fatalf("log leaked %q: %s", secret, encoded)
		}
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatal(err)
	}
	if entry["service"] != "test" || entry["node_id"] != "node-a" {
		t.Fatalf("missing structured context: %#v", entry)
	}
	if entry["timestamp"] == nil || entry["level"] != "INFO" {
		t.Fatalf("unexpected log envelope: %#v", entry)
	}
}

func TestMetricsUseOnlyBoundedLabelsAndInstrumentHTTP(t *testing.T) {
	metrics := NewMetrics()
	handler := metrics.InstrumentHTTP("public", http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusCreated)
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/documents/private-doc-id", nil))
	families, err := metrics.registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool, len(families))
	for _, family := range families {
		seen[family.GetName()] = true
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				for _, forbidden := range []string{"user_id", "doc_id", "document_id", "request_id"} {
					if label.GetName() == forbidden {
						t.Fatalf("metric %s contains high-cardinality label %s", family.GetName(), forbidden)
					}
				}
			}
		}
	}
	for _, name := range []string{"go_goroutines", "go_memstats_heap_alloc_bytes"} {
		if !seen[name] {
			t.Errorf("missing runtime metric %s", name)
		}
	}
	if runtime.GOOS == "linux" && !seen["process_resident_memory_bytes"] {
		t.Error("missing Linux process resident memory metric")
	}
}

func TestPresenceHeartbeatGaugeReturnsToZero(t *testing.T) {
	metrics := NewMetrics()
	metrics.RedisEvent("presence_heartbeat", "started")
	metrics.RedisEvent("presence_heartbeat", "started")
	metrics.RedisEvent("presence_heartbeat", "stopped")
	check := func(want float64) {
		t.Helper()
		families, err := metrics.registry.Gather()
		if err != nil {
			t.Fatal(err)
		}
		for _, family := range families {
			if family.GetName() == "collab_presence_heartbeats_active" {
				if got := family.Metric[0].GetGauge().GetValue(); got != want {
					t.Fatalf("active heartbeats = %v, want %v", got, want)
				}
				return
			}
		}
		t.Fatal("missing presence heartbeat gauge")
	}
	check(1)
	metrics.RedisEvent("presence_heartbeat", "stopped")
	check(0)
}

func TestHTTPInstrumentationPreservesWebSocketUpgrade(t *testing.T) {
	metrics := NewMetrics()
	handler := otelhttp.NewHandler(metrics.InstrumentHTTP("public", http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := websocket.Accept(writer, request, nil)
		if err != nil {
			t.Errorf("accept websocket: %v", err)
			return
		}
		_ = connection.Close(websocket.StatusNormalClosure, "test complete")
	})), "test.http")
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	connection, response, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v (status %d)", err, response.StatusCode)
		}
		t.Fatalf("dial websocket: %v", err)
	}
	_ = connection.CloseNow()
}

func TestHealthReadinessAndConcurrentAccess(t *testing.T) {
	health := NewHealth()
	health.Register("mysql", func(context.Context) error { return nil })
	health.Register("redis", func(context.Context) error { return errors.New("unavailable") })
	health.SetReady(true)
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			response := httptest.NewRecorder()
			health.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/readyz", nil))
			if response.Code != http.StatusServiceUnavailable {
				t.Errorf("readiness status = %d", response.Code)
			}
		}()
	}
	wait.Wait()
	health.Stop()
	response := httptest.NewRecorder()
	health.Handler(http.NotFoundHandler()).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/livez", nil))
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("stopped liveness status = %d", response.Code)
	}
}

func TestTracingDefaultsToNoopAndPropagatesW3CContext(t *testing.T) {
	shutdown, err := ConfigureTracing(context.Background(), TraceConfig{ServiceName: "test"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	carrier := propagation.MapCarrier{"traceparent": "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"}
	ctx := otel.GetTextMapPropagator().Extract(context.Background(), carrier)
	if !trace.SpanContextFromContext(ctx).IsValid() {
		t.Fatal("W3C trace context was not extracted")
	}
}

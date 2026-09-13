package worker

import (
	"bytes"
	"compress/gzip"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pgvector/pgvector-go"
	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	resourcepb "go.opentelemetry.io/proto/otlp/resource/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"
	"google.golang.org/protobuf/proto"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// strAttr builds an OTLP string-valued attribute.
func strAttr(key, val string) *commonpb.KeyValue {
	return &commonpb.KeyValue{
		Key:   key,
		Value: &commonpb.AnyValue{Value: &commonpb.AnyValue_StringValue{StringValue: val}},
	}
}

// makeSpan builds a minimal OTLP span. traceID/spanID drive dedup, so reuse
// them to simulate a resend. The namespace is supplied separately to processSpan
// as the resource-level k8s.namespace.name (per OTEL convention), not on the span.
func makeSpan(traceID, spanID string) *tracepb.Span {
	start := uint64(time.Now().Add(-time.Second).UnixNano())
	return &tracepb.Span{
		TraceId:           []byte(traceID),
		SpanId:            []byte(spanID),
		Name:              "GET /api/things",
		Kind:              tracepb.Span_SPAN_KIND_SERVER,
		StartTimeUnixNano: start,
		EndTimeUnixNano:   start + uint64(5*time.Millisecond),
		Attributes:        []*commonpb.KeyValue{strAttr("http.route", "/api/things")},
	}
}

func newTestTraceWorker(t *testing.T, scope nsscope.Scope) (*traceWorker, *llm.MockClient, database.Interface) {
	t.Helper()
	// Initializes the global logger; the span-drop path logs at debug level.
	testutil.SetupTestEnvironment(t)
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	mockLLM, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to llm.MockClient")
	}
	w, ok := NewTraceWorker(dbClient, llmClient, ":0", scope).(*traceWorker)
	if !ok {
		t.Fatal("failed to cast to *traceWorker")
	}
	return w, mockLLM, dbClient
}

func countTraceEntries(t *testing.T, db database.Interface) []*database.TraceEntry {
	t.Helper()
	results, err := db.SearchSimilar(pgvector.NewVector(make([]float32, 3)), 100)
	if err != nil {
		t.Fatalf("SearchSimilar failed: %v", err)
	}
	traces := make([]*database.TraceEntry, 0)
	for i := range results {
		if results[i].TraceEntry != nil {
			traces = append(traces, results[i].TraceEntry)
		}
	}
	return traces
}

func TestNewTraceWorker(t *testing.T) {
	w, _, _ := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))
	if w.Type() != "traces" {
		t.Errorf("expected type 'traces', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !w.IsHealthy() {
		t.Error("expected new worker to be healthy")
	}
}

// TestTraceWorker_IngestsOnlyAllowedNamespaces is the anti-loop guarantee: only
// spans from the configured (dkjr) namespace are embedded and stored; accretion's own
// traces and untagged spans are dropped BEFORE any embedding call is made.
func TestTraceWorker_IngestsOnlyAllowedNamespaces(t *testing.T) {
	w, mockLLM, dbClient := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	if err := w.processSpan("dkjr", "dkjr", makeSpan("trace-dkjr", "span-dkjr")); err != nil {
		t.Fatalf("dkjr span: unexpected error: %v", err)
	}
	if err := w.processSpan("accretion-api", "accretion-core", makeSpan("trace-accretion", "span-accretion")); err != nil {
		t.Fatalf("accretion-core span: unexpected error: %v", err)
	}
	if err := w.processSpan("mystery", "", makeSpan("trace-none", "span-none")); err != nil {
		t.Fatalf("empty-namespace span: unexpected error: %v", err)
	}

	// The embedding LLM must be called exactly once (dkjr only). A call for the
	// accretion-core span is what would create the trace feedback loop.
	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected exactly 1 embedding call (dkjr only), got %d", got)
	}

	traces := countTraceEntries(t, dbClient)
	if len(traces) != 1 {
		t.Fatalf("expected exactly 1 stored trace entry, got %d", len(traces))
	}
	if traces[0].Namespace != "dkjr" {
		t.Errorf("stored trace namespace = %q, want dkjr", traces[0].Namespace)
	}
	if traces[0].Service != "dkjr" {
		t.Errorf("stored trace service = %q, want dkjr", traces[0].Service)
	}
	if traces[0].Summary == "" {
		t.Error("expected populated trace summary")
	}
}

// TestTraceWorker_EmptyAllowlistAllowsAll documents that a nil/empty allowlist
// disables filtering (used only where scoping is intentionally off).
func TestTraceWorker_EmptyAllowlistAllowsAll(t *testing.T) {
	w, mockLLM, _ := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{}))
	if err := w.processSpan("accretion-api", "accretion-core", makeSpan("trace-accretion", "span-accretion")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected span to be ingested with empty allowlist, embedding calls = %d", got)
	}
}

// TestTraceWorker_DedupsRepeatedSpans verifies the same trace_id+span_id is only
// embedded/stored once even if delivered twice.
func TestTraceWorker_DedupsRepeatedSpans(t *testing.T) {
	w, mockLLM, dbClient := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	if err := w.processSpan("dkjr", "dkjr", makeSpan("trace-dup", "span-dup")); err != nil {
		t.Fatalf("first ingest: unexpected error: %v", err)
	}
	if err := w.processSpan("dkjr", "dkjr", makeSpan("trace-dup", "span-dup")); err != nil {
		t.Fatalf("second ingest: unexpected error: %v", err)
	}

	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected 1 embedding call after dedup, got %d", got)
	}
	if traces := countTraceEntries(t, dbClient); len(traces) != 1 {
		t.Fatalf("expected 1 stored trace after dedup, got %d", len(traces))
	}
}

// TestTraceWorker_FallsBackToSpanNamespace verifies that when the resource has
// no k8s.namespace.name, the worker falls back to a span-level attribute.
func TestTraceWorker_FallsBackToSpanNamespace(t *testing.T) {
	w, mockLLM, dbClient := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	span := makeSpan("trace-fb", "span-fb")
	span.Attributes = append(span.Attributes, strAttr("k8s.namespace.name", "dkjr"))

	// Empty resource namespace forces the span-attribute fallback.
	if err := w.processSpan("dkjr", "", span); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected span-attr fallback to ingest (1 embedding call), got %d", got)
	}
	if traces := countTraceEntries(t, dbClient); len(traces) != 1 {
		t.Fatalf("expected 1 stored trace via fallback, got %d", len(traces))
	}
}

// TestHandleTraces_GzipEncoded verifies the OTLP receiver decompresses
// gzip-encoded request bodies — the OpenTelemetry Collector's otlphttp exporter
// gzips by default, so without this the real collector→worker path fails to
// unmarshal every request.
func TestHandleTraces_GzipEncoded(t *testing.T) {
	w, mockLLM, dbClient := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	span := makeSpan("trace-gz", "span-gz")
	exportReq := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "dkjr"),
				strAttr("k8s.namespace.name", "dkjr"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{span}}},
		}},
	}
	raw, err := proto.Marshal(exportReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	if _, err := gz.Write(raw); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", &buf)
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	w.handleTraces(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	// Ingestion is async; wait briefly for the embed+store to complete.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if mockLLM.GetEmbeddingCallCount() >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected gzip body to be decoded and ingested (1 embedding call), got %d", got)
	}
	if traces := countTraceEntries(t, dbClient); len(traces) != 1 {
		t.Fatalf("expected 1 stored trace from gzip request, got %d", len(traces))
	}
}

// A collector configured with a compression algorithm the worker doesn't
// support (e.g. zstd) must be rejected with 415 rather than having its body
// silently mis-parsed as protobuf.
func TestHandleTraces_UnsupportedEncodingRejected(t *testing.T) {
	w, mockLLM, _ := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", strings.NewReader("compressed-bytes"))
	req.Header.Set("Content-Type", "application/x-protobuf")
	req.Header.Set("Content-Encoding", "zstd")
	rec := httptest.NewRecorder()

	w.handleTraces(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("expected 415 for unsupported encoding, got %d", rec.Code)
	}
	if got := mockLLM.GetEmbeddingCallCount(); got != 0 {
		t.Fatalf("expected no ingestion for rejected request, got %d embedding calls", got)
	}
}

// TestTraceWorker_StopDrainsInFlight verifies that Stop waits for span
// processing accepted before shutdown to complete rather than dropping it.
func TestTraceWorker_StopDrainsInFlight(t *testing.T) {
	w, mockLLM, dbClient := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	// Accept an export request the normal way, then immediately begin shutdown.
	exportReq := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "dkjr"),
				strAttr("k8s.namespace.name", "dkjr"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{makeSpan("trace-drain", "span-drain")}}},
		}},
	}
	w.inflight.Add(1)
	go func() {
		defer w.inflight.Done()
		w.processExportRequest(exportReq)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	// After Stop returns, the in-flight span must have been fully processed.
	if got := mockLLM.GetEmbeddingCallCount(); got != 1 {
		t.Fatalf("expected in-flight span drained by Stop (1 embedding call), got %d", got)
	}
	if traces := countTraceEntries(t, dbClient); len(traces) != 1 {
		t.Fatalf("expected 1 stored trace after drain, got %d", len(traces))
	}
}

// TestTraceWorker_RejectsNewWorkAfterStop verifies handleTraces refuses new
// export requests with 503 once shutdown has begun.
func TestTraceWorker_RejectsNewWorkAfterStop(t *testing.T) {
	w, _, _ := newTestTraceWorker(t, nsscope.Static(config.NamespaceSelector{Include: []string{"dkjr"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.Stop(ctx); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	exportReq := &collectorpb.ExportTraceServiceRequest{
		ResourceSpans: []*tracepb.ResourceSpans{{
			Resource: &resourcepb.Resource{Attributes: []*commonpb.KeyValue{
				strAttr("service.name", "dkjr"),
				strAttr("k8s.namespace.name", "dkjr"),
			}},
			ScopeSpans: []*tracepb.ScopeSpans{{Spans: []*tracepb.Span{makeSpan("trace-late", "span-late")}}},
		}},
	}
	raw, err := proto.Marshal(exportReq)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/traces", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/x-protobuf")
	rec := httptest.NewRecorder()

	w.handleTraces(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 after shutdown, got %d", rec.Code)
	}
}

func TestComposeTraceSummary(t *testing.T) {
	summary := composeTraceSummary(
		"dkjr", "GET /api/things", "STATUS_CODE_ERROR", 12,
		map[string]string{"http.route": "/api/things", "http.method": "GET"},
		[]string{"boom"},
	)
	for _, want := range []string{"dkjr", "GET /api/things", "12ms", "ERROR", "route=/api/things", "method=GET", "errors: boom"} {
		if !strings.Contains(summary, want) {
			t.Errorf("summary %q missing %q", summary, want)
		}
	}
}

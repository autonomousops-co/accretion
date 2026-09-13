package worker

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/pgvector/pgvector-go"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"

	collectorpb "go.opentelemetry.io/proto/otlp/collector/trace/v1"
	commonpb "go.opentelemetry.io/proto/otlp/common/v1"
	tracepb "go.opentelemetry.io/proto/otlp/trace/v1"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/resilience"
)

// traceWorker processes OTLP trace data received via HTTP.
type traceWorker struct {
	shutdownCtx context.Context
	db          database.Interface
	llm         llm.Client
	server      *http.Server
	seenHash    *lruCache
	sem         chan struct{}
	shutdownCh  chan struct{}
	*BaseWorker
	shutdownCancel context.CancelFunc
	listenAddr     string
	inflight       sync.WaitGroup
	shutdownOnce   sync.Once
}

// NewTraceWorker creates a new trace worker. scope decides which workload
// namespaces' traces are ingested into RAG; spans from any other namespace
// (notably accretion's own) are dropped before embedding so ingestion cannot feed
// back into itself. A scope over a selector with an empty Include allows all
// namespaces (minus its Exclude); it is used only in tests that don't exercise
// filtering.
func NewTraceWorker(
	db database.Interface, llm llm.Client, listenAddr string, scope nsscope.Scope,
) TraceWorker {
	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())
	return &traceWorker{
		BaseWorker:     NewBaseWorker("traces", nil, scope),
		db:             db,
		llm:            llm,
		seenHash:       newLRUCache(50000),
		listenAddr:     listenAddr,
		sem:            make(chan struct{}, 100),
		shutdownCh:     make(chan struct{}),
		shutdownCtx:    shutdownCtx,
		shutdownCancel: shutdownCancel,
	}
}

func (w *traceWorker) Start(ctx context.Context) error {
	logger.Logger.Info("Starting trace worker",
		zap.String("worker_id", w.ID()),
		zap.String("listen_addr", w.listenAddr))

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/traces", w.handleTraces)

	w.server = &http.Server{
		Addr:              w.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		if err := w.server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("trace worker HTTP server failed: %w", err)
	case <-ctx.Done():
		return w.server.Shutdown(context.Background())
	}
}

func (w *traceWorker) Stop(ctx context.Context) error {
	// Stop accepting new export requests before draining, so the in-flight set
	// can only shrink from here on.
	w.beginShutdown()

	if w.server != nil {
		if err := w.server.Shutdown(ctx); err != nil {
			logger.Logger.Error("Failed to shutdown trace HTTP server", zap.Error(err))
		}
	}

	// Wait for in-flight span processing to finish so spans that were already
	// accepted (200 returned to the exporter) are not lost on rollout. The wait
	// is bounded by ctx: if it expires, cancel the shutdown context to unblock
	// long-running embedding/DB calls and semaphore acquisitions, then give the
	// goroutines a final moment to observe cancellation and exit.
	done := make(chan struct{})
	go func() {
		w.inflight.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-ctx.Done():
		logger.Logger.Warn("Trace worker drain timed out; canceling in-flight span processing",
			zap.String("worker_id", w.ID()))
		w.shutdownCancel()
		<-done
		return ctx.Err()
	}
}

// beginShutdown idempotently marks the worker as shutting down so handleTraces
// stops accepting new export requests.
func (w *traceWorker) beginShutdown() {
	w.shutdownOnce.Do(func() { close(w.shutdownCh) })
}

// isShuttingDown reports whether Stop has begun.
func (w *traceWorker) isShuttingDown() bool {
	select {
	case <-w.shutdownCh:
		return true
	default:
		return false
	}
}

// handleTraces handles POST /v1/traces with OTLP protobuf payload.
func (w *traceWorker) handleTraces(resp http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(resp, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Reject unsupported Content-Encodings before buffering. OTLP/HTTP exporters
	// (including the OpenTelemetry Collector's otlphttp exporter) gzip-compress
	// request bodies by default and set Content-Encoding; only gzip and the
	// identity encoding are supported here. A collector configured with e.g.
	// zstd would otherwise have its body silently mis-parsed, so surface a clear
	// 415 instead of decoding garbage.
	encoding := strings.TrimSpace(req.Header.Get("Content-Encoding"))
	if encoding != "" && !strings.EqualFold(encoding, "gzip") && !strings.EqualFold(encoding, "identity") {
		logger.Logger.Warn("Rejecting trace request with unsupported Content-Encoding",
			zap.String("content_encoding", encoding))
		http.Error(resp, "unsupported content-encoding: "+encoding, http.StatusUnsupportedMediaType)
		return
	}

	// Gate request admission on the same semaphore that bounds concurrent span
	// processing, acquiring it BEFORE buffering the (possibly 64 MB decompressed)
	// body. Without this, many concurrent large POSTs could each allocate up to
	// the decompressed cap in io.ReadAll before any processing capacity is
	// checked. Acquire with the request context so a client disconnect while
	// waiting for a slot doesn't leak a blocked goroutine.
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-req.Context().Done():
		http.Error(resp, "request canceled", http.StatusRequestTimeout)
		return
	}

	req.Body = http.MaxBytesReader(resp, req.Body, 10*1024*1024) // 10MB limit
	defer req.Body.Close()

	// Decompress before unmarshaling, otherwise proto.Unmarshal sees gzip bytes.
	var reader io.Reader = req.Body
	if strings.EqualFold(encoding, "gzip") {
		gz, gzErr := gzip.NewReader(req.Body)
		if gzErr != nil {
			logger.Logger.Error("Failed to create gzip reader for trace request", zap.Error(gzErr))
			http.Error(resp, "invalid gzip body", http.StatusBadRequest)
			return
		}
		defer gz.Close()
		// Bound the decompressed size to guard against decompression bombs.
		reader = io.LimitReader(gz, 64*1024*1024)
	}

	body, err := io.ReadAll(reader)
	if err != nil {
		logger.Logger.Error("Failed to read trace request body", zap.Error(err))
		http.Error(resp, "failed to read body", http.StatusBadRequest)
		return
	}

	var exportReq collectorpb.ExportTraceServiceRequest
	if err := proto.Unmarshal(body, &exportReq); err != nil {
		logger.Logger.Error("Failed to unmarshal OTLP trace request", zap.Error(err))
		http.Error(resp, "invalid protobuf", http.StatusBadRequest)
		return
	}

	// Refuse new work once shutdown has begun so we don't accept spans we can't
	// promise to drain. Exporters retry on 503, so this does not drop data.
	if w.isShuttingDown() {
		http.Error(resp, "shutting down", http.StatusServiceUnavailable)
		return
	}

	// Track the request so Stop can wait for it to drain. Add before spawning to
	// avoid a race where Stop's Wait observes zero while the goroutine starts.
	w.inflight.Add(1)
	go func() {
		defer w.inflight.Done()
		w.processExportRequest(&exportReq)
	}()

	resp.WriteHeader(http.StatusOK)
}

// processExportRequest processes all resource spans in an export request.
func (w *traceWorker) processExportRequest(exportReq *collectorpb.ExportTraceServiceRequest) {
	var wg sync.WaitGroup
	for _, rs := range exportReq.GetResourceSpans() {
		serviceName := extractServiceName(rs)
		resourceNamespace := extractResourceNamespace(rs)
		for _, ss := range rs.GetScopeSpans() {
			for _, span := range ss.GetSpans() {
				// Acquire the concurrency slot, but abort if the shutdown context
				// is canceled so a drain timeout can't leave us blocked forever.
				select {
				case w.sem <- struct{}{}:
				case <-w.shutdownCtx.Done():
					return
				}
				wg.Add(1)
				go func(svc, ns string, s *tracepb.Span) {
					defer wg.Done()
					defer func() { <-w.sem }()
					if err := w.processSpan(svc, ns, s); err != nil {
						logger.Logger.Error("Failed to process span",
							zap.String("worker_id", w.ID()),
							zap.String("service", svc),
							zap.String("span_name", s.GetName()),
							zap.Error(err))
						w.IncrementErrors()
					}
				}(serviceName, resourceNamespace, span)
			}
		}
	}
	wg.Wait()
}

// processSpan processes a single span and ingests it into the RAG database.
// resourceNamespace is k8s.namespace.name from the span's resource attributes.
func (w *traceWorker) processSpan(serviceName, resourceNamespace string, span *tracepb.Span) error {
	ctx, otelSpan := w.StartSpan(w.shutdownCtx, "processSpan")
	defer otelSpan.End()

	traceID := hex.EncodeToString(span.GetTraceId())
	spanID := hex.EncodeToString(span.GetSpanId())

	// Extract attributes as a map
	attrs := extractAttributes(span.GetAttributes())

	// Determine namespace (resource-level k8s.namespace.name, falling back to a
	// span-level attribute) and drop spans from namespaces outside the ingestion
	// allowlist BEFORE any other work (dedup, embedding, DB). This is what
	// prevents accretion's own traces — including those emitted by the embedding path
	// itself — from being ingested and looping back in, and keeps the dedup cache
	// free of non-ingested spans.
	namespace := resourceNamespace
	if namespace == "" {
		namespace = attrs["k8s.namespace.name"]
	}
	if !w.scope.Allows(namespace) {
		logger.Logger.Debug("dropping trace span from non-ingested namespace",
			zap.String("namespace", namespace),
			zap.String("service", serviceName))
		return nil
	}

	// Compute content hash for dedup: trace_id + span_id
	contentHash := fmt.Sprintf("%x", sha256.Sum256([]byte(traceID+"|"+spanID)))

	if w.seenHash.contains(contentHash) {
		return nil
	}
	w.seenHash.add(contentHash)

	// Compute duration
	startNano := span.GetStartTimeUnixNano()
	endNano := span.GetEndTimeUnixNano()
	durationMs := float64(endNano-startNano) / 1e6

	// Extract status
	statusCode := "UNSET"
	statusMsg := ""
	if span.GetStatus() != nil {
		statusCode = span.GetStatus().GetCode().String()
		statusMsg = span.GetStatus().GetMessage()
	}

	// Extract error events
	var errorEvents []string
	for _, event := range span.GetEvents() {
		if event.GetName() == "exception" {
			eventAttrs := extractAttributes(event.GetAttributes())
			if msg, ok := eventAttrs["exception.message"]; ok {
				errorEvents = append(errorEvents, msg)
			}
		}
	}

	// Compose human-readable text summary for embedding
	summary := composeTraceSummary(serviceName, span.GetName(), statusCode, durationMs, attrs, errorEvents)

	// Generate embedding
	var embedding []float32
	var err error

	err = w.llmBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			embedding, err = w.llm.GenerateEmbedding(ctx, summary)
			return err
		})
	})
	if err != nil {
		return fmt.Errorf("embedding generation failed: %w", err)
	}

	// Build data JSON
	data, err := json.Marshal(map[string]any{
		"trace_id":     traceID,
		"span_id":      spanID,
		"service":      serviceName,
		"operation":    span.GetName(),
		"kind":         span.GetKind().String(),
		"status_code":  statusCode,
		"status_msg":   statusMsg,
		"duration_ms":  durationMs,
		"attributes":   attrs,
		"error_events": errorEvents,
		"summary":      summary,
	})
	if err != nil {
		return fmt.Errorf("failed to marshal trace data: %w", err)
	}

	// start_time_unix_nano arrives from whatever sent the span, so the
	// conversion is guarded rather than assumed: a value above MaxInt64 would
	// wrap to a negative nanosecond count and land the entry in 1969, where it
	// is invisible to every time-bounded query. Anything past year 2262 is not a
	// real span start, so it falls back to ingest time.
	nanos := int64(0)
	if startNano > math.MaxInt64 {
		nanos = time.Now().UnixNano()
	} else {
		nanos = int64(startNano)
	}
	timestamp := time.Unix(0, nanos)

	dbEntry := &database.UnifiedEntry{
		ResourceType: database.ResourceTypeTrace,
		Namespace:    namespace,
		ResourceName: serviceName,
		Data:         data,
		Timestamp:    timestamp,
		ContentHash:  contentHash,
		Embedding:    pgvector.NewVector(embedding),
	}

	// Insert with resilience
	return w.dbBreaker.Execute(ctx, func(ctx context.Context) error {
		return resilience.WithRetry(ctx, resilience.DefaultRetryConfig(), func(ctx context.Context) error {
			return w.db.InsertEntry(dbEntry)
		})
	})
}

// extractServiceName extracts the service.name from resource attributes.
func extractServiceName(rs *tracepb.ResourceSpans) string {
	if rs.GetResource() == nil {
		return "unknown"
	}
	for _, attr := range rs.GetResource().GetAttributes() {
		if attr.GetKey() == "service.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return "unknown"
}

// extractResourceNamespace extracts k8s.namespace.name from resource attributes.
// Per OTEL semantic conventions this is a resource-level attribute (set once per
// pod), not a span attribute, so it must be read here rather than off the span.
func extractResourceNamespace(rs *tracepb.ResourceSpans) string {
	if rs.GetResource() == nil {
		return ""
	}
	for _, attr := range rs.GetResource().GetAttributes() {
		if attr.GetKey() == "k8s.namespace.name" {
			return attr.GetValue().GetStringValue()
		}
	}
	return ""
}

// extractAttributes converts OTLP KeyValue pairs to a string map.
func extractAttributes(attrs []*commonpb.KeyValue) map[string]string {
	result := make(map[string]string, len(attrs))
	for _, attr := range attrs {
		result[attr.GetKey()] = anyValueToString(attr.GetValue())
	}
	return result
}

// anyValueToString converts an OTLP AnyValue to its string representation.
func anyValueToString(val *commonpb.AnyValue) string {
	if val == nil {
		return ""
	}
	switch v := val.GetValue().(type) {
	case *commonpb.AnyValue_StringValue:
		return v.StringValue
	case *commonpb.AnyValue_IntValue:
		return fmt.Sprintf("%d", v.IntValue)
	case *commonpb.AnyValue_DoubleValue:
		return fmt.Sprintf("%.2f", v.DoubleValue)
	case *commonpb.AnyValue_BoolValue:
		return fmt.Sprintf("%t", v.BoolValue)
	default:
		return val.String()
	}
}

// composeTraceSummary builds a human-readable text summary suitable for embedding.
func composeTraceSummary(
	serviceName, operation, statusCode string,
	durationMs float64,
	attrs map[string]string,
	errorEvents []string,
) string {
	var sb strings.Builder

	fmt.Fprintf(&sb, "%s: %s took %.0fms", serviceName, operation, durationMs)

	switch statusCode {
	case "STATUS_CODE_ERROR":
		sb.WriteString(", ERROR")
	case "STATUS_CODE_OK":
		sb.WriteString(", OK")
	}

	// Add notable attributes
	if route, ok := attrs["http.route"]; ok {
		fmt.Fprintf(&sb, ", route=%s", route)
	}
	if method, ok := attrs["http.method"]; ok {
		fmt.Fprintf(&sb, ", method=%s", method)
	}
	if httpStatus, ok := attrs["http.status_code"]; ok {
		fmt.Fprintf(&sb, ", http_status=%s", httpStatus)
	}
	if dbSystem, ok := attrs["db.system"]; ok {
		fmt.Fprintf(&sb, ", db=%s", dbSystem)
	}
	if dbStatement, ok := attrs["db.statement"]; ok {
		// Truncate long DB statements
		if len(dbStatement) > 100 {
			dbStatement = dbStatement[:100] + "..."
		}
		fmt.Fprintf(&sb, ", query=%s", dbStatement)
	}

	if len(errorEvents) > 0 {
		fmt.Fprintf(&sb, ", errors: %s", strings.Join(errorEvents, "; "))
	}

	return sb.String()
}

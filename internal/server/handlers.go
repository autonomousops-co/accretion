package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	"github.com/autonomousops-co/accretion/internal/tempo"
)

const (
	defaultNamespace = "default"
)

type ChatRequest struct {
	Messages []llm.Message `json:"messages"`
	Stream   bool          `json:"stream"`
}

type ChatResponse struct {
	Usage   *llm.Usage `json:"usage,omitempty"`
	Content string     `json:"content"`
}

type LogsResponse struct {
	Logs []database.LogEntry `json:"logs"`
}

func (s *Server) chatHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.FromCtx(ctx)
	log.Info("Chat handler started")

	// Handle CORS preflight request
	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Max-Age", "86400")
		w.WriteHeader(http.StatusOK)
		return
	}

	if r.Method != http.MethodPost {
		log.Warn("Chat handler: invalid method", zap.String("method", r.Method))
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Set CORS headers for actual POST request
	w.Header().Set("Access-Control-Allow-Origin", "*")

	var req ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		log.Error("Chat handler: failed to decode JSON request", zap.Error(err))
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}
	log.Info("Chat handler: decoded request", zap.Int("messageCount", len(req.Messages)))

	if len(req.Messages) == 0 {
		log.Warn("Chat handler: no messages provided in request")
		http.Error(w, "No messages provided", http.StatusBadRequest)
		return
	}

	// Get the latest user message for context search
	userMessage := req.Messages[len(req.Messages)-1].Content
	log.Info("Chat handler: user message", zap.String("preview", userMessage[:minInt(50, len(userMessage))]+"..."))

	// Use shared clients
	if s.llmClient == nil || s.dbClient == nil {
		log.Error("Chat handler: shared clients not initialized")
		http.Error(w, "Server not ready", http.StatusServiceUnavailable)
		return
	}

	// Generate embedding for the user query
	log.Info("Chat handler: generating embedding for user query")
	embedding, err := s.llmClient.GenerateEmbedding(ctx, userMessage)
	if err != nil {
		log.Error("Chat handler: failed to generate embedding", zap.Error(err))
		http.Error(w, "Failed to generate embedding", http.StatusInternalServerError)
		return
	}
	log.Info("Chat handler: embedding generated successfully", zap.Int("dimensions", len(embedding)))

	// Extract time window from the user query
	log.Info("Chat handler: extracting time window from user query")
	searchOpts := s.extractTimeWindow(ctx, userMessage)
	if !searchOpts.StartTime.IsZero() || !searchOpts.EndTime.IsZero() {
		log.Info("Chat handler: time window extracted",
			zap.Time("start", searchOpts.StartTime), zap.Time("end", searchOpts.EndTime))
	}

	// Search for similar entries within the time window
	log.Info("Chat handler: searching for similar entries in database")
	queryVector := pgvector.NewVector(embedding)
	searchResults, err := s.dbClient.SearchSimilarWithOptions(queryVector, 10, searchOpts)
	if err != nil {
		log.Error("Chat handler: failed to search database", zap.Error(err))
		http.Error(w, "Failed to search database", http.StatusInternalServerError)
		return
	}
	log.Info("Chat handler: found similar entries", zap.Int("count", len(searchResults)))

	// Build context and enhanced messages
	contextData := s.buildContextFromResults(searchResults)
	enhancedMessages := s.buildEnhancedMessages(req.Messages, contextData)
	log.Info("Chat handler: context built, sending to LLM", zap.Int("contextChars", len(contextData)))
	response, err := s.llmClient.Chat(ctx, enhancedMessages)
	if err != nil {
		log.Error("Chat handler: failed to get LLM response", zap.Error(err))
		http.Error(w, "Failed to get LLM response", http.StatusInternalServerError)
		return
	}
	log.Info("Chat handler: received LLM response", zap.Int("length", len(response.Content)))

	log.Info("Chat handler: sending response to client")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(ChatResponse{
		Content: response.Content,
		Usage:   &response.Usage,
	}); err != nil {
		log.Error("Chat handler: failed to encode JSON response", zap.Error(err))
		return
	}

	log.Info("Chat handler: request completed successfully")
}

// searchHandler exposes raw semantic (vector) search over the store. It embeds the
// `q` query parameter and returns the matched unified entries above the similarity
// threshold, applying the same embed-then-search flow used by chatHandler.
func (s *Server) searchHandler(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logger.FromCtx(ctx)

	query := r.URL.Query().Get("q")
	if query == "" {
		http.Error(w, "Missing required query parameter: q", http.StatusBadRequest)
		return
	}

	limit := 10
	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			limit = l
		}
	}

	var opts database.SearchOptions
	if st := r.URL.Query().Get("start_time"); st != "" {
		if t, err := time.Parse(time.RFC3339, st); err == nil {
			opts.StartTime = t
		}
	}
	if et := r.URL.Query().Get("end_time"); et != "" {
		if t, err := time.Parse(time.RFC3339, et); err == nil {
			opts.EndTime = t
		}
	}

	if s.llmClient == nil || s.dbClient == nil {
		log.Error("Search handler: shared clients not initialized")
		http.Error(w, "Server not ready", http.StatusServiceUnavailable)
		return
	}

	embedding, err := s.llmClient.GenerateEmbedding(ctx, query)
	if err != nil {
		log.Error("Search handler: failed to generate embedding", zap.Error(err))
		http.Error(w, "Failed to generate embedding", http.StatusInternalServerError)
		return
	}

	// SearchEntries already applies the MinSimilarityThreshold cutoff.
	entries, err := s.dbClient.SearchEntries(pgvector.NewVector(embedding), limit, opts)
	if err != nil {
		log.Error("Search handler: failed to search database", zap.Error(err))
		http.Error(w, "Failed to search database", http.StatusInternalServerError)
		return
	}

	if entries == nil {
		entries = []database.UnifiedEntry{}
	}
	log.Info("Search handler: found entries", zap.Int("count", len(entries)))

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		log.Error("Search handler: failed to encode response", zap.Error(err))
	}
}

func (s *Server) logsHandler(w http.ResponseWriter, r *http.Request) {
	log := logger.FromCtx(r.Context())
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	namespace := r.URL.Query().Get("namespace")
	if namespace == "" {
		namespace = defaultNamespace
	}

	limitStr := r.URL.Query().Get("limit")
	limit := 50
	if limitStr != "" {
		if parsedLimit, err := strconv.Atoi(limitStr); err == nil && parsedLimit > 0 {
			limit = parsedLimit
		}
	}

	if s.dbClient == nil {
		http.Error(w, "Server not ready", http.StatusServiceUnavailable)
		return
	}

	logs, err := s.dbClient.GetRecentLogEntries(namespace, limit)
	if err != nil {
		log.Error("Failed to get logs from database", zap.Error(err))
		http.Error(w, "Failed to get logs", http.StatusInternalServerError)
		return
	}

	// Handle empty results gracefully
	if logs == nil {
		logs = []database.LogEntry{}
	}

	log.Info("Retrieved log entries", zap.Int("count", len(logs)), zap.String("namespace", namespace))

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(LogsResponse{
		Logs: logs,
	}); err != nil {
		log.Error("Failed to encode logs response", zap.Error(err))
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

//nolint:gocyclo,cyclop,funlen // dispatch per resource type requires many branches
func (s *Server) buildContextFromResults(results []database.SearchResult) string {
	if len(results) == 0 {
		return "No relevant logs or metrics found in the database."
	}

	var sb strings.Builder
	sb.WriteString("Here are some relevant logs, metrics, and events from your Kubernetes cluster:\n\n")
	ts := "2006-01-02 15:04:05"

	for i := range results {
		// Limit context to top 5 results with sufficient similarity
		if i >= 5 || results[i].Similarity < database.MinSimilarityThreshold {
			break
		}
		s.writeResultContext(&sb, &results[i], ts)
	}

	return sb.String()
}

//nolint:cyclop,gocyclo,funlen // dispatch per resource type requires many branches
func (s *Server) writeResultContext(sb *strings.Builder, result *database.SearchResult, ts string) {
	if result.LogEntry != nil {
		fmt.Fprintf(sb, "LOG ENTRY (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.LogEntry.Namespace)
		fmt.Fprintf(sb, "  Pod: %s\n", result.LogEntry.PodName)
		fmt.Fprintf(sb, "  Container: %s\n", result.LogEntry.Container)
		fmt.Fprintf(sb, "  Message: %s\n", result.LogEntry.Message)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.LogEntry.Timestamp.Format(ts))
	}

	if result.MetricsEntry != nil {
		fmt.Fprintf(sb, "METRICS ENTRY (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.MetricsEntry.Namespace)
		fmt.Fprintf(sb, "  Pod: %s\n", result.MetricsEntry.PodName)
		fmt.Fprintf(sb, "  Container: %s\n", result.MetricsEntry.Container)
		fmt.Fprintf(sb, "  CPU: %s\n", result.MetricsEntry.CPU)
		fmt.Fprintf(sb, "  Memory: %s\n", result.MetricsEntry.Memory)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.MetricsEntry.Timestamp.Format(ts))
	}

	if result.EventEntry != nil {
		fmt.Fprintf(sb, "EVENT (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.EventEntry.Namespace)
		fmt.Fprintf(sb, "  Type: %s\n", result.EventEntry.EventType)
		fmt.Fprintf(sb, "  Object: %s\n", result.EventEntry.InvolvedObject)
		fmt.Fprintf(sb, "  Reason: %s\n", result.EventEntry.Reason)
		fmt.Fprintf(sb, "  Message: %s\n", result.EventEntry.Message)
		fmt.Fprintf(sb, "  Source: %s\n", result.EventEntry.Source)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.EventEntry.Timestamp.Format(ts))
	}

	if result.PodStatusEntry != nil {
		fmt.Fprintf(sb, "POD STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.PodStatusEntry.Namespace)
		fmt.Fprintf(sb, "  Pod: %s\n", result.PodStatusEntry.PodName)
		fmt.Fprintf(sb, "  Phase: %s\n", result.PodStatusEntry.Phase)
		fmt.Fprintf(sb, "  Conditions: %s\n", result.PodStatusEntry.Conditions)
		fmt.Fprintf(sb, "  Containers: %s\n", result.PodStatusEntry.Containers)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.PodStatusEntry.Timestamp.Format(ts))
	}

	if result.NodeStatusEntry != nil {
		fmt.Fprintf(sb, "NODE STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Node: %s\n", result.NodeStatusEntry.NodeName)
		fmt.Fprintf(sb, "  Conditions: %s\n", result.NodeStatusEntry.Conditions)
		fmt.Fprintf(sb, "  Resources: %s\n", result.NodeStatusEntry.Resources)
		fmt.Fprintf(sb, "  NodeInfo: %s\n", result.NodeStatusEntry.NodeInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.NodeStatusEntry.Timestamp.Format(ts))
	}

	if result.DeploymentStatusEntry != nil {
		fmt.Fprintf(sb, "DEPLOYMENT STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.DeploymentStatusEntry.Namespace)
		fmt.Fprintf(sb, "  Deployment: %s\n", result.DeploymentStatusEntry.DeploymentName)
		fmt.Fprintf(sb, "  Conditions: %s\n", result.DeploymentStatusEntry.Conditions)
		fmt.Fprintf(sb, "  Replicas: %s\n", result.DeploymentStatusEntry.Replicas)
		fmt.Fprintf(sb, "  ChangeCause: %s\n", result.DeploymentStatusEntry.ChangeCause)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.DeploymentStatusEntry.Timestamp.Format(ts))
	}

	if result.HPAStatusEntry != nil {
		fmt.Fprintf(sb, "HPA STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.HPAStatusEntry.Namespace)
		fmt.Fprintf(sb, "  HPA: %s\n", result.HPAStatusEntry.HPAName)
		fmt.Fprintf(sb, "  Conditions: %s\n", result.HPAStatusEntry.Conditions)
		fmt.Fprintf(sb, "  Metrics: %s\n", result.HPAStatusEntry.Metrics)
		fmt.Fprintf(sb, "  ScalingInfo: %s\n", result.HPAStatusEntry.ScalingInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.HPAStatusEntry.Timestamp.Format(ts))
	}

	if result.EndpointHealthEntry != nil {
		fmt.Fprintf(sb, "ENDPOINT HEALTH (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.EndpointHealthEntry.Namespace)
		fmt.Fprintf(sb, "  Service: %s\n", result.EndpointHealthEntry.ServiceName)
		fmt.Fprintf(sb, "  HealthCounts: %s\n", result.EndpointHealthEntry.HealthCounts)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.EndpointHealthEntry.Timestamp.Format(ts))
	}

	if result.PVCStatusEntry != nil {
		fmt.Fprintf(sb, "PVC STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.PVCStatusEntry.Namespace)
		fmt.Fprintf(sb, "  PVC: %s\n", result.PVCStatusEntry.PVCName)
		fmt.Fprintf(sb, "  Phase: %s\n", result.PVCStatusEntry.Phase)
		fmt.Fprintf(sb, "  VolumeInfo: %s\n", result.PVCStatusEntry.VolumeInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.PVCStatusEntry.Timestamp.Format(ts))
	}

	if result.PVStatusEntry != nil {
		fmt.Fprintf(sb, "PV STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  PV: %s\n", result.PVStatusEntry.PVName)
		fmt.Fprintf(sb, "  Phase: %s\n", result.PVStatusEntry.Phase)
		fmt.Fprintf(sb, "  VolumeInfo: %s\n", result.PVStatusEntry.VolumeInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.PVStatusEntry.Timestamp.Format(ts))
	}

	if result.ConfigMapEntry != nil {
		fmt.Fprintf(sb, "CONFIGMAP CHANGE (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.ConfigMapEntry.Namespace)
		fmt.Fprintf(sb, "  Name: %s\n", result.ConfigMapEntry.Name)
		fmt.Fprintf(sb, "  Metadata: %s\n", result.ConfigMapEntry.Metadata)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.ConfigMapEntry.Timestamp.Format(ts))
	}

	if result.SecretEntry != nil {
		fmt.Fprintf(sb, "SECRET CHANGE (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.SecretEntry.Namespace)
		fmt.Fprintf(sb, "  Name: %s\n", result.SecretEntry.Name)
		fmt.Fprintf(sb, "  Type: %s\n", result.SecretEntry.SecretType)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.SecretEntry.Timestamp.Format(ts))
	}

	if result.JobStatusEntry != nil {
		fmt.Fprintf(sb, "JOB STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.JobStatusEntry.Namespace)
		fmt.Fprintf(sb, "  Job: %s\n", result.JobStatusEntry.JobName)
		fmt.Fprintf(sb, "  Conditions: %s\n", result.JobStatusEntry.Conditions)
		fmt.Fprintf(sb, "  StatusInfo: %s\n", result.JobStatusEntry.StatusInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.JobStatusEntry.Timestamp.Format(ts))
	}

	if result.CronJobStatusEntry != nil {
		fmt.Fprintf(sb, "CRONJOB STATUS (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.CronJobStatusEntry.Namespace)
		fmt.Fprintf(sb, "  CronJob: %s\n", result.CronJobStatusEntry.CronJobName)
		fmt.Fprintf(sb, "  ScheduleInfo: %s\n", result.CronJobStatusEntry.ScheduleInfo)
		fmt.Fprintf(sb, "  StatusInfo: %s\n", result.CronJobStatusEntry.StatusInfo)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.CronJobStatusEntry.Timestamp.Format(ts))
	}
	if result.TraceEntry != nil {
		fmt.Fprintf(sb, "TRACE (similarity: %.3f):\n", result.Similarity)
		fmt.Fprintf(sb, "  Namespace: %s\n", result.TraceEntry.Namespace)
		fmt.Fprintf(sb, "  Service: %s\n", result.TraceEntry.Service)
		fmt.Fprintf(sb, "  Operation: %s\n", result.TraceEntry.Operation)
		fmt.Fprintf(sb, "  Status: %s %s\n", result.TraceEntry.StatusCode, result.TraceEntry.StatusMsg)
		fmt.Fprintf(sb, "  Duration: %.2fms\n", result.TraceEntry.DurationMs)
		fmt.Fprintf(sb, "  TraceID: %s\n", result.TraceEntry.TraceID)
		fmt.Fprintf(sb, "  Summary: %s\n", result.TraceEntry.Summary)
		fmt.Fprintf(sb, "  Timestamp: %s\n\n", result.TraceEntry.Timestamp.Format(ts))
	}
}

func (s *Server) buildEnhancedMessages(originalMessages []llm.Message, context string) []llm.Message {
	// Add system message with context
	now := time.Now().UTC().Format(time.RFC3339)
	promptContent := strings.ReplaceAll(s.systemPromptTemplate, "{{current_utc_time}}", now)
	promptContent = strings.ReplaceAll(promptContent, "{{cluster_context}}", context)

	systemPrompt := llm.Message{
		Role:    "system",
		Content: promptContent,
	}

	// Build enhanced message list
	enhancedMessages := []llm.Message{systemPrompt}
	enhancedMessages = append(enhancedMessages, originalMessages...)

	return enhancedMessages
}

// extractTimeWindow uses the LLM to parse natural-language time references from a user query
// and returns SearchOptions with the resolved UTC time range.
func (s *Server) extractTimeWindow(ctx context.Context, userMessage string) database.SearchOptions {
	log := logger.FromCtx(ctx)
	now := time.Now().UTC()
	prompt := fmt.Sprintf("Current UTC time: %s\n\nUser message: %s", now.Format(time.RFC3339), userMessage)

	messages := []llm.Message{
		{Role: "system", Content: s.timeExtractionPrompt},
		{Role: "user", Content: prompt},
	}

	resp, err := s.llmClient.Chat(ctx, messages)
	if err != nil {
		log.Warn("Chat handler: failed to extract time window", zap.Error(err))
		return database.SearchOptions{}
	}

	return parseTimeWindowResponse(resp.Content)
}

// parseTimeWindowResponse parses the JSON response from the time extraction LLM call.
func parseTimeWindowResponse(content string) database.SearchOptions {
	content = strings.TrimSpace(content)

	var parsed struct {
		Start string `json:"start"`
		End   string `json:"end"`
	}
	if err := json.Unmarshal([]byte(content), &parsed); err != nil {
		logger.Logger.Warn("Chat handler: failed to parse time window JSON", zap.Error(err), zap.String("content", content))
		return database.SearchOptions{}
	}

	var opts database.SearchOptions
	if parsed.Start != "" {
		if t, err := time.Parse(time.RFC3339, parsed.Start); err == nil {
			opts.StartTime = t
		} else {
			logger.Logger.Warn("Chat handler: failed to parse start time", zap.String("start", parsed.Start), zap.Error(err))
		}
	}
	if parsed.End != "" {
		if t, err := time.Parse(time.RFC3339, parsed.End); err == nil {
			opts.EndTime = t
		} else {
			logger.Logger.Warn("Chat handler: failed to parse end time", zap.String("end", parsed.End), zap.Error(err))
		}
	}
	return opts
}

//nolint:cyclop,gocyclo // query parameter parsing
func (s *Server) resourcesHandler(w http.ResponseWriter, r *http.Request) {
	log := logger.FromCtx(r.Context())

	q := database.ResourceQuery{}

	if types := r.URL.Query().Get("types"); types != "" {
		q.ResourceTypes = strings.Split(types, ",")
	}
	q.Namespace = r.URL.Query().Get("namespace")
	q.ResourceName = r.URL.Query().Get("name")

	if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
		if l, err := strconv.Atoi(limitStr); err == nil && l > 0 {
			q.Limit = l
		}
	}
	if st := r.URL.Query().Get("start_time"); st != "" {
		if t, err := time.Parse(time.RFC3339, st); err == nil {
			q.StartTime = t
		}
	}
	if et := r.URL.Query().Get("end_time"); et != "" {
		if t, err := time.Parse(time.RFC3339, et); err == nil {
			q.EndTime = t
		}
	}

	// Parse data filters: df.key=value
	q.DataFilters = make(map[string]string)
	q.DataContains = make(map[string]string)
	for key, vals := range r.URL.Query() {
		if strings.HasPrefix(key, "df.") && len(vals) > 0 {
			q.DataFilters[strings.TrimPrefix(key, "df.")] = vals[0]
		}
		if strings.HasPrefix(key, "dc.") && len(vals) > 0 {
			q.DataContains[strings.TrimPrefix(key, "dc.")] = vals[0]
		}
	}

	entries, err := s.dbClient.QueryResources(&q)
	if err != nil {
		log.Error("Failed to query resources", zap.Error(err))
		http.Error(w, "Failed to query resources", http.StatusInternalServerError)
		return
	}

	if entries == nil {
		entries = []database.UnifiedEntry{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(entries); err != nil {
		log.Error("Failed to encode resources response", zap.Error(err))
	}
}

func (s *Server) statsHandler(w http.ResponseWriter, r *http.Request) {
	log := logger.FromCtx(r.Context())

	stats, err := s.dbClient.GetResourceStats()
	if err != nil {
		log.Error("Failed to get resource stats", zap.Error(err))
		http.Error(w, "Failed to get stats", http.StatusInternalServerError)
		return
	}

	if stats == nil {
		stats = []database.ResourceStat{}
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(stats); err != nil {
		log.Error("Failed to encode stats response", zap.Error(err))
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (s *Server) tracesHandler(w http.ResponseWriter, r *http.Request) {
	if s.tempoClient == nil {
		http.Error(w, `{"error":"Tempo integration not enabled"}`, http.StatusServiceUnavailable)
		return
	}

	params := &tempo.SearchParams{
		ServiceName: r.URL.Query().Get("service"),
		SpanName:    r.URL.Query().Get("operation"),
		MinDuration: r.URL.Query().Get("minDuration"),
		MaxDuration: r.URL.Query().Get("maxDuration"),
	}
	if l := r.URL.Query().Get("limit"); l != "" {
		if v, err := strconv.Atoi(l); err == nil {
			params.Limit = v
		}
	}
	if st := r.URL.Query().Get("start"); st != "" {
		if t, err := time.Parse(time.RFC3339, st); err == nil {
			params.Start = t.Unix()
		}
	}
	if et := r.URL.Query().Get("end"); et != "" {
		if t, err := time.Parse(time.RFC3339, et); err == nil {
			params.End = t.Unix()
		}
	}

	result, err := s.tempoClient.SearchTraces(r.Context(), params)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if encErr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); encErr != nil {
			log := logger.FromCtx(r.Context())
			log.Error("failed to encode error response", zap.Error(encErr))
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(result); err != nil {
		log := logger.FromCtx(r.Context())
		log.Error("failed to encode trace search response", zap.Error(err))
	}
}

func (s *Server) traceHandler(w http.ResponseWriter, r *http.Request) {
	if s.tempoClient == nil {
		http.Error(w, `{"error":"Tempo integration not enabled"}`, http.StatusServiceUnavailable)
		return
	}

	vars := mux.Vars(r)
	traceID := vars["traceID"]

	trace, err := s.tempoClient.GetTrace(r.Context(), traceID)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		if encErr := json.NewEncoder(w).Encode(map[string]string{"error": err.Error()}); encErr != nil {
			log := logger.FromCtx(r.Context())
			log.Error("failed to encode error response", zap.Error(encErr))
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	// #nosec G705 -- served as application/json, never interpreted as markup
	if _, err = w.Write(trace); err != nil {
		log := logger.FromCtx(r.Context())
		log.Error("failed to write trace response", zap.Error(err))
	}
}

// userInfoHandler returns information about the current authenticated user
func (s *Server) userInfoHandler(w http.ResponseWriter, r *http.Request) {
	log := logger.FromCtx(r.Context())
	if !s.authManager.IsEnabled() {
		http.Error(w, "Authentication is disabled", http.StatusServiceUnavailable)
		return
	}

	user := s.authManager.GetCurrentUser(r)
	if user == nil {
		http.Error(w, "Not authenticated", http.StatusUnauthorized)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(user); err != nil {
		log.Error("Failed to encode user response", zap.Error(err))
		http.Error(w, "Failed to encode response", http.StatusInternalServerError)
	}
}

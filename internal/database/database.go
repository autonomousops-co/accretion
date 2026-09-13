package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/lib/pq" // PostgreSQL driver
	"github.com/pgvector/pgvector-go"
	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/logger"
)

// Interface defines the database operations contract.
type Interface interface {
	// InsertEntry inserts a unified entry into the database.
	// If ContentHash is non-empty and a duplicate exists, it will be ignored (ON CONFLICT DO NOTHING).
	// If ContentHash is empty (e.g. metrics), it always inserts.
	InsertEntry(entry *UnifiedEntry) error

	// SearchEntries performs vector similarity search across unified_entries with optional time filters.
	SearchEntries(queryEmbedding pgvector.Vector, limit int, opts SearchOptions) ([]UnifiedEntry, error)

	// QueryResources retrieves entries by resource type with structured filters.
	QueryResources(query *ResourceQuery) ([]UnifiedEntry, error)

	// GetResourceStats returns entry counts grouped by resource type and namespace.
	GetResourceStats() ([]ResourceStat, error)

	// Legacy compatibility

	// GetRecentLogEntries retrieves the most recent log entries for a given namespace.
	GetRecentLogEntries(namespace string, limit int) ([]LogEntry, error)

	// SearchSimilar performs vector similarity search.
	SearchSimilar(queryEmbedding pgvector.Vector, limit int) ([]SearchResult, error)

	// SearchSimilarWithOptions performs vector similarity search with optional time range filtering.
	SearchSimilarWithOptions(queryEmbedding pgvector.Vector, limit int, opts SearchOptions) ([]SearchResult, error)

	// GetLogStats returns statistics about stored entries.
	GetLogStats() (map[string]int, error)

	// Close closes the database connection.
	Close() error
}

// errNoRows is the error string returned by sql.QueryRow when no rows match.
const errNoRows = "sql: no rows in result set"

// Resource type constants for the unified_entries table.
const (
	ResourceTypeLog              = "log"
	ResourceTypeMetrics          = "metrics"
	ResourceTypeEvent            = "event"
	ResourceTypePodStatus        = "pod_status"
	ResourceTypeNodeStatus       = "node_status"
	ResourceTypeDeploymentStatus = "deployment_status"
	ResourceTypeHPAStatus        = "hpa_status"
	ResourceTypeEndpointHealth   = "endpoint_health"
	ResourceTypePVCStatus        = "pvc_status"
	ResourceTypePVStatus         = "pv_status"
	ResourceTypeConfigMap        = "configmap"
	ResourceTypeSecret           = "secret"
	ResourceTypeJobStatus        = "job_status"
	ResourceTypeCronJobStatus    = "cronjob_status"
	ResourceTypeTrace            = "trace"
)

// MinSimilarityThreshold is the minimum cosine similarity score for search results.
const MinSimilarityThreshold = 0.3

// UnifiedEntry represents a single row in the unified_entries table.
type UnifiedEntry struct {
	Timestamp    time.Time       `json:"timestamp"`
	CreatedAt    time.Time       `json:"created_at"`
	ResourceType string          `json:"resource_type"`
	Namespace    string          `json:"namespace"`
	ResourceName string          `json:"resource_name"`
	ContentHash  string          `json:"content_hash"`
	Embedding    pgvector.Vector `json:"-"`
	Data         json.RawMessage `json:"data"`
	Similarity   float64         `json:"similarity,omitempty"`
	ID           int             `json:"id"`
}

// SearchOptions configures optional filters for similarity search.
type SearchOptions struct {
	StartTime time.Time
	EndTime   time.Time
}

// ResourceQuery defines structured filters for QueryResources.
type ResourceQuery struct {
	StartTime     time.Time
	EndTime       time.Time
	DataFilters   map[string]string
	DataContains  map[string]string
	Namespace     string
	ResourceName  string
	ResourceTypes []string
	Limit         int
}

// ResourceStat holds an entry count for a resource type + namespace pair.
type ResourceStat struct {
	ResourceType string `json:"resource_type"`
	Namespace    string `json:"namespace"`
	Count        int    `json:"count"`
}

// Legacy entry structs kept for internal conversion in workers.

// LogEntry represents a log entry (internal conversion type).
type LogEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	PodName     string          `json:"pod_name"`
	Container   string          `json:"container"`
	Message     string          `json:"message"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// MetricsEntry represents a metrics entry (internal conversion type).
type MetricsEntry struct {
	Timestamp time.Time       `json:"timestamp"`
	CreatedAt time.Time       `json:"created_at"`
	Namespace string          `json:"namespace"`
	PodName   string          `json:"pod_name"`
	Container string          `json:"container"`
	CPU       string          `json:"cpu"`
	Memory    string          `json:"memory"`
	Embedding pgvector.Vector `json:"-"`
	ID        int             `json:"id"`
}

// EventEntry represents an event entry (internal conversion type).
type EventEntry struct {
	Timestamp      time.Time       `json:"timestamp"`
	CreatedAt      time.Time       `json:"created_at"`
	Namespace      string          `json:"namespace"`
	Name           string          `json:"name"`
	Reason         string          `json:"reason"`
	Message        string          `json:"message"`
	InvolvedObject string          `json:"involved_object"`
	Kind           string          `json:"kind"`
	Source         string          `json:"source"`
	EventType      string          `json:"event_type"`
	ContentHash    string          `json:"content_hash"`
	Embedding      pgvector.Vector `json:"-"`
	ID             int             `json:"id"`
	Count          int32           `json:"count"`
}

// TraceEntry represents a trace (span) entry ingested into RAG (internal
// conversion type). Fields mirror the data blob written by the trace worker.
type TraceEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	Service     string          `json:"service"`
	Operation   string          `json:"operation"`
	TraceID     string          `json:"trace_id"`
	SpanID      string          `json:"span_id"`
	Kind        string          `json:"kind"`
	StatusCode  string          `json:"status_code"`
	StatusMsg   string          `json:"status_msg"`
	Summary     string          `json:"summary"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	DurationMs  float64         `json:"duration_ms"`
	ID          int             `json:"id"`
}

// PodStatusEntry represents a pod status entry (internal conversion type).
type PodStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	PodName     string          `json:"pod_name"`
	Phase       string          `json:"phase"`
	Conditions  string          `json:"conditions"`
	Containers  string          `json:"containers"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// NodeStatusEntry represents a node status entry (internal conversion type).
type NodeStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	NodeName    string          `json:"node_name"`
	Conditions  string          `json:"conditions"`
	Resources   string          `json:"resources"`
	NodeInfo    string          `json:"node_info"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// DeploymentStatusEntry represents a deployment status entry (internal conversion type).
type DeploymentStatusEntry struct {
	Timestamp      time.Time       `json:"timestamp"`
	CreatedAt      time.Time       `json:"created_at"`
	Namespace      string          `json:"namespace"`
	DeploymentName string          `json:"deployment_name"`
	Conditions     string          `json:"conditions"`
	Replicas       string          `json:"replicas"`
	ReplicaSets    string          `json:"replica_sets"`
	ChangeCause    string          `json:"change_cause"`
	ContentHash    string          `json:"content_hash"`
	Embedding      pgvector.Vector `json:"-"`
	ID             int             `json:"id"`
}

// HPAStatusEntry represents an HPA status entry (internal conversion type).
type HPAStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	HPAName     string          `json:"hpa_name"`
	Conditions  string          `json:"conditions"`
	Metrics     string          `json:"metrics"`
	ScalingInfo string          `json:"scaling_info"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// EndpointHealthEntry represents an endpoint health entry (internal conversion type).
type EndpointHealthEntry struct {
	Timestamp    time.Time       `json:"timestamp"`
	CreatedAt    time.Time       `json:"created_at"`
	Namespace    string          `json:"namespace"`
	ServiceName  string          `json:"service_name"`
	Endpoints    string          `json:"endpoints"`
	HealthCounts string          `json:"health_counts"`
	ContentHash  string          `json:"content_hash"`
	Embedding    pgvector.Vector `json:"-"`
	ID           int             `json:"id"`
}

// PVCStatusEntry represents a PVC status entry (internal conversion type).
type PVCStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	PVCName     string          `json:"pvc_name"`
	Phase       string          `json:"phase"`
	Conditions  string          `json:"conditions"`
	VolumeInfo  string          `json:"volume_info"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// PVStatusEntry represents a PV status entry (internal conversion type).
type PVStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	PVName      string          `json:"pv_name"`
	Phase       string          `json:"phase"`
	VolumeInfo  string          `json:"volume_info"`
	StatusInfo  string          `json:"status_info"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// ConfigMapEntry stores metadata about a ConfigMap change (internal conversion type).
type ConfigMapEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	Name        string          `json:"name"`
	Metadata    string          `json:"metadata"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// SecretEntry stores metadata about a Secret change (internal conversion type).
//
// READ PATH ONLY. Accretion no longer collects Kubernetes Secrets and no chart
// role grants any verb on them; nothing writes a `secret` row any more. This
// type, populateSecretResult and the renderer in internal/server/handlers.go
// exist so rows written before that change stay queryable instead of silently
// rendering blank. Do not reintroduce a writer.
type SecretEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	Name        string          `json:"name"`
	SecretType  string          `json:"secret_type"`
	Metadata    string          `json:"metadata"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// JobStatusEntry stores status information about a Job (internal conversion type).
type JobStatusEntry struct {
	Timestamp   time.Time       `json:"timestamp"`
	CreatedAt   time.Time       `json:"created_at"`
	Namespace   string          `json:"namespace"`
	JobName     string          `json:"job_name"`
	Conditions  string          `json:"conditions"`
	StatusInfo  string          `json:"status_info"`
	ContentHash string          `json:"content_hash"`
	Embedding   pgvector.Vector `json:"-"`
	ID          int             `json:"id"`
}

// CronJobStatusEntry stores status information about a CronJob (internal conversion type).
type CronJobStatusEntry struct {
	Timestamp    time.Time       `json:"timestamp"`
	CreatedAt    time.Time       `json:"created_at"`
	Namespace    string          `json:"namespace"`
	CronJobName  string          `json:"cronjob_name"`
	ScheduleInfo string          `json:"schedule_info"`
	StatusInfo   string          `json:"status_info"`
	ContentHash  string          `json:"content_hash"`
	Embedding    pgvector.Vector `json:"-"`
	ID           int             `json:"id"`
}

// SearchResult holds a search result with the entry data populated per resource type.
type SearchResult struct {
	LogEntry              *LogEntry              `json:"log_entry,omitempty"`
	MetricsEntry          *MetricsEntry          `json:"metrics_entry,omitempty"`
	EventEntry            *EventEntry            `json:"event_entry,omitempty"`
	PodStatusEntry        *PodStatusEntry        `json:"pod_status_entry,omitempty"`
	NodeStatusEntry       *NodeStatusEntry       `json:"node_status_entry,omitempty"`
	DeploymentStatusEntry *DeploymentStatusEntry `json:"deployment_status_entry,omitempty"`
	HPAStatusEntry        *HPAStatusEntry        `json:"hpa_status_entry,omitempty"`
	EndpointHealthEntry   *EndpointHealthEntry   `json:"endpoint_health_entry,omitempty"`
	PVCStatusEntry        *PVCStatusEntry        `json:"pvc_status_entry,omitempty"`
	PVStatusEntry         *PVStatusEntry         `json:"pv_status_entry,omitempty"`
	ConfigMapEntry        *ConfigMapEntry        `json:"configmap_entry,omitempty"`
	SecretEntry           *SecretEntry           `json:"secret_entry,omitempty"`
	JobStatusEntry        *JobStatusEntry        `json:"job_status_entry,omitempty"`
	CronJobStatusEntry    *CronJobStatusEntry    `json:"cronjob_status_entry,omitempty"`
	TraceEntry            *TraceEntry            `json:"trace_entry,omitempty"`
	Similarity            float64                `json:"similarity"`
}

// Client implements the Interface for PostgreSQL database operations.
type Client struct {
	db *sql.DB
}

// Compile-time verification that Client implements Interface
var _ Interface = (*Client)(nil)

// NewClient creates a new database client that implements the Interface
func NewClient(cfg *config.DatabaseConfig) (Interface, error) {
	return newPostgreSQLClient(cfg)
}

// waitForDB pings the database with exponential backoff until it succeeds or retries are exhausted.
func waitForDB(db *sql.DB, label string, maxRetries int) error {
	baseDelay := 1 * time.Second
	maxDelay := 30 * time.Second

	for attempt := range maxRetries {
		if err := db.Ping(); err == nil {
			if attempt > 0 {
				logger.Logger.Info("Database connection successful", zap.String("label", label), zap.Int("attempts", attempt+1))
			}
			return nil
		} else if attempt == maxRetries-1 {
			return fmt.Errorf("%s connection failed after %d attempts: %w", label, maxRetries, err)
		} else {
			delay := min(time.Duration(attempt+1)*baseDelay, maxDelay)
			logger.Logger.Warn("Database connection attempt failed",
				zap.String("label", label), zap.Int("attempt", attempt+1), zap.Int("max_attempts", maxRetries),
				zap.Error(err), zap.Duration("retry_in", delay))
			time.Sleep(delay)
		}
	}
	return nil
}

// newPostgreSQLClient creates a new PostgreSQL client implementation.
func newPostgreSQLClient(cfg *config.DatabaseConfig) (*Client, error) {
	connectionString := fmt.Sprintf("host=%s port=%d user=%s password=%s dbname=%s sslmode=%s connect_timeout=10",
		cfg.Host, cfg.Port, cfg.User, cfg.Password, cfg.Database, cfg.SSLMode)

	db, err := sql.Open("postgres", connectionString)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := waitForDB(db, "Database", 10); err != nil {
		db.Close()
		return nil, err
	}

	return &Client{db: db}, nil
}

func (c *Client) InsertEntry(entry *UnifiedEntry) error {
	if entry.ContentHash != "" {
		// Deduplicated insert
		query := `
			INSERT INTO unified_entries (resource_type, namespace, resource_name, data, timestamp, embedding, content_hash)
			VALUES ($1, $2, $3, $4, $5, $6, $7)
			ON CONFLICT (content_hash, timestamp) DO NOTHING
			RETURNING id, created_at
		`
		err := c.db.QueryRow(query,
			entry.ResourceType,
			entry.Namespace,
			entry.ResourceName,
			entry.Data,
			entry.Timestamp,
			entry.Embedding,
			entry.ContentHash,
		).Scan(&entry.ID, &entry.CreatedAt)

		if err != nil {
			if errors.Is(err, sql.ErrNoRows) || err.Error() == errNoRows {
				logger.Logger.Debug("Duplicate entry detected",
					zap.String("resource_type", entry.ResourceType),
					zap.String("hash", entry.ContentHash[:min(8, len(entry.ContentHash))]))
				return nil
			}
			return fmt.Errorf("failed to insert %s entry: %w", entry.ResourceType, err)
		}
	} else {
		// Metrics-style insert (no content_hash, allow duplicates)
		query := `
			INSERT INTO unified_entries (resource_type, namespace, resource_name, data, timestamp, embedding)
			VALUES ($1, $2, $3, $4, $5, $6)
			RETURNING id, created_at
		`
		err := c.db.QueryRow(query,
			entry.ResourceType,
			entry.Namespace,
			entry.ResourceName,
			entry.Data,
			entry.Timestamp,
			entry.Embedding,
		).Scan(&entry.ID, &entry.CreatedAt)

		if err != nil {
			return fmt.Errorf("failed to insert %s entry: %w", entry.ResourceType, err)
		}
	}

	logger.Logger.Debug("Inserted new entry",
		zap.String("resource_type", entry.ResourceType),
		zap.String("resource_name", entry.ResourceName), zap.String("namespace", entry.Namespace))
	return nil
}

func (c *Client) SearchEntries(queryEmbedding pgvector.Vector, limit int, opts SearchOptions) ([]UnifiedEntry, error) {
	q := `
		SELECT id, resource_type, namespace, resource_name, data,
		       timestamp, embedding, created_at, content_hash,
		       1 - (embedding <=> $1) as similarity
		FROM unified_entries
		WHERE embedding IS NOT NULL`
	q += fmt.Sprintf(" AND 1 - (embedding <=> $1) >= %g", MinSimilarityThreshold)
	args := []interface{}{queryEmbedding}
	argIdx := 2

	q, args, argIdx = appendTimeFilters(q, args, argIdx, opts)

	// #nosec G202 -- argIdx is an internal counter forming a placeholder
	// position; the limit value itself is bound as a parameter below.
	q += fmt.Sprintf(" ORDER BY embedding <=> $1 LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := c.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to search entries: %w", err)
	}
	defer rows.Close()

	var results []UnifiedEntry
	for rows.Next() {
		var entry UnifiedEntry
		var contentHash sql.NullString
		if scanErr := rows.Scan(
			&entry.ID, &entry.ResourceType, &entry.Namespace, &entry.ResourceName,
			&entry.Data, &entry.Timestamp, &entry.Embedding, &entry.CreatedAt,
			&contentHash, &entry.Similarity,
		); scanErr != nil {
			return nil, fmt.Errorf("failed to scan entry: %w", scanErr)
		}
		if contentHash.Valid {
			entry.ContentHash = contentHash.String
		}
		results = append(results, entry)
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, fmt.Errorf("row iteration error: %w", rowErr)
	}
	return results, nil
}

// SearchSimilar performs vector similarity search (legacy compatibility).
func (c *Client) SearchSimilar(queryEmbedding pgvector.Vector, limit int) ([]SearchResult, error) {
	return c.SearchSimilarWithOptions(queryEmbedding, limit, SearchOptions{})
}

// SearchSimilarWithOptions performs vector similarity search with optional time filtering.
// It searches the unified_entries table and converts results to SearchResult structs.
func (c *Client) SearchSimilarWithOptions(
	queryEmbedding pgvector.Vector, limit int, opts SearchOptions,
) ([]SearchResult, error) {
	entries, err := c.SearchEntries(queryEmbedding, limit, opts)
	if err != nil {
		return nil, err
	}

	return unifiedEntriesToSearchResults(entries), nil
}

// GetRecentLogEntries retrieves the most recent log entries for a given namespace.
func (c *Client) GetRecentLogEntries(namespace string, limit int) ([]LogEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Check if unified_entries table exists
	var tableExists bool
	checkQuery := "SELECT EXISTS (SELECT FROM information_schema.tables WHERE table_name = 'unified_entries')"
	err := c.db.QueryRowContext(ctx, checkQuery).Scan(&tableExists)
	if err != nil {
		return nil, fmt.Errorf("failed to check if unified_entries table exists: %w", err)
	}

	if !tableExists {
		return []LogEntry{}, nil
	}

	query := `
		SELECT id, namespace, resource_name, data, timestamp, embedding, created_at, content_hash
		FROM unified_entries
		WHERE resource_type = 'log' AND namespace = $1
		ORDER BY timestamp DESC
		LIMIT $2
	`

	rows, err := c.db.QueryContext(ctx, query, namespace, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to get recent log entries: %w", err)
	}
	defer rows.Close()

	var entries []LogEntry
	for rows.Next() {
		var id int
		var ns, resourceName string
		var data json.RawMessage
		var ts, createdAt time.Time
		var embedding pgvector.Vector
		var contentHash sql.NullString
		if scanErr := rows.Scan(&id, &ns, &resourceName, &data, &ts, &embedding, &createdAt, &contentHash); scanErr != nil {
			return nil, fmt.Errorf("failed to scan log entry: %w", scanErr)
		}
		entry := LogEntry{
			ID:        id,
			Namespace: ns,
			PodName:   resourceName,
			Timestamp: ts,
			Embedding: embedding,
			CreatedAt: createdAt,
		}
		if contentHash.Valid {
			entry.ContentHash = contentHash.String
		}
		// Parse data JSONB for container and message
		var logData struct {
			Container string `json:"container"`
			Message   string `json:"message"`
		}
		if jsonErr := json.Unmarshal(data, &logData); jsonErr == nil {
			entry.Container = logData.Container
			entry.Message = logData.Message
		}
		entries = append(entries, entry)
	}

	return entries, nil
}

// GetLogStats returns statistics about stored entries.
func (c *Client) GetLogStats() (map[string]int, error) {
	stats := make(map[string]int)

	var totalEntries int
	err := c.db.QueryRow("SELECT COUNT(*) FROM unified_entries WHERE resource_type = 'log'").Scan(&totalEntries)
	if err != nil {
		return nil, fmt.Errorf("failed to get total entries: %w", err)
	}
	stats["total_entries"] = totalEntries

	var entriesWithEmbeddings int
	err = c.db.QueryRow(
		"SELECT COUNT(*) FROM unified_entries WHERE resource_type = 'log' AND embedding IS NOT NULL",
	).Scan(&entriesWithEmbeddings)
	if err != nil {
		return nil, fmt.Errorf("failed to get entries with embeddings: %w", err)
	}
	stats["entries_with_embeddings"] = entriesWithEmbeddings

	return stats, nil
}

//nolint:cyclop,gocyclo,gocritic // query builder with many optional filters
func (c *Client) QueryResources(query *ResourceQuery) ([]UnifiedEntry, error) {
	q := `SELECT id, resource_type, namespace, resource_name, data, timestamp, created_at, content_hash
		FROM unified_entries WHERE 1=1`
	args := make([]interface{}, 0, len(query.ResourceTypes)+6) //nolint:prealloc // size not predictable
	argIdx := 1

	if len(query.ResourceTypes) > 0 {
		placeholders := make([]string, len(query.ResourceTypes))
		for i, rt := range query.ResourceTypes {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, rt)
			argIdx++
		}
		q += " AND resource_type IN (" + strings.Join(placeholders, ",") + ")"
	}
	if query.Namespace != "" {
		q += fmt.Sprintf(" AND namespace = $%d", argIdx)
		args = append(args, query.Namespace)
		argIdx++
	}
	if query.ResourceName != "" {
		q += fmt.Sprintf(" AND resource_name ILIKE $%d", argIdx)
		args = append(args, "%"+query.ResourceName+"%")
		argIdx++
	}
	if !query.StartTime.IsZero() {
		q += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, query.StartTime)
		argIdx++
	}
	if !query.EndTime.IsZero() {
		q += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, query.EndTime)
		argIdx++
	}
	for key, val := range query.DataFilters {
		q += fmt.Sprintf(" AND data->>'%s' = $%d", key, argIdx)
		args = append(args, val)
		argIdx++
	}
	for key, val := range query.DataContains {
		q += fmt.Sprintf(" AND data->>'%s' ILIKE $%d", key, argIdx)
		args = append(args, "%"+val+"%")
		argIdx++
	}

	limit := query.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	// #nosec G202 -- argIdx is an internal counter forming a placeholder
	// position; the limit value itself is bound as a parameter below.
	q += fmt.Sprintf(" ORDER BY timestamp DESC LIMIT $%d", argIdx)
	args = append(args, limit)

	rows, err := c.db.Query(q, args...)
	if err != nil {
		return nil, fmt.Errorf("failed to query resources: %w", err)
	}
	defer rows.Close()

	var results []UnifiedEntry
	for rows.Next() {
		var entry UnifiedEntry
		var contentHash sql.NullString
		if scanErr := rows.Scan(
			&entry.ID, &entry.ResourceType, &entry.Namespace, &entry.ResourceName,
			&entry.Data, &entry.Timestamp, &entry.CreatedAt, &contentHash,
		); scanErr != nil {
			return nil, fmt.Errorf("failed to scan resource entry: %w", scanErr)
		}
		if contentHash.Valid {
			entry.ContentHash = contentHash.String
		}
		results = append(results, entry)
	}
	if rowErr := rows.Err(); rowErr != nil {
		return nil, fmt.Errorf("row iteration error: %w", rowErr)
	}
	return results, nil
}

func (c *Client) GetResourceStats() ([]ResourceStat, error) {
	rows, err := c.db.Query(`
		SELECT resource_type, COALESCE(namespace, ''), COUNT(*)
		FROM unified_entries
		GROUP BY resource_type, namespace
		ORDER BY resource_type, namespace`)
	if err != nil {
		return nil, fmt.Errorf("failed to get resource stats: %w", err)
	}
	defer rows.Close()

	var stats []ResourceStat
	for rows.Next() {
		var s ResourceStat
		if scanErr := rows.Scan(&s.ResourceType, &s.Namespace, &s.Count); scanErr != nil {
			return nil, fmt.Errorf("failed to scan resource stat: %w", scanErr)
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

func (c *Client) Close() error {
	return c.db.Close()
}

// appendTimeFilters adds optional time range WHERE clauses.
func appendTimeFilters(
	query string, args []interface{}, argIdx int, opts SearchOptions,
) (string, []interface{}, int) {
	if !opts.StartTime.IsZero() {
		query += fmt.Sprintf(" AND timestamp >= $%d", argIdx)
		args = append(args, opts.StartTime)
		argIdx++
	}
	if !opts.EndTime.IsZero() {
		query += fmt.Sprintf(" AND timestamp <= $%d", argIdx)
		args = append(args, opts.EndTime)
		argIdx++
	}
	return query, args, argIdx
}

// unifiedEntriesToSearchResults converts UnifiedEntry results to legacy SearchResult format.
func unifiedEntriesToSearchResults(entries []UnifiedEntry) []SearchResult {
	results := make([]SearchResult, 0, len(entries))
	for i := range entries {
		result := SearchResult{Similarity: entries[i].Similarity}
		populateSearchResult(&result, &entries[i])
		results = append(results, result)
	}
	return results
}

// unmarshalData is a helper that unmarshals entry data, logging on failure.
func unmarshalData(data json.RawMessage, dest interface{}) {
	if err := json.Unmarshal(data, dest); err != nil {
		logger.Logger.Warn("Failed to unmarshal entry data", zap.Error(err))
	}
}

// populateSearchResult populates the appropriate legacy entry field based on resource_type.
//
//nolint:cyclop,gocyclo,funlen // dispatch by resource_type requires many branches
func populateSearchResult(result *SearchResult, entry *UnifiedEntry) {
	switch entry.ResourceType {
	case ResourceTypeLog:
		populateLogResult(result, entry)
	case ResourceTypeMetrics:
		populateMetricsResult(result, entry)
	case ResourceTypeEvent:
		populateEventResult(result, entry)
	case ResourceTypePodStatus:
		populatePodStatusResult(result, entry)
	case ResourceTypeNodeStatus:
		populateNodeStatusResult(result, entry)
	case ResourceTypeDeploymentStatus:
		populateDeploymentStatusResult(result, entry)
	case ResourceTypeHPAStatus:
		populateHPAStatusResult(result, entry)
	case ResourceTypeEndpointHealth:
		populateEndpointHealthResult(result, entry)
	case ResourceTypePVCStatus:
		populatePVCStatusResult(result, entry)
	case ResourceTypePVStatus:
		populatePVStatusResult(result, entry)
	case ResourceTypeConfigMap:
		populateConfigMapResult(result, entry)
	case ResourceTypeSecret:
		populateSecretResult(result, entry)
	case ResourceTypeJobStatus:
		populateJobStatusResult(result, entry)
	case ResourceTypeCronJobStatus:
		populateCronJobStatusResult(result, entry)
	case ResourceTypeTrace:
		populateTraceResult(result, entry)
	}
}

func populateLogResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Container string `json:"container"`
		Message   string `json:"message"`
	}
	unmarshalData(entry.Data, &d)
	result.LogEntry = &LogEntry{
		ID: entry.ID, Namespace: entry.Namespace, PodName: entry.ResourceName,
		Container: d.Container, Message: d.Message, Timestamp: entry.Timestamp,
		Embedding: entry.Embedding, CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateMetricsResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Container string `json:"container"`
		CPU       string `json:"cpu"`
		Memory    string `json:"memory"`
	}
	unmarshalData(entry.Data, &d)
	result.MetricsEntry = &MetricsEntry{
		ID: entry.ID, Namespace: entry.Namespace, PodName: entry.ResourceName,
		Container: d.Container, CPU: d.CPU, Memory: d.Memory,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding, CreatedAt: entry.CreatedAt,
	}
}

func populateEventResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Name           string `json:"name"`
		Reason         string `json:"reason"`
		Message        string `json:"message"`
		InvolvedObject string `json:"involved_object"`
		Kind           string `json:"kind"`
		Source         string `json:"source"`
		EventType      string `json:"event_type"`
		Count          int32  `json:"count"`
	}
	unmarshalData(entry.Data, &d)
	result.EventEntry = &EventEntry{
		ID: entry.ID, Namespace: entry.Namespace, Name: d.Name,
		Reason: d.Reason, Message: d.Message, InvolvedObject: d.InvolvedObject,
		Kind: d.Kind, Source: d.Source, EventType: d.EventType, Count: d.Count,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateTraceResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		TraceID    string  `json:"trace_id"`
		SpanID     string  `json:"span_id"`
		Service    string  `json:"service"`
		Operation  string  `json:"operation"`
		Kind       string  `json:"kind"`
		StatusCode string  `json:"status_code"`
		StatusMsg  string  `json:"status_msg"`
		Summary    string  `json:"summary"`
		DurationMs float64 `json:"duration_ms"`
	}
	unmarshalData(entry.Data, &d)
	result.TraceEntry = &TraceEntry{
		ID: entry.ID, Namespace: entry.Namespace,
		Service: d.Service, Operation: d.Operation,
		TraceID: d.TraceID, SpanID: d.SpanID, Kind: d.Kind,
		StatusCode: d.StatusCode, StatusMsg: d.StatusMsg,
		DurationMs: d.DurationMs, Summary: d.Summary,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populatePodStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Phase      string `json:"phase"`
		Conditions string `json:"conditions"`
		Containers string `json:"containers"`
	}
	unmarshalData(entry.Data, &d)
	result.PodStatusEntry = &PodStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, PodName: entry.ResourceName,
		Phase: d.Phase, Conditions: d.Conditions, Containers: d.Containers,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateNodeStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Conditions string `json:"conditions"`
		Resources  string `json:"resources"`
		NodeInfo   string `json:"node_info"`
	}
	unmarshalData(entry.Data, &d)
	result.NodeStatusEntry = &NodeStatusEntry{
		ID: entry.ID, NodeName: entry.ResourceName,
		Conditions: d.Conditions, Resources: d.Resources, NodeInfo: d.NodeInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateDeploymentStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Conditions  string `json:"conditions"`
		Replicas    string `json:"replicas"`
		ReplicaSets string `json:"replica_sets"`
		ChangeCause string `json:"change_cause"`
	}
	unmarshalData(entry.Data, &d)
	result.DeploymentStatusEntry = &DeploymentStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, DeploymentName: entry.ResourceName,
		Conditions: d.Conditions, Replicas: d.Replicas,
		ReplicaSets: d.ReplicaSets, ChangeCause: d.ChangeCause,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateHPAStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Conditions  string `json:"conditions"`
		Metrics     string `json:"metrics"`
		ScalingInfo string `json:"scaling_info"`
	}
	unmarshalData(entry.Data, &d)
	result.HPAStatusEntry = &HPAStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, HPAName: entry.ResourceName,
		Conditions: d.Conditions, Metrics: d.Metrics, ScalingInfo: d.ScalingInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateEndpointHealthResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Endpoints    string `json:"endpoints"`
		HealthCounts string `json:"health_counts"`
	}
	unmarshalData(entry.Data, &d)
	result.EndpointHealthEntry = &EndpointHealthEntry{
		ID: entry.ID, Namespace: entry.Namespace, ServiceName: entry.ResourceName,
		Endpoints: d.Endpoints, HealthCounts: d.HealthCounts,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populatePVCStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Phase      string `json:"phase"`
		Conditions string `json:"conditions"`
		VolumeInfo string `json:"volume_info"`
	}
	unmarshalData(entry.Data, &d)
	result.PVCStatusEntry = &PVCStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, PVCName: entry.ResourceName,
		Phase: d.Phase, Conditions: d.Conditions, VolumeInfo: d.VolumeInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populatePVStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Phase      string `json:"phase"`
		VolumeInfo string `json:"volume_info"`
		StatusInfo string `json:"status_info"`
	}
	unmarshalData(entry.Data, &d)
	result.PVStatusEntry = &PVStatusEntry{
		ID: entry.ID, PVName: entry.ResourceName,
		Phase: d.Phase, VolumeInfo: d.VolumeInfo, StatusInfo: d.StatusInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateConfigMapResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Metadata string `json:"metadata"`
	}
	unmarshalData(entry.Data, &d)
	result.ConfigMapEntry = &ConfigMapEntry{
		ID: entry.ID, Namespace: entry.Namespace, Name: entry.ResourceName,
		Metadata:  d.Metadata,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateSecretResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		SecretType string `json:"secret_type"`
		Metadata   string `json:"metadata"`
	}
	unmarshalData(entry.Data, &d)
	result.SecretEntry = &SecretEntry{
		ID: entry.ID, Namespace: entry.Namespace, Name: entry.ResourceName,
		SecretType: d.SecretType, Metadata: d.Metadata,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateJobStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		Conditions string `json:"conditions"`
		StatusInfo string `json:"status_info"`
	}
	unmarshalData(entry.Data, &d)
	result.JobStatusEntry = &JobStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, JobName: entry.ResourceName,
		Conditions: d.Conditions, StatusInfo: d.StatusInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

func populateCronJobStatusResult(result *SearchResult, entry *UnifiedEntry) {
	var d struct {
		ScheduleInfo string `json:"schedule_info"`
		StatusInfo   string `json:"status_info"`
	}
	unmarshalData(entry.Data, &d)
	result.CronJobStatusEntry = &CronJobStatusEntry{
		ID: entry.ID, Namespace: entry.Namespace, CronJobName: entry.ResourceName,
		ScheduleInfo: d.ScheduleInfo, StatusInfo: d.StatusInfo,
		Timestamp: entry.Timestamp, Embedding: entry.Embedding,
		CreatedAt: entry.CreatedAt, ContentHash: entry.ContentHash,
	}
}

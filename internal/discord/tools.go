package discord

import (
	"context"
	"encoding/json"
	"fmt"

	"go.uber.org/zap"

	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/logger"
	apiclient "github.com/autonomousops-co/accretion/internal/mcp/accretion"
)

// maxToolIterations prevents runaway tool-use loops.
const maxToolIterations = 10

//nolint:funlen // tool definitions are long but flat
func clusterTools() []llm.Tool {
	return []llm.Tool{
		{
			Name: "search_cluster",
			Description: "Semantic search across all cluster telemetry " +
				"(logs, metrics, events, pods, nodes, deployments, " +
				"HPAs, PVCs, jobs, cronjobs, configmaps, secrets).",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"query": {
						"type": "string",
						"description": "Natural language search query describing what you're looking for"
					},
					"start_time": {
						"type": "string",
						"description": "Optional RFC3339 start time to filter results (e.g. 2026-03-06T00:00:00Z)"
					},
					"end_time": {
						"type": "string",
						"description": "Optional RFC3339 end time to filter results"
					},
					"limit": {
						"type": "integer",
						"description": "Maximum number of results to return (default: 5, max: 20)"
					}
				},
				"required": ["query"]
			}`),
		},
		{
			Name: "get_recent_logs",
			Description: "Retrieve recent log entries for a " +
				"specific namespace, ordered by time.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {
						"type": "string",
						"description": "Kubernetes namespace to query"
					},
					"limit": {
						"type": "integer",
						"description": "Maximum number of log entries to return (default: 20, max: 100)"
					}
				},
				"required": ["namespace"]
			}`),
		},
		{
			Name: "get_cluster_stats",
			Description: "Get statistics about cluster data " +
				"with counts broken down by resource type and namespace.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {}
			}`),
		},
		{
			Name: "get_workloads",
			Description: "Get deployment, job, cronjob, and HPA status. " +
				"Use to check scaling, rollout status, and batch job health.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Filter by namespace"},
					"name": {"type": "string", "description": "Filter by resource name (substring match)"},
					"resource_type": {
						"type": "string",
						"description": "Filter: deployment, job, cronjob, or hpa",
						"enum": ["deployment", "job", "cronjob", "hpa"]
					},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name: "get_pod_status",
			Description: "Get pod health and status. " +
				"Optionally include resource usage metrics.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Namespace to query (required)"},
					"name": {"type": "string", "description": "Filter by pod name (substring match)"},
					"include_metrics": {"type": "boolean", "description": "Also include pod resource metrics (default: false)"},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				},
				"required": ["namespace"]
			}`),
		},
		{
			Name:        "get_events",
			Description: "Get Kubernetes events, filterable by type (Normal/Warning) and reason.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Filter by namespace"},
					"event_type": {"type": "string", "description": "Filter by event type", "enum": ["Normal", "Warning"]},
					"reason": {"type": "string", "description": "Filter by event reason (e.g. BackOff, Unhealthy, Pulling)"},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name:        "get_nodes",
			Description: "Get node health, conditions, and capacity information.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"name": {"type": "string", "description": "Filter by node name (substring match)"},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name:        "get_networking",
			Description: "Get service endpoint health and connectivity status.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Filter by namespace"},
					"service": {"type": "string", "description": "Filter by service name (substring match)"},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name:        "get_storage",
			Description: "Get PVC and PV health and volume status.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Filter by namespace"},
					"name": {"type": "string", "description": "Filter by volume name (substring match)"},
					"resource_type": {"type": "string", "description": "Filter by type: pvc or pv", "enum": ["pvc", "pv"]},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name:        "get_config",
			Description: "Get ConfigMap and Secret change tracking data.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"namespace": {"type": "string", "description": "Filter by namespace"},
					"name": {"type": "string", "description": "Filter by resource name (substring match)"},
					"resource_type": {
						"type": "string",
						"description": "Filter: configmap or secret",
						"enum": ["configmap", "secret"]
					},
					"limit": {"type": "integer", "description": "Max results (default: 20, max: 100)"}
				}
			}`),
		},
		{
			Name:        "get_traces",
			Description: "Search distributed traces from Tempo. Filter by service name, operation, duration, and time range.",
			InputSchema: json.RawMessage(`{
				"type": "object",
				"properties": {
					"service": {"type": "string", "description": "Filter by service name (e.g. accretion-api, accretion-discord)"},
					"operation": {"type": "string", "description": "Filter by span/operation name"},
					"min_duration": {"type": "string", "description": "Minimum duration (e.g. 100ms, 1s)"},
					"max_duration": {"type": "string", "description": "Maximum duration"},
					"limit": {"type": "integer", "description": "Max results (default: 20)"}
				}
			}`),
		},
	}
}

// toolExecutor handles executing tool calls against the accretion-api HTTP API.
// Every tool routes through the API so there is a single auth-enforced data path
// and the bot holds no direct database or Tempo connection.
type toolExecutor struct {
	ctx       context.Context
	apiClient *apiclient.Client
}

// execute runs a tool call and returns the result as a string.
func (te *toolExecutor) execute(call *llm.ContentBlock) string {
	switch call.Name {
	case "search_cluster":
		return te.searchCluster(call.Input)
	case "get_recent_logs":
		return te.getRecentLogs(call.Input)
	case "get_cluster_stats":
		return te.getClusterStats()
	case "get_workloads":
		return te.getWorkloads(call.Input)
	case "get_pod_status":
		return te.getPodStatus(call.Input)
	case "get_events":
		return te.getEvents(call.Input)
	case "get_nodes":
		return te.getNodes(call.Input)
	case "get_networking":
		return te.getNetworking(call.Input)
	case "get_storage":
		return te.getStorage(call.Input)
	case "get_config":
		return te.getConfig(call.Input)
	case "get_traces":
		return te.getTraces(call.Input)
	default:
		return fmt.Sprintf("Unknown tool: %s", call.Name)
	}
}

type searchClusterInput struct {
	Query     string `json:"query"`
	StartTime string `json:"start_time"`
	EndTime   string `json:"end_time"`
	Limit     int    `json:"limit"`
}

func (te *toolExecutor) searchCluster(rawInput json.RawMessage) string {
	var input searchClusterInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	result, err := te.apiClient.Search(te.ctx, input.Query, input.Limit, input.StartTime, input.EndTime)
	if err != nil {
		return fmt.Sprintf("Error searching cluster: %v", err)
	}
	return result
}

type getRecentLogsInput struct {
	Namespace string `json:"namespace"`
	Limit     int    `json:"limit"`
}

func (te *toolExecutor) getRecentLogs(rawInput json.RawMessage) string {
	var input getRecentLogsInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	result, err := te.apiClient.Logs(te.ctx, input.Namespace, input.Limit)
	if err != nil {
		return fmt.Sprintf("Error fetching logs: %v", err)
	}
	return result
}

func (te *toolExecutor) getClusterStats() string {
	result, err := te.apiClient.Stats(te.ctx)
	if err != nil {
		return fmt.Sprintf("Error fetching stats: %v", err)
	}
	return result
}

type resourceToolInput struct {
	Namespace      string `json:"namespace"`
	Name           string `json:"name"`
	Service        string `json:"service"`
	ResourceType   string `json:"resource_type"`
	EventType      string `json:"event_type"`
	Reason         string `json:"reason"`
	IncludeMetrics bool   `json:"include_metrics"`
	Limit          int    `json:"limit"`
}

// resourceParams builds the /api/resources query parameters the same way the MCP
// resource tools do (see internal/mcp/accretion/server.go addResourceTool): the
// caller supplies the resolved comma-joined resource `types`, and this appends
// the structured filters (namespace, name/service, event_type, reason, limit,
// and the ",metrics" suffix when metrics are requested).
func resourceParams(types string, in *resourceToolInput) map[string]string {
	params := map[string]string{"types": types}
	if in.Namespace != "" {
		params["namespace"] = in.Namespace
	}
	if in.Name != "" {
		params["name"] = in.Name
	}
	if in.Service != "" {
		params["name"] = in.Service
	}
	if in.EventType != "" {
		params["df.event_type"] = in.EventType
	}
	if in.Reason != "" {
		params["dc.reason"] = in.Reason
	}
	if in.Limit > 0 {
		params["limit"] = fmt.Sprintf("%d", in.Limit)
	}
	if in.IncludeMetrics {
		params["types"] += ",metrics"
	}
	return params
}

// queryResources calls the /api/resources endpoint and returns its string body,
// or a formatted error string for the model to read.
func (te *toolExecutor) queryResources(label, types string, in *resourceToolInput) string {
	result, err := te.apiClient.Resources(te.ctx, resourceParams(types, in))
	if err != nil {
		return fmt.Sprintf("Error querying %s: %v", label, err)
	}
	return result
}

var workloadTypeMap = map[string]string{
	"deployment": "deployment_status",
	"job":        "job_status",
	"cronjob":    "cronjob_status",
	"hpa":        "hpa_status",
}

func (te *toolExecutor) getWorkloads(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	types := "deployment_status,job_status,cronjob_status,hpa_status"
	if input.ResourceType != "" {
		rt, ok := workloadTypeMap[input.ResourceType]
		if !ok {
			return fmt.Sprintf("Unknown resource_type: %s", input.ResourceType)
		}
		types = rt
	}
	return te.queryResources("workloads", types, &input)
}

func (te *toolExecutor) getPodStatus(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}
	return te.queryResources("pod status", "pod_status", &input)
}

func (te *toolExecutor) getEvents(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}
	return te.queryResources("events", "event", &input)
}

func (te *toolExecutor) getNodes(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}
	return te.queryResources("nodes", "node_status", &input)
}

func (te *toolExecutor) getNetworking(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}
	return te.queryResources("networking", "endpoint_health", &input)
}

var storageTypeMap = map[string]string{
	"pvc": "pvc_status",
	"pv":  "pv_status",
}

func (te *toolExecutor) getStorage(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	types := "pvc_status,pv_status"
	if input.ResourceType != "" {
		rt, ok := storageTypeMap[input.ResourceType]
		if !ok {
			return fmt.Sprintf("Unknown resource_type: %s", input.ResourceType)
		}
		types = rt
	}
	return te.queryResources("storage", types, &input)
}

var configTypeMap = map[string]string{
	"configmap": "configmap",
	"secret":    "secret",
}

func (te *toolExecutor) getConfig(rawInput json.RawMessage) string {
	var input resourceToolInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	types := "configmap,secret"
	if input.ResourceType != "" {
		rt, ok := configTypeMap[input.ResourceType]
		if !ok {
			return fmt.Sprintf("Unknown resource_type: %s", input.ResourceType)
		}
		types = rt
	}
	return te.queryResources("config", types, &input)
}

type getTracesInput struct {
	Service     string `json:"service"`
	Operation   string `json:"operation"`
	MinDuration string `json:"min_duration"`
	MaxDuration string `json:"max_duration"`
	Limit       int    `json:"limit"`
}

func (te *toolExecutor) getTraces(rawInput json.RawMessage) string {
	var input getTracesInput
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return fmt.Sprintf("Error parsing input: %v", err)
	}

	raw, err := te.apiClient.SearchTraces(
		te.ctx, input.Service, input.Operation,
		input.MinDuration, input.MaxDuration, input.Limit, "", "",
	)
	if err != nil {
		return fmt.Sprintf("Error searching traces: %v", err)
	}
	return string(raw)
}

// runAgentLoop executes the agentic tool-use loop, returning the final text response.
func (b *Bot) runAgentLoop(
	toolUser llm.ToolUser,
	systemPrompt string,
	history []llm.ToolMessage,
	userMessage string,
) (string, error) {
	ctx := context.Background()
	log := logger.Logger

	tools := clusterTools()
	executor := &toolExecutor{
		ctx:       ctx,
		apiClient: b.apiClient,
	}

	// Prior turns first, then this message. History holds only distilled
	// user/assistant text (see conversationStore), so the tool_use and
	// tool_result blocks appended below always pair up within this turn.
	messages := make([]llm.ToolMessage, 0, len(history)+1)
	messages = append(messages, history...)
	messages = append(messages, textMessage("user", userMessage))

	for range maxToolIterations {
		resp, err := toolUser.ChatWithTools(
			ctx, systemPrompt, messages, tools,
		)
		if err != nil {
			return "", fmt.Errorf("tool-use chat failed: %w", err)
		}

		if !resp.NeedsToolUse() {
			return resp.TextContent(), nil
		}

		// Append the assistant's response (with tool_use blocks) to messages
		messages = append(messages, llm.ToolMessage{
			Role:    "assistant",
			Content: resp.Content,
		})

		// Execute each tool call and build tool_result blocks
		calls := resp.ToolCalls()
		toolResults := make([]llm.ContentBlock, 0, len(calls))
		for i := range calls {
			log.Info("Executing tool",
				zap.String("tool", calls[i].Name),
				zap.String("id", calls[i].ID),
			)

			result := executor.execute(&calls[i])

			log.Info("Tool result",
				zap.String("tool", calls[i].Name),
				zap.Int("result_length", len(result)),
			)

			toolResults = append(toolResults, llm.ContentBlock{
				Type:      "tool_result",
				ToolUseID: calls[i].ID,
				Content:   result,
			})
		}

		// Append tool results as a user message
		messages = append(messages, llm.ToolMessage{
			Role:    "user",
			Content: toolResults,
		})
	}

	return "", fmt.Errorf("agent exceeded maximum iterations (%d)", maxToolIterations)
}

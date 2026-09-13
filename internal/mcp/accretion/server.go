package accretion

import (
	"context"
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
)

// NewServer creates an MCP server with Accretion tools.
func NewServer(client *Client, version string) *server.MCPServer {
	s := server.NewMCPServer(
		"accretion",
		version,
		server.WithToolCapabilities(true),
	)

	addCoreTools(s, client)
	addResourceTools(s, client)
	addTraceTools(s, client)

	return s
}

func addTraceTools(s *server.MCPServer, client *Client) {
	s.AddTool(
		mcp.NewTool("accretion_traces",
			mcp.WithDescription(
				"Search distributed traces from Tempo. Filter by service name, operation, duration, and time range.",
			),
			mcp.WithString("service", mcp.Description("Filter by service name (e.g. accretion-api, accretion-discord)")),
			mcp.WithString("operation", mcp.Description("Filter by span/operation name")),
			mcp.WithString("minDuration", mcp.Description("Minimum duration (e.g. 100ms, 1s)")),
			mcp.WithString("maxDuration", mcp.Description("Maximum duration")),
			mcp.WithNumber("limit", mcp.Description("Max results (default: 20)")),
			mcp.WithString("start", mcp.Description("Start time in RFC3339 format")),
			mcp.WithString("end", mcp.Description("End time in RFC3339 format")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			service := request.GetString("service", "")
			operation := request.GetString("operation", "")
			minDuration := request.GetString("minDuration", "")
			maxDuration := request.GetString("maxDuration", "")
			start := request.GetString("start", "")
			end := request.GetString("end", "")
			limit := 0
			if l, ok := request.GetArguments()["limit"].(float64); ok {
				limit = int(l)
			}

			result, err := client.SearchTraces(ctx, service, operation, minDuration, maxDuration, limit, start, end)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_traces failed: %v", err)), nil
			}
			return mcp.NewToolResultText(string(result)), nil
		},
	)

	s.AddTool(
		mcp.NewTool("accretion_trace",
			mcp.WithDescription("Get a specific distributed trace by its trace ID."),
			mcp.WithString("trace_id", mcp.Required(), mcp.Description("The trace ID to retrieve")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			traceID := request.GetString("trace_id", "")
			if traceID == "" {
				return mcp.NewToolResultError("trace_id is required"), nil
			}

			result, err := client.GetTrace(ctx, traceID)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_trace failed: %v", err)), nil
			}
			return mcp.NewToolResultText(string(result)), nil
		},
	)
}

func addCoreTools(s *server.MCPServer, client *Client) {
	s.AddTool(
		mcp.NewTool("accretion_ask",
			//nolint:lll // tool description
			mcp.WithDescription("Ask Accretion about your Kubernetes cluster. Accretion uses RAG (vector search over logs, metrics, events, pod/node/deployment status, HPAs, PVCs, jobs, cronjobs, configmaps, secrets) to answer questions about cluster behavior, diagnose issues, and provide root cause analysis."),
			mcp.WithString("question", mcp.Required(), mcp.Description("Your question about the Kubernetes cluster")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			question := request.GetString("question", "")
			if question == "" {
				return mcp.NewToolResultError("question is required"), nil
			}

			result, err := client.Chat(ctx, question)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_ask failed: %v", err)), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	s.AddTool(
		mcp.NewTool("accretion_logs",
			mcp.WithDescription("Retrieve recent log entries from the Kubernetes cluster."),
			mcp.WithString("namespace", mcp.Description("Kubernetes namespace to query (default: \"default\")")),
			mcp.WithNumber("limit", mcp.Description("Maximum number of log entries to return (default: 50)")),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			namespace := request.GetString("namespace", "")
			limit := 0
			args := request.GetArguments()
			if l, ok := args["limit"].(float64); ok {
				limit = int(l)
			}

			result, err := client.Logs(ctx, namespace, limit)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_logs failed: %v", err)), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)

	s.AddTool(
		mcp.NewTool("accretion_health",
			mcp.WithDescription("Check if the Accretion API server is reachable and healthy."),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := client.Health(ctx)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_health failed: %v", err)), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)
}

//nolint:cyclop,gocyclo // parameter dispatch for tool registration
func addResourceTool(s *server.MCPServer, client *Client, name, description, types string, params []string) {
	opts := []mcp.ToolOption{mcp.WithDescription(description)}
	for _, p := range params {
		switch p {
		case "namespace":
			opts = append(opts, mcp.WithString("namespace", mcp.Description("Kubernetes namespace")))
		case "name":
			opts = append(opts, mcp.WithString("name", mcp.Description("Resource name (substring match)")))
		case "service":
			opts = append(opts, mcp.WithString("service", mcp.Description("Service name (substring match)")))
		case "resource_type":
			opts = append(opts, mcp.WithString("resource_type", mcp.Description("Resource sub-type filter")))
		case "event_type":
			opts = append(opts, mcp.WithString("event_type", mcp.Description("Event type: Normal or Warning")))
		case "reason":
			opts = append(opts, mcp.WithString("reason", mcp.Description("Event reason filter")))
		case "include_metrics":
			opts = append(opts, mcp.WithBoolean("include_metrics", mcp.Description("Include resource usage metrics")))
		}
	}
	opts = append(opts, mcp.WithNumber("limit", mcp.Description("Max results (default: 20, max: 100)")))

	s.AddTool(
		mcp.NewTool(name, opts...),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			qp := map[string]string{"types": types}
			args := request.GetArguments()

			if v := request.GetString("namespace", ""); v != "" {
				qp["namespace"] = v
			}
			if v := request.GetString("name", ""); v != "" {
				qp["name"] = v
			}
			if v := request.GetString("service", ""); v != "" {
				qp["name"] = v
			}
			if v := request.GetString("event_type", ""); v != "" {
				qp["df.event_type"] = v
			}
			if v := request.GetString("reason", ""); v != "" {
				qp["dc.reason"] = v
			}
			if v := request.GetString("resource_type", ""); v != "" {
				qp["types"] = resolveResourceType(name, v)
			}
			if l, ok := args["limit"].(float64); ok && l > 0 {
				qp["limit"] = fmt.Sprintf("%d", int(l))
			}
			if b, ok := args["include_metrics"].(bool); ok && b {
				qp["types"] += ",metrics"
			}

			result, err := client.Resources(ctx, qp)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("%s failed: %v", name, err)), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)
}

func resolveResourceType(toolName, subType string) string {
	m := map[string]map[string]string{
		"accretion_workloads": {
			"deployment": "deployment_status",
			"job":        "job_status",
			"cronjob":    "cronjob_status",
			"hpa":        "hpa_status",
		},
		"accretion_storage": {"pvc": "pvc_status", "pv": "pv_status"},
		"accretion_config":  {"configmap": "configmap", "secret": "secret"},
	}
	if types, ok := m[toolName]; ok {
		if resolved, ok := types[subType]; ok {
			return resolved
		}
	}
	return subType
}

func addResourceTools(s *server.MCPServer, client *Client) {
	addResourceTool(s, client, "accretion_workloads",
		"Get deployment, job, cronjob, and HPA status from the Kubernetes cluster.",
		"deployment_status,job_status,cronjob_status,hpa_status",
		[]string{"namespace", "name", "resource_type"})

	addResourceTool(s, client, "accretion_pods",
		"Get pod health status and optionally resource usage metrics.",
		"pod_status",
		[]string{"namespace", "name", "include_metrics"})

	addResourceTool(s, client, "accretion_events",
		"Get Kubernetes events, filterable by type (Normal/Warning) and reason.",
		"event",
		[]string{"namespace", "event_type", "reason"})

	addResourceTool(s, client, "accretion_nodes",
		"Get node health, conditions, and capacity information.",
		"node_status",
		[]string{"name"})

	addResourceTool(s, client, "accretion_networking",
		"Get service endpoint health and connectivity status.",
		"endpoint_health",
		[]string{"namespace", "service"})

	addResourceTool(s, client, "accretion_storage",
		"Get PVC and PV health and volume status.",
		"pvc_status,pv_status",
		[]string{"namespace", "name", "resource_type"})

	addResourceTool(s, client, "accretion_config",
		"Get ConfigMap and Secret change tracking data.",
		"configmap,secret",
		[]string{"namespace", "name", "resource_type"})

	s.AddTool(
		mcp.NewTool("accretion_stats",
			mcp.WithDescription("Get cluster data statistics with counts by resource type and namespace."),
		),
		func(ctx context.Context, request mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			result, err := client.Stats(ctx)
			if err != nil {
				return mcp.NewToolResultError(fmt.Sprintf("accretion_stats failed: %v", err)), nil
			}
			return mcp.NewToolResultText(result), nil
		},
	)
}

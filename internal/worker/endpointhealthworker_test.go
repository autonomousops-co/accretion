package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

func TestNewEndpointHealthWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewEndpointHealthWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "endpoint-health" {
		t.Errorf("expected type 'endpoint-health', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "endpoint-health-") {
		t.Errorf("expected ID to start with 'endpoint-health-', got %q", w.ID())
	}
	if !w.IsHealthy() {
		t.Error("expected new worker to be healthy")
	}
	if w.GetAssignedPods() != nil {
		t.Errorf("expected nil assigned pods, got %v", w.GetAssignedPods())
	}
	if w.GetErrorCount() != 0 {
		t.Errorf("expected 0 errors, got %d", w.GetErrorCount())
	}
}

func TestEndpointHealthWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewEndpointHealthWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err := w.Start(ctx)
	if err != context.DeadlineExceeded {
		t.Errorf("expected context.DeadlineExceeded, got %v", err)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err := w.Stop(stopCtx); err != nil {
		t.Errorf("unexpected stop error: %v", err)
	}
}

func TestEndpointHealthWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewEndpointHealthWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildEndpointHealthEmbeddingText(t *testing.T) {
	tests := []struct {
		name       string
		contains   []string
		notContain []string
		status     k8s.EndpointHealthEntry
	}{
		{
			name: "healthy service",
			status: k8s.EndpointHealthEntry{
				Namespace:     "default",
				ServiceName:   "my-svc",
				ReadyCount:    3,
				NotReadyCount: 0,
				TotalCount:    3,
			},
			contains: []string{
				"Endpoint Health: default/my-svc",
				"Ready: 3",
				"NotReady: 0",
				"Total: 3",
			},
			notContain: []string{"complete outage", "not ready"},
		},
		{
			name: "complete outage",
			status: k8s.EndpointHealthEntry{
				Namespace:     "default",
				ServiceName:   "my-svc",
				ReadyCount:    0,
				NotReadyCount: 3,
				TotalCount:    3,
			},
			contains: []string{
				"zero ready endpoints (complete outage)",
			},
		},
		{
			name: "no endpoints",
			status: k8s.EndpointHealthEntry{
				Namespace:   "default",
				ServiceName: "my-svc",
				TotalCount:  0,
			},
			contains: []string{
				"no endpoints",
			},
		},
		{
			name: "partially unhealthy with target refs",
			status: k8s.EndpointHealthEntry{
				Namespace:     "default",
				ServiceName:   "my-svc",
				ReadyCount:    2,
				NotReadyCount: 1,
				TotalCount:    3,
				Endpoints: []k8s.EndpointInfo{
					{Conditions: k8s.EndpointConditionInfo{Ready: true}},
					{Conditions: k8s.EndpointConditionInfo{Ready: true}},
					{Conditions: k8s.EndpointConditionInfo{Ready: false, Terminating: true}, TargetRef: "Pod/my-pod-3"},
				},
			},
			contains: []string{
				"some endpoints not ready",
				"Endpoint not-ready",
				"target: Pod/my-pod-3",
				"terminating",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildEndpointHealthEmbeddingText(&tt.status)
			for _, substr := range tt.contains {
				if !strings.Contains(text, substr) {
					t.Errorf("expected embedding text to contain %q, got:\n%s", substr, text)
				}
			}
			for _, substr := range tt.notContain {
				if strings.Contains(text, substr) {
					t.Errorf("expected embedding text NOT to contain %q, got:\n%s", substr, text)
				}
			}
		})
	}
}

func TestEndpointHealthWorker_Deduplication(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbMock, ok := database.NewMockClient().(*database.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	llmClient := llm.NewMockClient()

	// Add an endpoint health entry
	mockK8s.AddEndpointHealthEntry("default", &k8s.EndpointHealthEntry{
		Timestamp:     time.Now(),
		Namespace:     "default",
		ServiceName:   "my-svc",
		ReadyCount:    3,
		NotReadyCount: 0,
		TotalCount:    3,
		Endpoints: []k8s.EndpointInfo{
			{Addresses: []string{"10.0.0.1"}, Conditions: k8s.EndpointConditionInfo{Ready: true}},
		},
	})

	w := NewEndpointHealthWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	mock, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	callCount := mock.GetEmbeddingCallCount()
	if callCount != 1 {
		t.Errorf("expected exactly 1 embedding call (dedup should prevent re-processing), got %d", callCount)
	}
}

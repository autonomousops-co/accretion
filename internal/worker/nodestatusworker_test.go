package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

func TestNewNodeStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewNodeStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second)

	if w.Type() != "node-status" {
		t.Errorf("expected type 'node-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "node-status-") {
		t.Errorf("expected ID to start with 'node-status-', got %q", w.ID())
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

func TestNodeStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewNodeStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond)

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

func TestNodeStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewNodeStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second)

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildNodeStatusEmbeddingText(t *testing.T) {
	tests := []struct {
		name       string
		status     k8s.NodeStatusEntry
		contains   []string
		notContain []string
	}{
		{
			name: "basic node status",
			status: k8s.NodeStatusEntry{
				NodeName: "node-1",
				CPU:      k8s.NodeResourceInfo{Allocatable: "4", Capacity: "4"},
				Memory:   k8s.NodeResourceInfo{Allocatable: "8Gi", Capacity: "8Gi"},
			},
			contains: []string{
				"Node Status: node-1",
				"CPU: 4/4",
				"Memory: 8Gi/8Gi",
			},
			notContain: []string{"Condition", "EphemeralStorage"},
		},
		{
			name: "node with conditions",
			status: k8s.NodeStatusEntry{
				NodeName: "node-2",
				CPU:      k8s.NodeResourceInfo{Allocatable: "4", Capacity: "4"},
				Memory:   k8s.NodeResourceInfo{Allocatable: "8Gi", Capacity: "8Gi"},
				Conditions: []k8s.NodeConditionInfo{
					{Type: "Ready", Status: "True"},
					{Type: "MemoryPressure", Status: "True", Reason: "KubeletHasInsufficientMemory", Message: "node has insufficient memory"},
				},
			},
			contains: []string{
				"Condition Ready=True",
				"Condition MemoryPressure=True",
				"reason: KubeletHasInsufficientMemory",
				"message: node has insufficient memory",
			},
		},
		{
			name: "node with full resource and info",
			status: k8s.NodeStatusEntry{
				NodeName:         "node-3",
				CPU:              k8s.NodeResourceInfo{Allocatable: "3800m", Capacity: "4"},
				Memory:           k8s.NodeResourceInfo{Allocatable: "7Gi", Capacity: "8Gi"},
				EphemeralStorage: k8s.NodeResourceInfo{Allocatable: "90Gi", Capacity: "100Gi"},
				Pods:             k8s.NodeResourceInfo{Allocatable: "110", Capacity: "110"},
				Info: k8s.NodeInfoDetails{
					KubeletVersion:   "v1.28.0",
					ContainerRuntime: "containerd://1.7.0",
				},
			},
			contains: []string{
				"CPU: 3800m/4",
				"EphemeralStorage: 90Gi/100Gi",
				"Pods: 110/110",
				"Kubelet: v1.28.0",
				"Runtime: containerd://1.7.0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildNodeStatusEmbeddingText(&tt.status)
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

func TestNodeStatusWorker_Deduplication(t *testing.T) {
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

	// Add a node with status
	mockK8s.AddNodeStatusEntry(&k8s.NodeStatusEntry{
		Timestamp: time.Now(),
		NodeName:  "node-1",
		Conditions: []k8s.NodeConditionInfo{
			{Type: "Ready", Status: "True"},
		},
		CPU:    k8s.NodeResourceInfo{Allocatable: "4", Capacity: "4"},
		Memory: k8s.NodeResourceInfo{Allocatable: "8Gi", Capacity: "8Gi"},
	})

	w := NewNodeStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	// Start the worker; it will poll multiple times before the context expires.
	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called only once because the node status
	// hasn't changed between polls, so the content hash dedup should kick in.
	mock, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	callCount := mock.GetEmbeddingCallCount()
	if callCount != 1 {
		t.Errorf("expected exactly 1 embedding call (dedup should prevent re-processing), got %d", callCount)
	}
}

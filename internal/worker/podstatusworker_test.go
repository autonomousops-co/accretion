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

func TestNewPodStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewPodStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "pod-status" {
		t.Errorf("expected type 'pod-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "pod-status-") {
		t.Errorf("expected ID to start with 'pod-status-', got %q", w.ID())
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

func TestPodStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewPodStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

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

func TestPodStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewPodStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildPodStatusEmbeddingText(t *testing.T) {
	tests := []struct {
		name       string
		status     k8s.PodStatusEntry
		contains   []string
		notContain []string
	}{
		{
			name: "basic pod status",
			status: k8s.PodStatusEntry{
				Namespace: "default",
				PodName:   "my-app-abc123",
				Phase:     "Running",
			},
			contains: []string{
				"Pod Status: default/my-app-abc123",
				"Phase: Running",
			},
			notContain: []string{"Condition", "Container"},
		},
		{
			name: "pod with conditions",
			status: k8s.PodStatusEntry{
				Namespace: "default",
				PodName:   "my-app-abc123",
				Phase:     "Pending",
				Conditions: []k8s.PodConditionInfo{
					{Type: "PodScheduled", Status: "True"},
					{Type: "Ready", Status: "False", Reason: "ContainersNotReady", Message: "containers not ready"},
				},
			},
			contains: []string{
				"Phase: Pending",
				"Condition PodScheduled=True",
				"Condition Ready=False",
				"reason: ContainersNotReady",
				"message: containers not ready",
			},
		},
		{
			name: "pod with container statuses",
			status: k8s.PodStatusEntry{
				Namespace: "default",
				PodName:   "my-app-abc123",
				Phase:     "Running",
				ContainerStatuses: []k8s.ContainerStateInfo{
					{Name: "app", State: "running"},
					{Name: "sidecar", State: "waiting", Reason: "CrashLoopBackOff", RestartCount: 5},
					{Name: "init", State: "terminated", Reason: "OOMKilled", ExitCode: 137, Signal: 9},
				},
			},
			contains: []string{
				"Container app: running",
				"Container sidecar: waiting",
				"reason: CrashLoopBackOff",
				"restarts: 5",
				"Container init: terminated",
				"reason: OOMKilled",
				"exit_code: 137",
				"signal: 9",
			},
			notContain: []string{
				"restarts: 0",
				"signal: 0",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildPodStatusEmbeddingText(&tt.status)
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

func TestPodStatusWorker_Deduplication(t *testing.T) {
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

	// Add a pod with status
	pod := k8s.CreateMockPod("default", "my-app-abc123", []string{"app"})
	mockK8s.AddPod("default", &pod)

	w := NewPodStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	// Start the worker; it will poll multiple times before the context expires.
	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called only once because the pod status
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

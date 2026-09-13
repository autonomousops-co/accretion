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

func TestNewHPAStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewHPAStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "hpa-status" {
		t.Errorf("expected type 'hpa-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "hpa-status-") {
		t.Errorf("expected ID to start with 'hpa-status-', got %q", w.ID())
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

func TestHPAStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewHPAStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

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

func TestHPAStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewHPAStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildHPAStatusEmbeddingText(t *testing.T) {
	lastScale := time.Date(2024, 1, 15, 10, 30, 0, 0, time.UTC)

	tests := []struct {
		name       string
		contains   []string
		notContain []string
		status     k8s.HPAStatusEntry
	}{
		{
			name: "basic HPA status",
			status: k8s.HPAStatusEntry{
				Namespace:       "default",
				HPAName:         "my-app",
				CurrentReplicas: 3,
				DesiredReplicas: 3,
				MinReplicas:     1,
				MaxReplicas:     10,
			},
			contains: []string{
				"HPA Status: default/my-app",
				"Replicas: 3/3",
				"min: 1",
				"max: 10",
			},
			notContain: []string{"scaling up", "scaling down", "at max replicas"},
		},
		{
			name: "HPA at max replicas",
			status: k8s.HPAStatusEntry{
				Namespace:       "default",
				HPAName:         "my-app",
				CurrentReplicas: 10,
				DesiredReplicas: 10,
				MinReplicas:     1,
				MaxReplicas:     10,
			},
			contains: []string{
				"at max replicas",
			},
		},
		{
			name: "HPA scaling up",
			status: k8s.HPAStatusEntry{
				Namespace:       "default",
				HPAName:         "my-app",
				CurrentReplicas: 3,
				DesiredReplicas: 5,
				MinReplicas:     1,
				MaxReplicas:     10,
			},
			contains: []string{
				"scaling up",
			},
			notContain: []string{"scaling down"},
		},
		{
			name: "HPA with conditions and metrics",
			status: k8s.HPAStatusEntry{
				Namespace:       "default",
				HPAName:         "my-app",
				CurrentReplicas: 3,
				DesiredReplicas: 3,
				MinReplicas:     1,
				MaxReplicas:     10,
				Conditions: []k8s.HPAConditionInfo{
					{Type: "ScalingActive", Status: "True", Reason: "ValidMetricFound"},
					{Type: "ScalingLimited", Status: "True", Reason: "TooManyReplicas", Message: "the desired replica count is more than the maximum replica count"},
				},
				Metrics: []k8s.HPAMetricStatus{
					{Name: "cpu", CurrentValue: "75%", TargetValue: "80%"},
				},
				LastScaleTime: &lastScale,
			},
			contains: []string{
				"Condition ScalingActive=True",
				"reason: ValidMetricFound",
				"Condition ScalingLimited=True",
				"reason: TooManyReplicas",
				"message: the desired replica count is more than the maximum replica count",
				"Metric cpu: current=75%",
				"target=80%",
				"LastScaleTime:",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildHPAStatusEmbeddingText(&tt.status)
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

func TestHPAStatusWorker_Deduplication(t *testing.T) {
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

	// Add an HPA with status
	mockK8s.AddHPAStatusEntry("default", &k8s.HPAStatusEntry{
		Timestamp:       time.Now(),
		Namespace:       "default",
		HPAName:         "my-app",
		CurrentReplicas: 3,
		DesiredReplicas: 3,
		MinReplicas:     1,
		MaxReplicas:     10,
		Conditions: []k8s.HPAConditionInfo{
			{Type: "ScalingActive", Status: "True"},
		},
	})

	w := NewHPAStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	// Start the worker; it will poll multiple times before the context expires.
	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called only once because the HPA status
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

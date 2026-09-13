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

func TestNewDeploymentStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewDeploymentStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "deployment-status" {
		t.Errorf("expected type 'deployment-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "deployment-status-") {
		t.Errorf("expected ID to start with 'deployment-status-', got %q", w.ID())
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

func TestDeploymentStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewDeploymentStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

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

func TestDeploymentStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewDeploymentStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildDeploymentStatusEmbeddingText(t *testing.T) {
	tests := []struct {
		name       string
		contains   []string
		notContain []string
		status     k8s.DeploymentStatusEntry
	}{
		{
			name: "basic deployment status",
			status: k8s.DeploymentStatusEntry{
				Namespace:      "default",
				DeploymentName: "my-app",
				Replicas: k8s.DeploymentReplicaCounts{
					Desired: 3, Ready: 3, Available: 3, Updated: 3,
				},
			},
			contains: []string{
				"Deployment Status: default/my-app",
				"desired=3", "ready=3", "available=3", "updated=3",
			},
			notContain: []string{"Condition", "ChangeCause"},
		},
		{
			name: "deployment with conditions",
			status: k8s.DeploymentStatusEntry{
				Namespace:      "default",
				DeploymentName: "my-app",
				Replicas: k8s.DeploymentReplicaCounts{
					Desired: 3, Ready: 2, Available: 2, Unavailable: 1, Updated: 3,
				},
				Conditions: []k8s.DeploymentConditionInfo{
					{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"},
					{Type: "Progressing", Status: "True", Reason: "ReplicaSetUpdated", Message: "ReplicaSet my-app-abc updated"},
				},
			},
			contains: []string{
				"unavailable=1",
				"Condition Available=True",
				"reason: MinimumReplicasAvailable",
				"Condition Progressing=True",
				"reason: ReplicaSetUpdated",
				"message: ReplicaSet my-app-abc updated",
			},
		},
		{
			name: "deployment with replica sets and change cause",
			status: k8s.DeploymentStatusEntry{
				Namespace:      "default",
				DeploymentName: "my-app",
				Replicas: k8s.DeploymentReplicaCounts{
					Desired: 3, Ready: 3, Available: 3, Updated: 3,
				},
				ReplicaSets: []k8s.ReplicaSetInfo{
					{Name: "my-app-abc123", Revision: "3", Replicas: 3, ReadyReplicas: 3},
					{Name: "my-app-def456", Revision: "2", Replicas: 0, ReadyReplicas: 0},
				},
				ChangeCause: "image updated to v2.0",
			},
			contains: []string{
				"ReplicaSet my-app-abc123 revision=3",
				"ReplicaSet my-app-def456 revision=2",
				"ChangeCause: image updated to v2.0",
			},
		},
		{
			name: "stuck deployment with ProgressDeadlineExceeded",
			status: k8s.DeploymentStatusEntry{
				Namespace:      "production",
				DeploymentName: "api-server",
				Replicas: k8s.DeploymentReplicaCounts{
					Desired: 5, Ready: 3, Available: 3, Unavailable: 2, Updated: 2,
				},
				Conditions: []k8s.DeploymentConditionInfo{
					{Type: "Progressing", Status: "False", Reason: "ProgressDeadlineExceeded",
						Message: "ReplicaSet api-server-xyz has timed out progressing"},
					{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"},
				},
			},
			contains: []string{
				"Deployment Status: production/api-server",
				"unavailable=2",
				"reason: ProgressDeadlineExceeded",
				"message: ReplicaSet api-server-xyz has timed out progressing",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildDeploymentStatusEmbeddingText(&tt.status)
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

func TestDeploymentStatusWorker_Deduplication(t *testing.T) {
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

	// Add a deployment status entry.
	mockK8s.AddDeploymentStatusEntry("default", &k8s.DeploymentStatusEntry{
		Timestamp:      time.Now(),
		Namespace:      "default",
		DeploymentName: "my-app",
		Replicas: k8s.DeploymentReplicaCounts{
			Desired: 3, Ready: 3, Available: 3, Updated: 3,
		},
		Conditions: []k8s.DeploymentConditionInfo{
			{Type: "Available", Status: "True", Reason: "MinimumReplicasAvailable"},
			{Type: "Progressing", Status: "True", Reason: "NewReplicaSetAvailable"},
		},
	})

	w := NewDeploymentStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	// Start the worker; it will poll multiple times before the context expires.
	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called only once because the deployment status
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

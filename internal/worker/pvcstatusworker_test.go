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

func TestNewPVCStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewPVCStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "pvc-status" {
		t.Errorf("expected type 'pvc-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "pvc-status-") {
		t.Errorf("expected ID to start with 'pvc-status-', got %q", w.ID())
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

func TestPVCStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewPVCStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

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

func TestPVCStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewPVCStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildPVCStatusEmbeddingText(t *testing.T) {
	tests := []struct { //nolint:govet // test struct field alignment not important
		name       string
		status     k8s.PVCStatusEntry
		contains   []string
		notContain []string
	}{
		{
			name: "bound PVC",
			status: k8s.PVCStatusEntry{
				Namespace:    "default",
				PVCName:      "my-pvc",
				Phase:        "Bound",
				StorageClass: "gp3",
				Capacity:     "10Gi",
				VolumeName:   "pv-123",
			},
			contains: []string{
				"PVC Status: default/my-pvc",
				"Phase: Bound",
				"StorageClass: gp3",
				"Capacity: 10Gi",
				"Volume: pv-123",
			},
			notContain: []string{"stuck in Pending", "lost"},
		},
		{
			name: "pending PVC",
			status: k8s.PVCStatusEntry{
				Namespace:    "default",
				PVCName:      "my-pvc",
				Phase:        "Pending",
				StorageClass: "gp3",
			},
			contains: []string{
				"Phase: Pending",
				"stuck in Pending",
			},
		},
		{
			name: "lost PVC",
			status: k8s.PVCStatusEntry{
				Namespace: "default",
				PVCName:   "my-pvc",
				Phase:     "Lost",
			},
			contains: []string{
				"Phase: Lost",
				"lost",
			},
		},
		{
			name: "PVC with conditions",
			status: k8s.PVCStatusEntry{
				Namespace: "default",
				PVCName:   "my-pvc",
				Phase:     "Bound",
				Conditions: []k8s.PVCConditionInfo{
					{Type: "Resizing", Status: "True", Reason: "VolumeExpansion", Message: "expanding volume"},
				},
			},
			contains: []string{
				"Condition Resizing=True",
				"reason: VolumeExpansion",
				"message: expanding volume",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildPVCStatusEmbeddingText(&tt.status)
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

func TestBuildPVStatusEmbeddingText(t *testing.T) {
	tests := []struct { //nolint:govet // test struct field alignment not important
		name     string
		status   k8s.PVStatusEntry
		contains []string
	}{
		{
			name: "bound PV",
			status: k8s.PVStatusEntry{
				PVName:        "pv-123",
				Phase:         "Bound",
				ReclaimPolicy: "Delete",
				StorageClass:  "gp3",
				Capacity:      "10Gi",
				ClaimRef:      "default/my-pvc",
			},
			contains: []string{
				"PV Status: pv-123",
				"Phase: Bound",
				"ReclaimPolicy: Delete",
				"StorageClass: gp3",
				"ClaimRef: default/my-pvc",
			},
		},
		{
			name: "failed PV",
			status: k8s.PVStatusEntry{
				PVName:  "pv-456",
				Phase:   "Failed",
				Reason:  "AttachError",
				Message: "failed to attach volume",
			},
			contains: []string{
				"PV in failed state",
				"Reason: AttachError",
				"Message: failed to attach volume",
			},
		},
		{
			name: "released PV",
			status: k8s.PVStatusEntry{
				PVName: "pv-789",
				Phase:  "Released",
			},
			contains: []string{
				"PV released",
				"manual cleanup",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildPVStatusEmbeddingText(&tt.status)
			for _, substr := range tt.contains {
				if !strings.Contains(text, substr) {
					t.Errorf("expected embedding text to contain %q, got:\n%s", substr, text)
				}
			}
		})
	}
}

func TestPVCStatusWorker_Deduplication(t *testing.T) {
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

	// Add a PVC with status
	mockK8s.AddPVCStatusEntry("default", &k8s.PVCStatusEntry{
		Timestamp:    time.Now(),
		Namespace:    "default",
		PVCName:      "my-pvc",
		Phase:        "Bound",
		StorageClass: "gp3",
		Capacity:     "10Gi",
		VolumeName:   "pv-123",
	})

	// Add a PV
	mockK8s.AddPVStatusEntry(&k8s.PVStatusEntry{
		Timestamp:     time.Now(),
		PVName:        "pv-123",
		Phase:         "Bound",
		ReclaimPolicy: "Delete",
		StorageClass:  "gp3",
		ClaimRef:      "default/my-pvc",
	})

	w := NewPVCStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called exactly twice (once for PVC, once for PV)
	// because the statuses haven't changed between polls, so dedup should kick in.
	mock, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	callCount := mock.GetEmbeddingCallCount()
	if callCount != 2 {
		t.Errorf("expected exactly 2 embedding calls (1 PVC + 1 PV, dedup should prevent re-processing), got %d", callCount)
	}
}

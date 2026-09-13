package worker

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/database"
	"github.com/autonomousops-co/accretion/internal/k8s"
	"github.com/autonomousops-co/accretion/internal/llm"
	"github.com/autonomousops-co/accretion/internal/nsscope"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

// TestPodStatusWorker_IngestsFromWatch drives the worker end to end through the
// mock's watch emulation (#96): the informer's initial delivery must land as a
// pod_status row, and an out-of-scope namespace must not.
func TestPodStatusWorker_IngestsFromWatch(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}
	for _, ns := range []string{"default", "elsewhere"} {
		mockK8s.AddPod(ns, &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "web", Namespace: ns},
			Status:     corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	db, ok := database.NewMockClient().(*database.MockClient)
	if !ok {
		t.Fatal("failed to cast to database MockClient")
	}

	w := NewPodStatusWorker(mockK8s, db, llm.NewMockClient(), time.Hour,
		nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := w.Start(ctx); err != context.DeadlineExceeded {
		t.Fatalf("expected context.DeadlineExceeded, got %v", err)
	}

	rows, err := db.QueryResources(&database.ResourceQuery{ResourceTypes: []string{"pod_status"}})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected exactly one pod_status row (the in-scope pod), got %d", len(rows))
	}
	if rows[0].Namespace != "default" || rows[0].ResourceName != "web" {
		t.Fatalf("unexpected row %+v", rows[0])
	}
}

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

func TestNewJobStatusWorker(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()
	namespaces := nsscope.Static(config.NamespaceSelector{Include: []string{"default", "kube-system"}})

	w := NewJobStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, namespaces)

	if w.Type() != "job-status" {
		t.Errorf("expected type 'job-status', got %q", w.Type())
	}
	if w.ID() == "" {
		t.Error("expected non-empty worker ID")
	}
	if !strings.HasPrefix(w.ID(), "job-status-") {
		t.Errorf("expected ID to start with 'job-status-', got %q", w.ID())
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

func TestJobStatusWorker_StartStop(t *testing.T) {
	testutil.SetupTestEnvironment(t)

	mockK8s, ok := k8s.NewMockClient().(*k8s.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewJobStatusWorker(mockK8s, dbClient, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

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

func TestJobStatusWorker_HealthReporting(t *testing.T) {
	k8sClient := k8s.NewMockClient()
	dbClient := database.NewMockClient()
	llmClient := llm.NewMockClient()

	w := NewJobStatusWorker(k8sClient, dbClient, llmClient, 30*time.Second, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx := context.Background()
	if err := w.StartHealthReporting(ctx); err != nil {
		t.Errorf("unexpected health reporting error: %v", err)
	}
}

func TestBuildJobStatusEmbeddingText(t *testing.T) {
	tests := []struct {
		name       string
		contains   []string
		notContain []string
		status     k8s.JobStatusEntry
	}{
		{
			name: "running job",
			status: k8s.JobStatusEntry{
				Namespace: "default",
				JobName:   "my-job",
				Active:    2,
				Succeeded: 0,
				Failed:    0,
				Duration:  5 * time.Minute,
			},
			contains: []string{
				"Job Status: default/my-job",
				"Active: 2",
				"Succeeded: 0",
				"Failed: 0",
				"Duration: 5m0s",
			},
			notContain: []string{"deadline exceeded", "has failures"},
		},
		{
			name: "failed job with deadline exceeded",
			status: k8s.JobStatusEntry{
				Namespace:        "default",
				JobName:          "my-job",
				Active:           0,
				Succeeded:        0,
				Failed:           3,
				DeadlineExceeded: true,
				Conditions: []k8s.JobConditionInfo{
					{Type: "Failed", Status: "True", Reason: "DeadlineExceeded", Message: "Job exceeded deadline"},
				},
			},
			contains: []string{
				"deadline exceeded",
				"job has failures",
				"Condition Failed=True",
				"reason: DeadlineExceeded",
			},
		},
		{
			name: "completed job",
			status: k8s.JobStatusEntry{
				Namespace: "default",
				JobName:   "my-job",
				Active:    0,
				Succeeded: 3,
				Failed:    0,
				Duration:  2 * time.Minute,
				Conditions: []k8s.JobConditionInfo{
					{Type: "Complete", Status: "True"},
				},
			},
			contains: []string{
				"Succeeded: 3",
				"Condition Complete=True",
			},
			notContain: []string{"deadline exceeded", "has failures"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildJobStatusEmbeddingText(&tt.status)
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

func TestBuildCronJobStatusEmbeddingText(t *testing.T) {
	now := time.Now()
	lastSchedule := now.Add(-30 * time.Minute)
	lastSuccess := now.Add(-35 * time.Minute)

	tests := []struct {
		name     string
		contains []string
		status   k8s.CronJobStatusEntry
	}{
		{
			name: "active cronjob",
			status: k8s.CronJobStatusEntry{
				Namespace:          "default",
				CronJobName:        "my-cronjob",
				Schedule:           "*/5 * * * *",
				ActiveJobs:         1,
				LastScheduleTime:   &lastSchedule,
				LastSuccessfulTime: &lastSuccess,
			},
			contains: []string{
				"CronJob Status: default/my-cronjob",
				"Schedule: */5 * * * *",
				"ActiveJobs: 1",
				"LastScheduled:",
				"LastSuccess:",
			},
		},
		{
			name: "suspended cronjob",
			status: k8s.CronJobStatusEntry{
				Namespace:   "default",
				CronJobName: "my-cronjob",
				Schedule:    "0 * * * *",
				Suspended:   true,
			},
			contains: []string{
				"suspended",
			},
		},
		{
			name: "missed schedule",
			status: k8s.CronJobStatusEntry{
				Namespace:      "default",
				CronJobName:    "my-cronjob",
				Schedule:       "*/5 * * * *",
				MissedSchedule: true,
			},
			contains: []string{
				"missed schedule detected",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			text := buildCronJobStatusEmbeddingText(&tt.status)
			for _, substr := range tt.contains {
				if !strings.Contains(text, substr) {
					t.Errorf("expected embedding text to contain %q, got:\n%s", substr, text)
				}
			}
		})
	}
}

func TestJobStatusWorker_Deduplication(t *testing.T) {
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

	// Add a Job entry.
	mockK8s.AddJobStatusEntry("default", &k8s.JobStatusEntry{
		Timestamp: time.Now(),
		Namespace: "default",
		JobName:   "my-job",
		Active:    1,
		Succeeded: 0,
		Failed:    0,
	})

	// Add a CronJob entry.
	mockK8s.AddCronJobStatusEntry("default", &k8s.CronJobStatusEntry{
		Timestamp:   time.Now(),
		Namespace:   "default",
		CronJobName: "my-cronjob",
		Schedule:    "*/5 * * * *",
		ActiveJobs:  0,
	})

	w := NewJobStatusWorker(mockK8s, dbMock, llmClient, 100*time.Millisecond, nsscope.Static(config.NamespaceSelector{Include: []string{"default"}}))

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()

	if err := w.Start(ctx); err != nil && err != context.DeadlineExceeded {
		t.Fatalf("unexpected start error: %v", err)
	}

	// The mock LLM should have been called exactly twice (once for Job, once for CronJob)
	// because the statuses haven't changed between polls, so dedup should kick in.
	mock, ok := llmClient.(*llm.MockClient)
	if !ok {
		t.Fatal("failed to cast to MockClient")
	}

	callCount := mock.GetEmbeddingCallCount()
	if callCount != 2 {
		t.Errorf("expected exactly 2 embedding calls (1 Job + 1 CronJob, dedup should prevent re-processing), got %d", callCount)
	}
}

package k8s

import (
	"time"

	batchv1 "k8s.io/api/batch/v1"
)

// JobConditionInfo captures a single Job condition.
type JobConditionInfo struct {
	// LastTransitionTime is when this condition last changed.
	LastTransitionTime time.Time
	// Type is the condition type (e.g., Complete, Failed, Suspended, FailureTarget, SuccessCriteriaMet).
	Type string
	// Status is True, False, or Unknown.
	Status string
	// Reason provides a machine-readable reason for the condition.
	Reason string
	// Message provides a human-readable explanation.
	Message string
}

// JobStatusEntry represents the status snapshot of a single Job.
type JobStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// StartTime is when the Job started.
	StartTime *time.Time
	// CompletionTime is when the Job completed.
	CompletionTime *time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// JobName is the name of the Job.
	JobName string
	// Conditions are the Job conditions.
	Conditions []JobConditionInfo
	// Duration is the elapsed time from start to completion (or now if still running).
	Duration time.Duration
	// Active is the number of actively running pods.
	Active int32
	// Succeeded is the number of pods that completed successfully.
	Succeeded int32
	// Failed is the number of pods that failed.
	Failed int32
	// DeadlineExceeded indicates whether the Job exceeded its activeDeadlineSeconds.
	DeadlineExceeded bool
}

// CronJobStatusEntry represents the status snapshot of a single CronJob.
type CronJobStatusEntry struct {
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// LastScheduleTime is when the CronJob was last scheduled.
	LastScheduleTime *time.Time
	// LastSuccessfulTime is when the CronJob last completed successfully.
	LastSuccessfulTime *time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// CronJobName is the name of the CronJob.
	CronJobName string
	// Schedule is the cron schedule expression.
	Schedule string
	// ActiveJobs is the number of currently active jobs.
	ActiveJobs int
	// Suspended indicates whether the CronJob is suspended.
	Suspended bool
	// MissedSchedule indicates whether a schedule was likely missed.
	MissedSchedule bool
}

// buildJobStatusEntry builds a JobStatusEntry from a Job object.
func buildJobStatusEntry(job *batchv1.Job, now time.Time) JobStatusEntry {
	entry := JobStatusEntry{
		Timestamp: now,
		Namespace: job.Namespace,
		JobName:   job.Name,
		Active:    job.Status.Active,
		Succeeded: job.Status.Succeeded,
		Failed:    job.Status.Failed,
	}

	if job.Status.StartTime != nil {
		t := job.Status.StartTime.Time
		entry.StartTime = &t
	}

	if job.Status.CompletionTime != nil {
		t := job.Status.CompletionTime.Time
		entry.CompletionTime = &t
	}

	// Calculate duration.
	if entry.StartTime != nil {
		if entry.CompletionTime != nil {
			entry.Duration = entry.CompletionTime.Sub(*entry.StartTime)
		} else {
			entry.Duration = now.Sub(*entry.StartTime)
		}
	}

	// Check for deadline exceeded condition.
	candidates := make([]time.Time, 0, len(job.Status.Conditions)+2)
	if entry.StartTime != nil {
		candidates = append(candidates, *entry.StartTime)
	}
	if entry.CompletionTime != nil {
		candidates = append(candidates, *entry.CompletionTime)
	}
	for _, cond := range job.Status.Conditions {
		entry.Conditions = append(entry.Conditions, JobConditionInfo{
			Type:               string(cond.Type),
			Status:             string(cond.Status),
			Reason:             cond.Reason,
			Message:            cond.Message,
			LastTransitionTime: cond.LastTransitionTime.Time,
		})
		candidates = append(candidates, cond.LastTransitionTime.Time)

		if cond.Type == batchv1.JobFailed && cond.Status == "True" && cond.Reason == "DeadlineExceeded" {
			entry.DeadlineExceeded = true
		}
	}

	entry.Timestamp = deterministicStatusTime(&job.ObjectMeta, now, candidates...)
	return entry
}

// buildCronJobStatusEntry builds a CronJobStatusEntry from a CronJob object.
func buildCronJobStatusEntry(cj *batchv1.CronJob, now time.Time) CronJobStatusEntry {
	entry := CronJobStatusEntry{
		Timestamp:   now,
		Namespace:   cj.Namespace,
		CronJobName: cj.Name,
		Schedule:    cj.Spec.Schedule,
		ActiveJobs:  len(cj.Status.Active),
	}

	if cj.Spec.Suspend != nil {
		entry.Suspended = *cj.Spec.Suspend
	}

	if cj.Status.LastScheduleTime != nil {
		t := cj.Status.LastScheduleTime.Time
		entry.LastScheduleTime = &t
	}

	if cj.Status.LastSuccessfulTime != nil {
		t := cj.Status.LastSuccessfulTime.Time
		entry.LastSuccessfulTime = &t
	}

	// Detect missed schedules: if lastScheduleTime is more than 2x the schedule interval ago,
	// it likely missed a schedule. We use a simple heuristic based on common cron intervals.
	if entry.LastScheduleTime != nil && !entry.Suspended {
		timeSinceLastSchedule := now.Sub(*entry.LastScheduleTime)
		// If more than 2 hours since last schedule, flag as potentially missed.
		// This is a conservative heuristic; parsing cron expressions would be more precise.
		if timeSinceLastSchedule > 2*time.Hour {
			entry.MissedSchedule = true
		}
	}

	candidates := make([]time.Time, 0, 2)
	if entry.LastScheduleTime != nil {
		candidates = append(candidates, *entry.LastScheduleTime)
	}
	if entry.LastSuccessfulTime != nil {
		candidates = append(candidates, *entry.LastSuccessfulTime)
	}
	entry.Timestamp = deterministicStatusTime(&cj.ObjectMeta, now, candidates...)
	return entry
}

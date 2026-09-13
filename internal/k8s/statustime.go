package k8s

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// deterministicStatusTime derives a stable timestamp for a status snapshot from
// the observed object itself rather than from wall-clock time.
//
// Why: unified_entries dedups on (content_hash, timestamp), and the status
// workers hash state only. If the timestamp were time.Now(), re-observing an
// unchanged object (every poll, and especially after a worker restart) would
// produce the same hash but a new timestamp and insert a duplicate row. Deriving
// the timestamp from the object's own transition times makes an unchanged object
// map to the same (content_hash, timestamp) pair, so InsertEntry dedupes it.
//
// It returns the most recent non-zero candidate (e.g. a condition's
// LastTransitionTime, a Job StartTime, an HPA LastScaleTime), falling back to the
// object's CreationTimestamp, and only as a last resort to fallback (ingest
// time). Real API objects always carry a CreationTimestamp, so the non-
// deterministic fallback is effectively never reached.
func deterministicStatusTime(meta *metav1.ObjectMeta, fallback time.Time, candidates ...time.Time) time.Time {
	latest := time.Time{}
	for _, t := range candidates {
		if t.After(latest) {
			latest = t
		}
	}
	if !latest.IsZero() {
		return latest
	}
	if meta != nil && !meta.CreationTimestamp.IsZero() {
		return meta.CreationTimestamp.Time
	}
	return fallback
}

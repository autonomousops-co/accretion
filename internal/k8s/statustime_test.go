package k8s

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var (
	tCreated = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tOld     = time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	tNew     = time.Date(2026, 7, 1, 8, 30, 0, 0, time.UTC)
)

func TestDeterministicStatusTime(t *testing.T) {
	meta := metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(tCreated)}
	fallback := time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)

	// Picks the most recent candidate, ignoring zero values and the fallback.
	got := deterministicStatusTime(&meta, fallback, tOld, time.Time{}, tNew)
	if !got.Equal(tNew) {
		t.Errorf("want max candidate %v, got %v", tNew, got)
	}

	// No candidates -> CreationTimestamp, never the fallback.
	got = deterministicStatusTime(&meta, fallback)
	if !got.Equal(tCreated) {
		t.Errorf("want creation %v, got %v", tCreated, got)
	}

	// No candidates and no creation -> fallback (real objects never hit this).
	got = deterministicStatusTime(&metav1.ObjectMeta{}, fallback)
	if !got.Equal(fallback) {
		t.Errorf("want fallback %v, got %v", fallback, got)
	}

	// nil meta (aggregated resources like endpoints have no owning object) is safe:
	// candidates still win, and with none it drops to the fallback without panicking.
	if got = deterministicStatusTime(nil, fallback, tOld, tNew); !got.Equal(tNew) {
		t.Errorf("nil meta with candidates: want %v, got %v", tNew, got)
	}
	if got = deterministicStatusTime(nil, fallback); !got.Equal(fallback) {
		t.Errorf("nil meta no candidates: want fallback %v, got %v", fallback, got)
	}
}

func TestBuildEndpointHealthEntry_TimestampIsDeterministic(t *testing.T) {
	now1 := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	now2 := now1.Add(90 * time.Minute)
	slices := []discoveryv1.EndpointSlice{
		{ObjectMeta: metav1.ObjectMeta{Name: "s1", CreationTimestamp: metav1.NewTime(tOld)}},
		{ObjectMeta: metav1.ObjectMeta{Name: "s2", CreationTimestamp: metav1.NewTime(tNew)}},
	}
	a := buildEndpointHealthEntry("dkjr", "svc", slices, now1)
	b := buildEndpointHealthEntry("dkjr", "svc", slices, now2)
	if !a.Timestamp.Equal(b.Timestamp) {
		t.Fatalf("timestamp not deterministic: %v vs %v", a.Timestamp, b.Timestamp)
	}
	if !a.Timestamp.Equal(tNew) {
		t.Errorf("want max slice creation %v, got %v", tNew, a.Timestamp)
	}
}

// The build functions must produce a timestamp derived from the object, not from
// the `now` they are handed — so two polls at different wall-clock times of an
// unchanged object yield the same (deduplicatable) timestamp.
func TestBuildStatusEntry_TimestampIsDeterministic(t *testing.T) {
	now1 := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	now2 := now1.Add(3 * time.Hour)

	t.Run("job uses max of conditions/start/completion", func(t *testing.T) {
		job := &batchv1.Job{
			ObjectMeta: metav1.ObjectMeta{Name: "j", Namespace: "dkjr", CreationTimestamp: metav1.NewTime(tCreated)},
			Status: batchv1.JobStatus{
				StartTime:      &metav1.Time{Time: tOld},
				CompletionTime: &metav1.Time{Time: tNew},
				Conditions: []batchv1.JobCondition{{
					Type: batchv1.JobComplete, Status: corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(tOld),
				}},
			},
		}
		a := buildJobStatusEntry(job, now1)
		b := buildJobStatusEntry(job, now2)
		if !a.Timestamp.Equal(b.Timestamp) {
			t.Fatalf("timestamp not deterministic: %v vs %v", a.Timestamp, b.Timestamp)
		}
		if !a.Timestamp.Equal(tNew) {
			t.Errorf("want completion time %v, got %v", tNew, a.Timestamp)
		}
	})

	t.Run("hpa uses max of conditions/lastScaleTime", func(t *testing.T) {
		hpa := &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: "h", Namespace: "dkjr", CreationTimestamp: metav1.NewTime(tCreated)},
			Spec:       autoscalingv2.HorizontalPodAutoscalerSpec{MaxReplicas: 5},
			Status: autoscalingv2.HorizontalPodAutoscalerStatus{
				LastScaleTime: &metav1.Time{Time: tNew},
				Conditions: []autoscalingv2.HorizontalPodAutoscalerCondition{{
					Type: autoscalingv2.AbleToScale, Status: corev1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(tOld),
				}},
			},
		}
		a := buildHPAEntry(hpa, now1)
		b := buildHPAEntry(hpa, now2)
		if !a.Timestamp.Equal(b.Timestamp) {
			t.Fatalf("timestamp not deterministic: %v vs %v", a.Timestamp, b.Timestamp)
		}
		if !a.Timestamp.Equal(tNew) {
			t.Errorf("want lastScaleTime %v, got %v", tNew, a.Timestamp)
		}
	})

	t.Run("cronjob with no schedule times falls back to creation", func(t *testing.T) {
		cj := &batchv1.CronJob{
			ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "dkjr", CreationTimestamp: metav1.NewTime(tCreated)},
			Spec:       batchv1.CronJobSpec{Schedule: "* * * * *"},
		}
		a := buildCronJobStatusEntry(cj, now1)
		b := buildCronJobStatusEntry(cj, now2)
		if !a.Timestamp.Equal(b.Timestamp) {
			t.Fatalf("timestamp not deterministic: %v vs %v", a.Timestamp, b.Timestamp)
		}
		if !a.Timestamp.Equal(tCreated) {
			t.Errorf("want creation %v, got %v", tCreated, a.Timestamp)
		}
	})
}

func TestParseLogLine(t *testing.T) {
	// kubelet prefixes each line with an RFC3339Nano timestamp and a space.
	line := "2026-07-12T10:00:00.123456789Z /health 200 OK"
	ts, msg := parseLogLine(line)
	want := time.Date(2026, 7, 12, 10, 0, 0, 123456789, time.UTC)
	if !ts.Equal(want) {
		t.Errorf("want parsed ts %v, got %v", want, ts)
	}
	if msg != "/health 200 OK" {
		t.Errorf("want stripped message, got %q", msg)
	}

	// Two occurrences of an identical message at different real times keep their
	// distinct timestamps -> distinct rows (no silent drop of repeated logs).
	ts2, _ := parseLogLine("2026-07-12T10:00:05.000000000Z /health 200 OK")
	if ts.Equal(ts2) {
		t.Error("distinct-time occurrences must not collapse to the same timestamp")
	}

	// A line without a parseable prefix is returned verbatim as the message.
	badLine := "no-timestamp-prefix here"
	_, msg = parseLogLine(badLine)
	if msg != badLine {
		t.Errorf("unparseable line should be kept whole, got %q", msg)
	}
}

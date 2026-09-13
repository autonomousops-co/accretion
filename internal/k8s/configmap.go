package k8s

import (
	"time"

	corev1 "k8s.io/api/core/v1"
)

// What a ConfigMap or Secret record carries: name, namespace, resourceVersion,
// creation time, labels, and the annotations SafeAnnotations admits. Never
// .data, and never an annotation that could carry it.

// safeAnnotationKeys is the allowlist of annotations a ConfigMap (or any other
// kind's) status record may carry. Annotations are where tooling stamps whole
// object bodies: kubectl's last-applied-configuration holds the applied
// object -- for a Secret, .data base64 and .stringData in plaintext -- and
// kapp, Rancher and others stamp similar blobs. Rather than chase every tool,
// a record keeps only annotations known to be metadata, and only when short.
// Everything else is dropped, not truncated: a truncated body still leaks.
var safeAnnotationKeys = map[string]bool{
	"kubernetes.io/change-cause":        true,
	"meta.helm.sh/release-name":         true,
	"meta.helm.sh/release-namespace":    true,
	"helm.sh/hook":                      true,
	"helm.sh/hook-weight":               true,
	"helm.sh/hook-delete-policy":        true,
	"helm.sh/resource-policy":           true,
	"argocd.argoproj.io/tracking-id":    true,
	"argocd.argoproj.io/sync-wave":      true,
	"argocd.argoproj.io/sync-options":   true,
	"deployment.kubernetes.io/revision": true,
}

// maxSafeAnnotationValue bounds an admitted annotation's value. Metadata is
// short; an object body is not.
const maxSafeAnnotationValue = 256

// SafeAnnotations returns the subset of in that a ConfigMap status record may
// carry, or nil when nothing qualifies. It is applied three times on purpose:
// by the informer store transform (nothing else is ever resident), by the
// entry conversion (nothing else reaches an entry), and by the configmap
// worker as it builds the row (nothing else is persisted, whichever path fed
// it). A kubectl-applied ConfigMap's last-applied-configuration annotation
// carries that ConfigMap's own data, which is why this still matters with
// Secrets out of scope.
func SafeAnnotations(in map[string]string) map[string]string {
	return filterAnnotations(in, safeAnnotationKeys)
}

func filterAnnotations(in map[string]string, allowed map[string]bool) map[string]string {
	var out map[string]string
	for k, v := range in {
		if !allowed[k] || len(v) > maxSafeAnnotationValue {
			continue
		}
		if out == nil {
			out = make(map[string]string)
		}
		out[k] = v
	}
	return out
}

// ConfigMapEntry represents the metadata snapshot of a single ConfigMap.
// SECURITY: Only metadata is tracked. No data values are stored, and
// annotations pass through SafeAnnotations.
type ConfigMapEntry struct {
	// Labels are the resource labels.
	Labels map[string]string
	// Annotations are the resource annotations.
	Annotations map[string]string
	// Timestamp is when the status was captured.
	Timestamp time.Time
	// CreationTimestamp is when the resource was created.
	CreationTimestamp time.Time
	// Namespace is the Kubernetes namespace.
	Namespace string
	// Name is the name of the ConfigMap.
	Name string
	// ResourceVersion is the Kubernetes resource version.
	ResourceVersion string
}

// configMapEntry converts one ConfigMap; shared by the List and informer paths (#96).
func configMapEntry(cm *corev1.ConfigMap, now time.Time) ConfigMapEntry {
	entry := ConfigMapEntry{
		Timestamp:         now,
		Namespace:         cm.Namespace,
		Name:              cm.Name,
		ResourceVersion:   cm.ResourceVersion,
		CreationTimestamp: cm.CreationTimestamp.Time,
	}

	if len(cm.Labels) > 0 {
		entry.Labels = make(map[string]string, len(cm.Labels))
		for k, v := range cm.Labels {
			entry.Labels[k] = v
		}
	}

	entry.Annotations = SafeAnnotations(cm.Annotations)

	// No conditions; the immutable creation time is the deterministic stamp so
	// an unchanged ConfigMap dedupes across observations/restarts.
	entry.Timestamp = deterministicStatusTime(&cm.ObjectMeta, now)
	return entry
}

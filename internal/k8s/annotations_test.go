package k8s

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// A kubectl-applied Secret carries its whole applied body -- .data base64 and
// .stringData in plaintext -- in the last-applied-configuration annotation.
// These tests hold the line at every layer: the store transform, the entry
// conversion, and SafeAnnotations itself.

const (
	leakB64   = "aHVudGVyMg=="
	leakPlain = "hunter2-plaintext"
)

var lastApplied = `{"apiVersion":"v1","kind":"Secret","metadata":{"name":"db","namespace":"prod"},` +
	`"type":"Opaque","data":{"token":"` + leakB64 + `"},"stringData":{"password":"` + leakPlain + `"}}`

func mustNotLeak(t *testing.T, what string, v interface{}) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %s: %v", what, err)
	}
	for _, needle := range []string{leakB64, leakPlain, "last-applied-configuration"} {
		if strings.Contains(string(b), needle) {
			t.Fatalf("%s carries %q: %s", what, needle, b)
		}
	}
}

func TestSafeAnnotations(t *testing.T) {
	in := map[string]string{
		"kubectl.kubernetes.io/last-applied-configuration": lastApplied,
		"kubernetes.io/change-cause":                       "kubectl apply",
		"meta.helm.sh/release-name":                        "accretion",
		"helm.sh/hook":                                     strings.Repeat("x", maxSafeAnnotationValue+1),
		"example.com/anything":                             "unknown tooling",
	}
	got := SafeAnnotations(in)
	want := map[string]string{"kubernetes.io/change-cause": "kubectl apply", "meta.helm.sh/release-name": "accretion"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if SafeAnnotations(nil) != nil || SafeAnnotations(map[string]string{"example.com/x": "y"}) != nil {
		t.Fatal("nothing admitted must be nil, not an empty map (the record's JSON differs)")
	}
}

func TestConfigMapInformer_LastAppliedAnnotationNeverLeaves(t *testing.T) {
	quietLogger(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "prod", ResourceVersion: "3", CreationTimestamp: created,
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": `{"data":{"DB_PASSWORD":"` + leakPlain + `"}}`,
			}},
		Data: map[string]string{"DB_PASSWORD": leakPlain},
	}
	f := &fakeLW{list: func() runtime.Object {
		return &corev1.ConfigMapList{ListMeta: metav1.ListMeta{ResourceVersion: "3"}, Items: []corev1.ConfigMap{*cm}}
	}}
	got := make(chan ConfigMapEntry, 16)
	inf := newConfigMapInformer(f.lw(), time.Hour, func(obj interface{}) {
		if c, ok := obj.(*corev1.ConfigMap); ok {
			got <- configMapEntry(c, time.Now())
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = runStatusInformers(ctx, inf) }()

	mustNotLeak(t, "the ConfigMapEntry", recv(t, got, 5*time.Second))
	mustNotLeak(t, "the stored ConfigMap", inf.informer.GetStore().List()[0])
	mustNotLeak(t, "configMapEntry of a raw ConfigMap", configMapEntry(cm, time.Now()))
}

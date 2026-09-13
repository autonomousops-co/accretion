package worker

import (
	"reflect"
	"sort"
	"testing"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/nsscope"
)

func TestBaseWorker_VerifyNamespaces(t *testing.T) {
	configuredNamespaces := []string{"ns1", "ns2", "ns3"}
	worker := NewBaseWorker("test", nil, nsscope.Static(config.NamespaceSelector{Include: configuredNamespaces}))

	// Initially, all namespaces should be missing
	missing := worker.VerifyNamespaces(worker.scope.Namespaces())
	sort.Strings(missing)
	expectedMissing := []string{"ns1", "ns2", "ns3"}
	if !reflect.DeepEqual(missing, expectedMissing) {
		t.Errorf("expected missing namespaces %v, got %v", expectedMissing, missing)
	}

	// Record data for ns1
	worker.stats.RecordLogIngestion("ns1")
	missing = worker.VerifyNamespaces(worker.scope.Namespaces())
	sort.Strings(missing)
	expectedMissing = []string{"ns2", "ns3"}
	if !reflect.DeepEqual(missing, expectedMissing) {
		t.Errorf("expected missing namespaces %v, got %v", expectedMissing, missing)
	}

	// Record data for ns2
	worker.stats.RecordMetricIngestion("ns2")
	missing = worker.VerifyNamespaces(worker.scope.Namespaces())
	sort.Strings(missing)
	expectedMissing = []string{"ns3"}
	if !reflect.DeepEqual(missing, expectedMissing) {
		t.Errorf("expected missing namespaces %v, got %v", expectedMissing, missing)
	}

	// Record data for ns3
	worker.stats.RecordLogIngestion("ns3")
	missing = worker.VerifyNamespaces(worker.scope.Namespaces())
	if len(missing) > 0 {
		t.Errorf("expected no missing namespaces, got %v", missing)
	}
}

func TestBaseWorker_VerifyNamespaces_Extra(t *testing.T) {
	// configured only ns1, but receiving stats for ns2 should not affect verification of configured ns1
	configuredNamespaces := []string{"ns1"}
	worker := NewBaseWorker("test", nil, nsscope.Static(config.NamespaceSelector{Include: configuredNamespaces}))

	worker.stats.RecordLogIngestion("ns2")

	missing := worker.VerifyNamespaces(worker.scope.Namespaces())
	expectedMissing := []string{"ns1"}
	if !reflect.DeepEqual(missing, expectedMissing) {
		t.Errorf("expected missing namespaces %v, got %v", expectedMissing, missing)
	}
}

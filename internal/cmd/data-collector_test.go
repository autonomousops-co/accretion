package cmd

import (
	"reflect"
	"testing"

	"github.com/autonomousops-co/accretion/internal/config"
	"github.com/autonomousops-co/accretion/internal/testutil"
)

func toggle(v bool) config.CollectorToggle { return config.CollectorToggle{Enabled: &v} }

func TestSelectedDataCollectorsDefaultsToAll(t *testing.T) {
	testutil.SetupTestLogger(t)

	// The zero value is what a config written before these keys existed
	// unmarshals into. It must keep collecting everything.
	got, err := selectedDataCollectors(&config.CollectorsConfig{}, nil)
	if err != nil {
		t.Fatalf("selectedDataCollectors: %v", err)
	}
	want := []string{"log", "metric", "event", "trace"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectedDataCollectorsHonoursToggles(t *testing.T) {
	testutil.SetupTestLogger(t)

	cfg := &config.CollectorsConfig{
		Log:    toggle(true),
		Metric: toggle(false),
		Event:  toggle(false),
		Trace:  toggle(true),
	}
	got, err := selectedDataCollectors(cfg, nil)
	if err != nil {
		t.Fatalf("selectedDataCollectors: %v", err)
	}
	if want := []string{"log", "trace"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// TestSelectedDataCollectorsOnlyOverridesToggles is the split escape hatch of
// #92: a collector split into its own deployment must run exactly what it was
// split out for, regardless of what the shared ConfigMap still says.
func TestSelectedDataCollectorsOnlyOverridesToggles(t *testing.T) {
	testutil.SetupTestLogger(t)

	// Every collector is enabled in config, but --only log must still yield log
	// alone -- otherwise a split-out log deployment would quietly also run the
	// metric collector that the shared process is already running.
	cfg := &config.CollectorsConfig{
		Log:    toggle(true),
		Metric: toggle(true),
		Event:  toggle(true),
		Trace:  toggle(true),
	}
	got, err := selectedDataCollectors(cfg, []string{"log"})
	if err != nil {
		t.Fatalf("selectedDataCollectors: %v", err)
	}
	if want := []string{"log"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}

	// --only also wins over an explicit false, so a split deployment cannot be
	// rendered inert by the shared toggle the chart sets to remove it from the
	// shared process.
	cfg.Log = toggle(false)
	got, err = selectedDataCollectors(cfg, []string{"log"})
	if err != nil {
		t.Fatalf("selectedDataCollectors: %v", err)
	}
	if want := []string{"log"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestSelectedDataCollectorsRejectsUnknownName(t *testing.T) {
	testutil.SetupTestLogger(t)

	if _, err := selectedDataCollectors(&config.CollectorsConfig{}, []string{"logs"}); err == nil {
		t.Fatal("expected an error for an unknown collector name")
	}
}

func TestSelectedDataCollectorsDeduplicates(t *testing.T) {
	testutil.SetupTestLogger(t)

	got, err := selectedDataCollectors(&config.CollectorsConfig{}, []string{"log", "log", "metric"})
	if err != nil {
		t.Fatalf("selectedDataCollectors: %v", err)
	}
	// A duplicate must not produce two units of the same name, which the
	// supervisor rejects outright.
	if want := []string{"log", "metric"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestDataCollectorServiceName(t *testing.T) {
	cases := []struct {
		want string
		only []string
	}{
		{"accretion-data-collector", nil},
		{"accretion-log-collector", []string{"log"}},
		{"accretion-log-metric-collector", []string{"metric", "log"}},
	}
	for _, tc := range cases {
		if got := dataCollectorServiceName(tc.only); got != tc.want {
			t.Fatalf("dataCollectorServiceName(%v) = %q, want %q", tc.only, got, tc.want)
		}
	}
}

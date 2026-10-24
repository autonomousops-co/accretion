package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/autonomousops-co/accretion/internal/config"
)

// TestWriteOmittedSecretsNotice asserts `accretion generate config` names every
// secret it deliberately did not write, and the variable that supplies it
// (#129).
//
// It is driven off config.OmittedSecrets rather than a literal expected string,
// because the point of the notice is that it stays in step with what
// GenerateDefault actually omits. A hardcoded expectation would keep passing
// while a newly omitted secret went unmentioned, which is the one failure that
// matters here: an operator who is not told a value is missing discovers it
// from a failed startup instead.
func TestWriteOmittedSecretsNotice(t *testing.T) {
	if len(config.OmittedSecrets) == 0 {
		t.Fatal("config.OmittedSecrets is empty, so this test would assert nothing")
	}

	var buf bytes.Buffer
	writeOmittedSecretsNotice(&buf)
	got := buf.String()

	for _, secret := range config.OmittedSecrets {
		if !strings.Contains(got, secret.Key) {
			t.Errorf("notice must name the omitted key %q, got:\n%s", secret.Key, got)
		}
		if !strings.Contains(got, secret.EnvVar) {
			t.Errorf("notice must name %q as the source of %s, got:\n%s", secret.EnvVar, secret.Key, got)
		}
	}

	// Both provenances, because they are the actual question an operator has:
	// the answer differs between a laptop and a cluster.
	for _, want := range []string{".env", "Kubernetes Secret"} {
		if !strings.Contains(got, want) {
			t.Errorf("notice must say where secrets come from (%q), got:\n%s", want, got)
		}
	}
}

// TestWriteOmittedSecretsNoticeLeaksNoValue guards the obvious way this notice
// could go wrong: printing a secret while explaining that secrets are not
// printed. Nothing in writeOmittedSecretsNotice reads a value today, and this
// keeps it that way by checking the notice against values the environment
// would supply.
func TestWriteOmittedSecretsNoticeLeaksNoValue(t *testing.T) {
	const sentinel = "sentinel-secret-value-do-not-print"
	for _, secret := range config.OmittedSecrets {
		t.Setenv(secret.EnvVar, sentinel)
	}

	var buf bytes.Buffer
	writeOmittedSecretsNotice(&buf)

	if strings.Contains(buf.String(), sentinel) {
		t.Errorf("notice printed a secret value from the environment, got:\n%s", buf.String())
	}
}

package main

import "testing"

func TestVersionStringPrefersStampedVersion(t *testing.T) {
	old := version
	defer func() { version = old }()

	version = "1.2.3"
	if got := versionString(); got != "1.2.3" {
		t.Errorf("versionString() = %q, want the stamped version", got)
	}

	// Unstamped builds still report something; the exact value depends on
	// how the test binary was built.
	version = ""
	if got := versionString(); got == "" {
		t.Error("versionString() is empty for an unstamped build")
	}
}

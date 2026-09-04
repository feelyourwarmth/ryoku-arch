package doctor

import (
	"path/filepath"
	"testing"
)

func TestProvisionSkipsWhatTheUserRemoved(t *testing.T) {
	file := filepath.Join(t.TempDir(), "provisioned")
	provisionedFile = func() string { return file }

	installs := 0
	install := func() bool { installs++; return true }

	present, skipped := provision("ryoku-test-pkg", install)
	if !present || skipped || installs != 1 {
		t.Fatalf("first provisioning: present=%v skipped=%v installs=%d", present, skipped, installs)
	}
	if !provisioned()["ryoku-test-pkg"] {
		t.Fatal("a provisioned package must be recorded")
	}

	present, skipped = provision("ryoku-test-pkg", install)
	if present || !skipped || installs != 1 {
		t.Fatalf("a recorded package that is absent again was removed by the user: present=%v skipped=%v installs=%d", present, skipped, installs)
	}
	if !removedByUser("ryoku-test-pkg") || removedByUser("never-seen") {
		t.Fatal("removedByUser must be true only for a recorded, absent package")
	}
}

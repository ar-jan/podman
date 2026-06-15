//go:build !remote && linux && !systemd && runit

package libpod

import (
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

func TestRunitRunScriptQuotesArguments(t *testing.T) {
	script := runitRunScript([]string{"/usr/bin/podman", "--root", "/tmp/has space", "healthcheck", "run-loop", "quote'arg"})
	expected := "#!/bin/sh\nexec 2>&1\nexec '/usr/bin/podman' '--root' '/tmp/has space' 'healthcheck' 'run-loop' 'quote'\"'\"'arg'\n"
	if script != expected {
		t.Fatalf("unexpected script:\n%s", script)
	}
}

func TestRunitFinishScriptQuotesServiceDir(t *testing.T) {
	script := runitFinishScript("/tmp/has space/quote'dir")
	expected := "#!/bin/sh\nif [ -e ./podman-stop ]; then\n    cd /\n    rm -rf '/tmp/has space/quote'\"'\"'dir'\nfi\nexit 0\n"
	if script != expected {
		t.Fatalf("unexpected script:\n%s", script)
	}
}

func TestRunitSupervisorActive(t *testing.T) {
	serviceDir := t.TempDir()
	superviseDir := filepath.Join(serviceDir, "supervise")
	if err := unix.Mkdir(superviseDir, 0o700); err != nil {
		t.Fatal(err)
	}

	if runitSupervisorActive(serviceDir) {
		t.Fatal("service must not be active without a control pipe")
	}

	if err := unix.Mkfifo(filepath.Join(superviseDir, "control"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !runitSupervisorActive(serviceDir) {
		t.Fatal("service must be active with a control pipe")
	}
}

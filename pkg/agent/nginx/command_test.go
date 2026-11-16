package nginx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCommandUsesManagedConfigAndReportsDiagnostics(t *testing.T) {
	home := t.TempDir() + "/"
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" >&2\necho invalid-config >&2\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{nginx: bin, nginxHome: home}
	err := a.PreflightCheck()
	for _, expected := range []string{"invalid-config", "-p\n" + home, "-c\n" + home + "nginx.conf"} {
		if err == nil || !strings.Contains(err.Error(), expected) {
			t.Fatalf("missing %q: %v", expected, err)
		}
	}
}

func TestCommandTimeout(t *testing.T) {
	home := t.TempDir() + "/"
	bin := filepath.Join(home, "nginx-test")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nexec sleep 10\n"), 0700); err != nil {
		t.Fatal(err)
	}
	a := &Agent{nginx: bin, nginxHome: home, commandTimeout: 30 * time.Millisecond}
	if err := a.PreflightCheck(); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline, got %v", err)
	}
}

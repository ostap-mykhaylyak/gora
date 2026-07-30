package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ostap-mykhaylyak/gora/internal/config"
)

// writeDefaultConfig materialises the embedded template so it can be loaded
// like a real installation would.
func writeDefaultConfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, defaultConfig, 0o600); err != nil {
		t.Fatalf("writing the template: %v", err)
	}
	return path
}

// The template `--init` installs must always load: it is the first file
// every user runs gora with, and a stale key in it would greet them with a
// parse error.
func TestDefaultConfigTemplateIsValid(t *testing.T) {
	cfg, err := config.Load(writeDefaultConfig(t))
	if err != nil {
		t.Fatalf("the embedded config template does not load: %v", err)
	}
	if cfg.Listen.Address == "" || cfg.Backend.Address == "" {
		t.Fatalf("the template left required fields empty: %+v", cfg)
	}
}

// install rewrites the unit's ExecStart by substituting the default config
// path; if the unit stops mentioning it, --config would silently stop
// reaching the service.
func TestServiceUnitCarriesTheDefaultConfigPath(t *testing.T) {
	if !strings.Contains(serviceUnit, defaultConfigPath) {
		t.Fatalf("gora.service does not mention %s, --config would be ignored", defaultConfigPath)
	}
	if !strings.Contains(serviceUnit, binaryPath+" start") {
		t.Fatalf("gora.service does not start %s with the start command", binaryPath)
	}
}

// logrotate must point at the file gora actually writes.
func TestLogrotateMatchesTheLogFile(t *testing.T) {
	want := logDir + "/" + logFileName
	if !strings.Contains(string(logrotateConf), want) {
		t.Fatalf("gora.logrotate does not rotate %s", want)
	}
}

func TestRunVersion(t *testing.T) {
	var out, errOut bytes.Buffer
	if code := run([]string{"--version"}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.HasPrefix(out.String(), "gora ") {
		t.Fatalf("version output = %q", out.String())
	}
}

func TestRunHelpAndBareInvocation(t *testing.T) {
	for _, args := range [][]string{{"--help"}, nil} {
		var out, errOut bytes.Buffer
		if code := run(args, &out, &errOut); code != 0 {
			t.Fatalf("run(%q) exit code = %d, want 0", args, code)
		}
		if !strings.Contains(out.String(), "Commands (no dashes):") {
			t.Fatalf("run(%q) did not print the usage: %q", args, out.String())
		}
	}
}

func TestRunRejectsSingleDashOptions(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"-config", "/tmp/x.yaml"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
	if !strings.Contains(errOut.String(), "--config") {
		t.Fatalf("stderr does not suggest --config: %q", errOut.String())
	}
}

func TestRunCheckConfig(t *testing.T) {
	path := writeDefaultConfig(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"--check-config", "--config", path}, &out, &errOut); code != 0 {
		t.Fatalf("exit code = %d, want 0 (stderr: %s)", code, errOut.String())
	}
	if !strings.Contains(out.String(), "is valid") {
		t.Fatalf("--check-config output = %q", out.String())
	}
}

func TestRunCheckConfigFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(path, []byte("listen:\n  addres: \"0.0.0.0:3306\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var out, errOut bytes.Buffer
	if code := run([]string{"--check-config", "--config", path}, &out, &errOut); code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(errOut.String(), "addres") {
		t.Fatalf("the error does not name the unknown key: %q", errOut.String())
	}
}

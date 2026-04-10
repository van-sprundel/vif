package scripts

import (
	"bytes"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/van-sprundel/vif/internal/composer"
)

func TestRunShellCommand(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"echo hello"},
	}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("output = %q, want to contain 'hello'", buf.String())
	}
}

func TestRunMultipleHandlers(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"echo first", "echo second"},
	}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "first") || !strings.Contains(out, "second") {
		t.Errorf("output = %q, want both 'first' and 'second'", out)
	}
}

func TestRunScriptReference(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"@helper"},
		"helper":           {"echo referenced"},
	}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "referenced") {
		t.Errorf("output = %q, want 'referenced'", buf.String())
	}
}

func TestRunCircularReference(t *testing.T) {
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"a": {"@b"},
		"b": {"@a"},
	}, nil, &buf)

	err := r.Run("a")
	if err == nil {
		t.Fatal("expected circular reference error")
	}
	if !strings.Contains(err.Error(), "circular") {
		t.Errorf("error = %q, want to mention 'circular'", err.Error())
	}
}

func TestRunPutenv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	key := "VIF_TEST_PUTENV_" + t.Name()
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {
			"@putenv " + key + "=hello",
			"echo $" + key,
		},
	}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Clean up env.
	defer os.Unsetenv(key)

	if !strings.Contains(buf.String(), "hello") {
		t.Errorf("output = %q, want 'hello' from env", buf.String())
	}
}

func TestRunNoScriptsEvent(t *testing.T) {
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("expected no output for empty event, got %q", buf.String())
	}
}

func TestRunUnknownReference(t *testing.T) {
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"@nonexistent"},
	}, nil, &buf)

	err := r.Run("post-install-cmd")
	if err == nil {
		t.Fatal("expected error for unknown reference")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error = %q, want 'not found'", err.Error())
	}
}

func TestRunFailingCommandWarns(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"false"},
	}, nil, &buf)

	err := r.Run("post-install-cmd")
	if err != nil {
		t.Fatalf("Run should not return error for failing command, got: %v", err)
	}
	if !strings.Contains(buf.String(), "Warning") {
		t.Errorf("output = %q, want warning about failed command", buf.String())
	}
}

func TestRunComposerCommandSkipped(t *testing.T) {
	var buf bytes.Buffer
	r := New(t.TempDir(), composer.Scripts{
		"post-install-cmd": {"@composer dump-autoload"},
	}, nil, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(buf.String(), "Skipping @composer") {
		t.Errorf("output = %q, want skip message", buf.String())
	}
}

func TestRunAutoScripts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	var buf bytes.Buffer
	autoScripts := map[string]string{
		"cache:clear":    "symfony-cmd",
		"echo from-auto": "script",
	}
	r := New(t.TempDir(), composer.Scripts{}, autoScripts, &buf)

	if err := r.Run("post-install-cmd"); err != nil {
		t.Fatalf("Run: %v", err)
	}

	out := buf.String()
	// Check that it attempted the symfony-cmd.
	if !strings.Contains(out, "php bin/console cache:clear") {
		t.Errorf("output = %q, want attempt at 'php bin/console cache:clear'", out)
	}
}

func TestAutoScriptsNotRunForOtherEvents(t *testing.T) {
	var buf bytes.Buffer
	autoScripts := map[string]string{
		"cache:clear": "symfony-cmd",
	}
	r := New(t.TempDir(), composer.Scripts{
		"post-autoload-dump": {"echo autoload"},
	}, autoScripts, &buf)

	// Auto-scripts should NOT run for post-autoload-dump.
	if runtime.GOOS == "windows" {
		t.Skip("shell tests require unix")
	}
	if err := r.Run("post-autoload-dump"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if strings.Contains(buf.String(), "cache:clear") {
		t.Errorf("auto-scripts should not run for post-autoload-dump, got %q", buf.String())
	}
}

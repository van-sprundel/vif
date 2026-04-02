package composerauth

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestApplyRequestUsesGitLabTokenForArchives(t *testing.T) {
	cfg := &Config{
		GitLabToken: map[string]string{"gitlab.com": "token-123"},
	}

	req, err := http.NewRequest(http.MethodGet, "https://gitlab.com/api/v4/projects/1/packages/composer/archives/acme/pkg.zip?sha=abc", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	cfg.ApplyRequest(req)

	if got := req.Header.Get("Private-Token"); got != "token-123" {
		t.Fatalf("Private-Token = %q, want token-123", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Fatalf("Authorization = %q, want empty", got)
	}
}

func TestApplyRequestUsesBearerFallback(t *testing.T) {
	cfg := &Config{
		Bearer: map[string]string{"repo.example.com": "bearer-123"},
	}

	req, err := http.NewRequest(http.MethodGet, "https://repo.example.com/files/pkg.zip", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	cfg.ApplyRequest(req)

	if got := req.Header.Get("Authorization"); got != "Bearer bearer-123" {
		t.Fatalf("Authorization = %q, want bearer header", got)
	}
}

func TestLoadReadsAuthFilesAndEnv(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "config")
	if err := os.MkdirAll(filepath.Join(configDir, "composer"), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	authFile := filepath.Join(configDir, "composer", "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"gitlab-token":{"gitlab.com":"file-token"}}`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", configDir)
	t.Setenv("COMPOSER_HOME", "")
	t.Setenv("COMPOSER_AUTH", `{"bearer":{"repo.example.com":"env-token"}}`)

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config")
	}
	if got := cfg.GitLabToken["gitlab.com"]; got != "file-token" {
		t.Fatalf("gitlab token = %q, want file-token", got)
	}
	if got := cfg.Bearer["repo.example.com"]; got != "env-token" {
		t.Fatalf("bearer token = %q, want env-token", got)
	}
}

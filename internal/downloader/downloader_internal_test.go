package downloader

import (
	"strings"
	"testing"
)

func TestClassifyFetchStatusPrivateArchive(t *testing.T) {
	err := classifyFetchStatus("https://gitlab.com/api/v4/projects/55230020/packages/composer/archives/vendor/pkg.zip?sha=abc", 401)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "authenticated package archives are not supported yet") {
		t.Fatalf("got %q, want auth support hint", err)
	}
}

func TestClassifyFetchStatusGenericUnauthorized(t *testing.T) {
	err := classifyFetchStatus("https://example.com/packages/vendor/pkg.zip", 401)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "authenticated package archives are not supported yet") {
		t.Fatalf("got %q, did not want private archive hint", err)
	}
}

package downloader

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
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

func TestExtractTarGz(t *testing.T) {
	data := makeTarGz(t, map[string]string{
		"src/Foo.php": "<?php class Foo {}",
	})

	dest := t.TempDir()
	if err := extractTar(data, dest, tarGzip); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "src", "Foo.php"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "<?php class Foo {}" {
		t.Errorf("content = %q, want %q", got, "<?php class Foo {}")
	}
}

func TestExtractTarGzStripsPrefix(t *testing.T) {
	data := makeTarGzWithPrefix(t, "vendor-pkg-abc123/", map[string]string{
		"src/Foo.php":   "<?php class Foo {}",
		"composer.json": `{"name":"vendor/pkg"}`,
	})

	dest := t.TempDir()
	if err := extractTar(data, dest, tarGzip); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "src", "Foo.php"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "<?php class Foo {}" {
		t.Errorf("content = %q, want %q", got, "<?php class Foo {}")
	}

	if _, err := os.Stat(filepath.Join(dest, "vendor-pkg-abc123")); err == nil {
		t.Error("top-level wrapper directory should have been stripped")
	}
}

func TestExtractTarBz2(t *testing.T) {
	data := makeTarPlain(t, map[string]string{
		"src/Bar.php": "<?php class Bar {}",
	})

	dest := t.TempDir()
	if err := extractTar(data, dest, tarUncompressed); err != nil {
		t.Fatalf("extractTar: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(dest, "src", "Bar.php"))
	if err != nil {
		t.Fatalf("read extracted file: %v", err)
	}
	if string(got) != "<?php class Bar {}" {
		t.Errorf("content = %q, want %q", got, "<?php class Bar {}")
	}
}

func TestExtractArchiveUnsupportedType(t *testing.T) {
	err := extractArchive([]byte{}, "rar", t.TempDir())
	if err == nil {
		t.Fatal("expected error for unsupported type")
	}
	if !strings.Contains(err.Error(), "unsupported archive type") {
		t.Errorf("got %q, want unsupported archive type hint", err)
	}
}

func TestIsRetryableStatus(t *testing.T) {
	tests := []struct {
		code int
		want bool
	}{
		{200, false},
		{400, false},
		{401, false},
		{403, false},
		{404, false},
		{429, true},
		{500, true},
		{502, true},
		{503, true},
		{504, true},
	}
	for _, tt := range tests {
		if got := isRetryableStatus(tt.code); got != tt.want {
			t.Errorf("isRetryableStatus(%d) = %v, want %v", tt.code, got, tt.want)
		}
	}
}

func makeTarGz(t *testing.T, files map[string]string) []byte {
	t.Helper()
	return makeTarGzWithPrefix(t, "", files)
}

func makeTarGzWithPrefix(t *testing.T, prefix string, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if prefix != "" {
		if err := tw.WriteHeader(&tar.Header{
			Name:     prefix,
			Mode:     0o755,
			Typeflag: tar.TypeDir,
		}); err != nil {
			t.Fatalf("tar header dir %q: %v", prefix, err)
		}
	}
	for name, content := range files {
		fullName := prefix + name
		if err := tw.WriteHeader(&tar.Header{
			Name: fullName,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("tar header %q: %v", fullName, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %q: %v", fullName, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

func makeTarPlain(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for name, content := range files {
		if err := tw.WriteHeader(&tar.Header{
			Name: name,
			Mode: 0o644,
			Size: int64(len(content)),
		}); err != nil {
			t.Fatalf("tar header %q: %v", name, err)
		}
		if _, err := tw.Write([]byte(content)); err != nil {
			t.Fatalf("tar write %q: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("tar close: %v", err)
	}
	return buf.Bytes()
}

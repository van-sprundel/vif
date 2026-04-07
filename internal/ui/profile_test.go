package ui

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestProgressVerboseIncludesElapsedTimestamp(t *testing.T) {
	var out bytes.Buffer
	progress := NewProgress(&out, "Resolving", 0, true)
	progress.start = time.Now().Add(-1250 * time.Millisecond)

	progress.Increment("acme/foo")
	progress.Error("  Solving... last=acme/foo states=42")

	s := out.String()
	assertContains(t, s, "[1.25s] Resolving acme/foo")
	assertContains(t, s, "[1.25s]   Solving... last=acme/foo states=42")
}

func TestPrintProfile(t *testing.T) {
	var out bytes.Buffer
	PrintProfile(&out, 5*time.Second, []ProfileSection{
		{Name: "resolve", Duration: 1200 * time.Millisecond},
		{Name: "install", Duration: 800 * time.Millisecond},
		{Name: "download", Duration: 3 * time.Second},
	}, []ProfilePackage{
		{Name: "a/pkg", Duration: 900 * time.Millisecond},
		{Name: "z/pkg", Duration: 1800 * time.Millisecond},
		{Name: "m/pkg", Duration: 1800 * time.Millisecond},
	})

	s := out.String()
	assertContains(t, s, "Profile summary")
	assertContains(t, s, "total: 5.00s")

	downloadIdx := strings.Index(s, "download")
	resolveIdx := strings.Index(s, "resolve")
	installIdx := strings.Index(s, "install")
	if !(downloadIdx >= 0 && resolveIdx > downloadIdx && installIdx > resolveIdx) {
		t.Fatalf("sections not sorted by duration:\n%s", s)
	}

	mIdx := strings.Index(s, "m/pkg")
	zIdx := strings.Index(s, "z/pkg")
	aIdx := strings.Index(s, "a/pkg")
	if !(mIdx >= 0 && zIdx > mIdx && aIdx > zIdx) {
		t.Fatalf("packages not sorted by duration then name:\n%s", s)
	}
}

func assertContains(t *testing.T, got, needle string) {
	t.Helper()
	if !strings.Contains(got, needle) {
		t.Fatalf("expected output to contain %q, got:\n%s", needle, got)
	}
}

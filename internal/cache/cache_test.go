package cache_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/van-sprundel/vif/internal/cache"
	"github.com/van-sprundel/vif/internal/testhelper"
)

func TestNew(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	// Database file should exist.
	if _, err := os.Stat(filepath.Join(dir, "vif.db")); err != nil {
		t.Errorf("database file not found: %v", err)
	}

	// files/ directory should exist.
	if _, err := os.Stat(filepath.Join(dir, "files")); err != nil {
		t.Errorf("files directory not found: %v", err)
	}
}

func TestCacheKey(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{
			url:  "https://api.github.com/repos/asm89/stack-cors/zipball/abc123",
			want: "389ed024f05371b988b56974f01bad61b7007a97a5fd5b9ff42448fff1661530",
		},
		{
			url:  "",
			want: "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", // sha256 of empty string
		},
	}

	for _, tc := range tests {
		got := cache.CacheKey(tc.url)
		if got != tc.want {
			t.Errorf("CacheKey(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestInsertAndLookup(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	key := cache.CacheKey("https://example.com/pkg.zip")

	// Lookup before insert should return false.
	_, ok, err := c.Lookup("vendor/pkg", "1.0.0")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok {
		t.Fatal("Lookup returned true before insert")
	}

	// Insert.
	err = c.Insert("vendor/pkg", "1.0.0", "https://example.com/pkg.zip", "abc123", key)
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}

	// Lookup after insert should return the entry.
	entry, ok, err := c.Lookup("vendor/pkg", "1.0.0")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup returned false after insert")
	}
	if entry.Name != "vendor/pkg" {
		t.Errorf("entry.Name = %q, want %q", entry.Name, "vendor/pkg")
	}
	if entry.Version != "1.0.0" {
		t.Errorf("entry.Version = %q, want %q", entry.Version, "1.0.0")
	}
	if entry.CacheKey != key {
		t.Errorf("entry.CacheKey = %q, want %q", entry.CacheKey, key)
	}
	if entry.DistURL != "https://example.com/pkg.zip" {
		t.Errorf("entry.DistURL = %q, want %q", entry.DistURL, "https://example.com/pkg.zip")
	}
	if entry.DistRef != "abc123" {
		t.Errorf("entry.DistRef = %q, want %q", entry.DistRef, "abc123")
	}
	if entry.CachedAt == 0 {
		t.Error("entry.CachedAt is zero")
	}
}

func TestInsertUpsert(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	key1 := cache.CacheKey("https://example.com/v1.zip")
	key2 := cache.CacheKey("https://example.com/v2.zip")

	// Insert first version.
	if err := c.Insert("vendor/pkg", "1.0.0", "https://example.com/v1.zip", "ref1", key1); err != nil {
		t.Fatalf("Insert 1: %v", err)
	}

	// Insert same name+version with different data (upsert).
	if err := c.Insert("vendor/pkg", "1.0.0", "https://example.com/v2.zip", "ref2", key2); err != nil {
		t.Fatalf("Insert 2: %v", err)
	}

	entry, ok, err := c.Lookup("vendor/pkg", "1.0.0")
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !ok {
		t.Fatal("Lookup returned false after upsert")
	}
	if entry.CacheKey != key2 {
		t.Errorf("after upsert, CacheKey = %q, want %q", entry.CacheKey, key2)
	}
}

func TestPackageDir(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	key := "deadbeef"
	got := c.PackageDir(key)
	want := filepath.Join(dir, "files", key)
	if got != want {
		t.Errorf("PackageDir(%q) = %q, want %q", key, got, want)
	}
}

func TestExtractedDir(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	key := "deadbeef"
	got := c.ExtractedDir(key)
	want := filepath.Join(dir, "files", key, "extracted")
	if got != want {
		t.Errorf("ExtractedDir(%q) = %q, want %q", key, got, want)
	}
}

func TestHasExtracted(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	key := "testkey"

	// Before creating directory, should return false.
	if c.HasExtracted(key) {
		t.Error("HasExtracted returned true before directory exists")
	}

	// Create the extracted directory.
	extractedDir := c.ExtractedDir(key)
	if err := os.MkdirAll(extractedDir, 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	// Now should return true.
	if !c.HasExtracted(key) {
		t.Error("HasExtracted returned false after directory was created")
	}
}

func TestInsertAndLookupMetadata(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	repoURL := "https://repo.packagist.org"
	pkg := "acme/foo"
	body := []byte(`{"packages":{"acme/foo":[{"name":"acme/foo","version":"1.0.0"}]}}`)
	etag := `"etag-abc"`

	// Lookup before insert should return false.
	_, _, ok, err := c.LookupMetadata(repoURL, pkg)
	if err != nil {
		t.Fatalf("LookupMetadata: %v", err)
	}
	if ok {
		t.Fatal("LookupMetadata returned true before insert")
	}

	// Insert.
	if err := c.InsertMetadata(repoURL, pkg, etag, body); err != nil {
		t.Fatalf("InsertMetadata: %v", err)
	}

	// Lookup after insert should return the entry.
	gotETag, gotBody, ok, err := c.LookupMetadata(repoURL, pkg)
	if err != nil {
		t.Fatalf("LookupMetadata: %v", err)
	}
	if !ok {
		t.Fatal("LookupMetadata returned false after insert")
	}
	if gotETag != etag {
		t.Errorf("etag = %q, want %q", gotETag, etag)
	}
	if string(gotBody) != string(body) {
		t.Errorf("body = %q, want %q", gotBody, body)
	}
}

func TestInsertMetadataUpsert(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	repoURL := "https://repo.packagist.org"
	pkg := "acme/foo"
	body1 := []byte(`{"packages":{"acme/foo":[{"version":"1.0.0"}]}}`)
	body2 := []byte(`{"packages":{"acme/foo":[{"version":"2.0.0"}]}}`)

	if err := c.InsertMetadata(repoURL, pkg, `"etag-v1"`, body1); err != nil {
		t.Fatalf("InsertMetadata 1: %v", err)
	}
	if err := c.InsertMetadata(repoURL, pkg, `"etag-v2"`, body2); err != nil {
		t.Fatalf("InsertMetadata 2: %v", err)
	}

	gotETag, gotBody, ok, err := c.LookupMetadata(repoURL, pkg)
	if err != nil {
		t.Fatalf("LookupMetadata: %v", err)
	}
	if !ok {
		t.Fatal("LookupMetadata returned false")
	}
	if gotETag != `"etag-v2"` {
		t.Errorf("after upsert, etag = %q, want %q", gotETag, `"etag-v2"`)
	}
	if string(gotBody) != string(body2) {
		t.Errorf("after upsert, body = %q, want %q", gotBody, body2)
	}
}

func TestMetadataDifferentRepos(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	pkg := "acme/foo"
	body1 := []byte(`{"packages":{"acme/foo":[{"version":"1.0.0"}]}}`)
	body2 := []byte(`{"packages":{"acme/foo":[{"version":"2.0.0"}]}}`)

	if err := c.InsertMetadata("https://repo.packagist.org", pkg, `"etag-public"`, body1); err != nil {
		t.Fatalf("InsertMetadata public: %v", err)
	}
	if err := c.InsertMetadata("https://private.example.com", pkg, `"etag-private"`, body2); err != nil {
		t.Fatalf("InsertMetadata private: %v", err)
	}

	_, gotBody1, ok1, err := c.LookupMetadata("https://repo.packagist.org", pkg)
	if err != nil || !ok1 {
		t.Fatalf("LookupMetadata public: ok=%v err=%v", ok1, err)
	}
	_, gotBody2, ok2, err := c.LookupMetadata("https://private.example.com", pkg)
	if err != nil || !ok2 {
		t.Fatalf("LookupMetadata private: ok=%v err=%v", ok2, err)
	}

	if string(gotBody1) != string(body1) {
		t.Errorf("public body mismatch: %q", gotBody1)
	}
	if string(gotBody2) != string(body2) {
		t.Errorf("private body mismatch: %q", gotBody2)
	}
}

func TestInsertAndLookupSolverMetadata(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	repoURL := "https://repo.packagist.org"
	pkg := "acme/foo"
	body := []byte(`{"name":"acme/foo","versions":[{"version":"1.0.0"}]}`)

	_, ok, err := c.LookupSolverMetadata(repoURL, pkg, true)
	if err != nil {
		t.Fatalf("LookupSolverMetadata: %v", err)
	}
	if ok {
		t.Fatal("LookupSolverMetadata returned true before insert")
	}

	if err := c.InsertSolverMetadata(repoURL, pkg, true, `"etag-solver"`, "hash-solver", 1, body); err != nil {
		t.Fatalf("InsertSolverMetadata: %v", err)
	}

	entry, ok, err := c.LookupSolverMetadata(repoURL, pkg, true)
	if err != nil {
		t.Fatalf("LookupSolverMetadata: %v", err)
	}
	if !ok {
		t.Fatal("LookupSolverMetadata returned false after insert")
	}
	if entry.SourceETag != `"etag-solver"` {
		t.Errorf("SourceETag = %q, want %q", entry.SourceETag, `"etag-solver"`)
	}
	if entry.SourceHash != "hash-solver" {
		t.Errorf("SourceHash = %q, want %q", entry.SourceHash, "hash-solver")
	}
	if entry.SchemaVer != 1 {
		t.Errorf("SchemaVer = %d, want 1", entry.SchemaVer)
	}
	if string(entry.Body) != string(body) {
		t.Errorf("Body = %q, want %q", entry.Body, body)
	}
	if !entry.DevIncluded {
		t.Error("DevIncluded = false, want true")
	}
	if entry.GeneratedAt == 0 {
		t.Error("GeneratedAt is zero")
	}
}

func TestSolverMetadataUpsertAndDevIsolation(t *testing.T) {
	dir := testhelper.TempDir(t, "cache")
	c, err := cache.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()

	repoURL := "https://repo.packagist.org"
	pkg := "acme/foo"
	body1 := []byte(`{"name":"acme/foo","versions":[{"version":"1.0.0"}]}`)
	body2 := []byte(`{"name":"acme/foo","versions":[{"version":"2.0.0"}]}`)
	devBody := []byte(`{"name":"acme/foo","dev_included":true,"versions":[{"version":"dev-main"}]}`)

	if err := c.InsertSolverMetadata(repoURL, pkg, false, `"etag-v1"`, "hash-v1", 1, body1); err != nil {
		t.Fatalf("InsertSolverMetadata stable 1: %v", err)
	}
	if err := c.InsertSolverMetadata(repoURL, pkg, false, `"etag-v2"`, "hash-v2", 1, body2); err != nil {
		t.Fatalf("InsertSolverMetadata stable 2: %v", err)
	}
	if err := c.InsertSolverMetadata(repoURL, pkg, true, `"etag-dev"`, "hash-dev", 1, devBody); err != nil {
		t.Fatalf("InsertSolverMetadata dev: %v", err)
	}

	stable, ok, err := c.LookupSolverMetadata(repoURL, pkg, false)
	if err != nil || !ok {
		t.Fatalf("LookupSolverMetadata stable: ok=%v err=%v", ok, err)
	}
	dev, ok, err := c.LookupSolverMetadata(repoURL, pkg, true)
	if err != nil || !ok {
		t.Fatalf("LookupSolverMetadata dev: ok=%v err=%v", ok, err)
	}

	if stable.SourceETag != `"etag-v2"` {
		t.Errorf("stable SourceETag = %q, want %q", stable.SourceETag, `"etag-v2"`)
	}
	if string(stable.Body) != string(body2) {
		t.Errorf("stable Body = %q, want %q", stable.Body, body2)
	}
	if string(dev.Body) != string(devBody) {
		t.Errorf("dev Body = %q, want %q", dev.Body, devBody)
	}
}

func BenchmarkInsert(b *testing.B) {
	dir := testhelper.TempDir(b, "cache")
	c, err := cache.New(dir)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer c.Close()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = c.Insert("vendor/pkg", "1.0.0", "https://example.com/pkg.zip", "ref", "cachekey")
	}
}

func BenchmarkLookup(b *testing.B) {
	dir := testhelper.TempDir(b, "cache")
	c, err := cache.New(dir)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	defer c.Close()

	_ = c.Insert("vendor/pkg", "1.0.0", "https://example.com/pkg.zip", "ref", "cachekey")

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _ = c.Lookup("vendor/pkg", "1.0.0")
	}
}

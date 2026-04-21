package cache

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Entry represents a cached package row.
type Entry struct {
	Name     string
	Version  string
	DistURL  string
	DistRef  string
	CacheKey string
	CachedAt int64
}

// Cache provides SQLite-backed package cache operations.
type Cache struct {
	db               *sql.DB
	dir              string // root cache directory
	insertPkgStmt    *sql.Stmt
	lookupPkgStmt    *sql.Stmt
	insertMetaStmt   *sql.Stmt
	lookupMetaStmt   *sql.Stmt
	insertSolverStmt *sql.Stmt
	lookupSolverStmt *sql.Stmt
}

// SolverMetadataEntry stores a compact resolver-oriented metadata row.
type SolverMetadataEntry struct {
	RepoURL     string
	Package     string
	DevIncluded bool
	SourceETag  string
	SourceHash  string
	SchemaVer   int
	Body        []byte
	GeneratedAt int64
}

// CacheKey computes the cache key for a dist URL: hex-encoded SHA-256.
func CacheKey(distURL string) string {
	h := sha256.Sum256([]byte(distURL))
	return hex.EncodeToString(h[:])
}

// New opens (or creates) the cache at dir.
// It creates the database, runs migrations, and ensures the files/ directory exists.
func New(dir string) (*Cache, error) {
	if err := os.MkdirAll(filepath.Join(dir, "files"), 0o755); err != nil {
		return nil, fmt.Errorf("cache: create directories: %w", err)
	}

	dbPath := filepath.Join(dir, "vif.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("cache: open database: %w", err)
	}

	// Serialize all DB access through a single connection.
	// SQLite only supports one writer at a time; this avoids SQLITE_BUSY
	// from Go's connection pool opening multiple connections.
	db.SetMaxOpenConns(1)

	// WAL mode for better read concurrency.
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: set WAL mode: %w", err)
	}
	if _, err := db.Exec("PRAGMA busy_timeout=5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: set busy timeout: %w", err)
	}

	if _, err := db.Exec(createPackagesTable); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: create table: %w", err)
	}
	if _, err := db.Exec(createMetadataTable); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: create metadata table: %w", err)
	}
	if _, err := db.Exec(createSolverMetadataTable); err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: create solver metadata table: %w", err)
	}

	insertPkgStmt, err := db.Prepare(`
		INSERT INTO packages (name, version, dist_url, dist_ref, cache_key, cached_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name, version) DO UPDATE SET
			dist_url  = excluded.dist_url,
			dist_ref  = excluded.dist_ref,
			cache_key = excluded.cache_key,
			cached_at = excluded.cached_at
	`)
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("cache: prepare package insert: %w", err)
	}

	lookupPkgStmt, err := db.Prepare(`
		SELECT name, version, dist_url, dist_ref, cache_key, cached_at
		FROM packages
		WHERE name = ? AND version = ?
	`)
	if err != nil {
		insertPkgStmt.Close()
		db.Close()
		return nil, fmt.Errorf("cache: prepare package lookup: %w", err)
	}

	insertMetaStmt, err := db.Prepare(`
		INSERT INTO metadata (repo_url, package, etag, body, fetched_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (repo_url, package) DO UPDATE SET
			etag       = excluded.etag,
			body       = excluded.body,
			fetched_at = excluded.fetched_at
	`)
	if err != nil {
		lookupPkgStmt.Close()
		insertPkgStmt.Close()
		db.Close()
		return nil, fmt.Errorf("cache: prepare metadata insert: %w", err)
	}

	lookupMetaStmt, err := db.Prepare(`
		SELECT etag, body
		FROM metadata
		WHERE repo_url = ? AND package = ?
	`)
	if err != nil {
		insertMetaStmt.Close()
		lookupPkgStmt.Close()
		insertPkgStmt.Close()
		db.Close()
		return nil, fmt.Errorf("cache: prepare metadata lookup: %w", err)
	}

	insertSolverStmt, err := db.Prepare(`
		INSERT INTO solver_metadata (repo_url, package, dev_included, source_etag, source_hash, schema_ver, body, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (repo_url, package, dev_included) DO UPDATE SET
			source_etag  = excluded.source_etag,
			source_hash  = excluded.source_hash,
			schema_ver   = excluded.schema_ver,
			body         = excluded.body,
			generated_at = excluded.generated_at
	`)
	if err != nil {
		lookupMetaStmt.Close()
		insertMetaStmt.Close()
		lookupPkgStmt.Close()
		insertPkgStmt.Close()
		db.Close()
		return nil, fmt.Errorf("cache: prepare solver metadata insert: %w", err)
	}

	lookupSolverStmt, err := db.Prepare(`
		SELECT source_etag, source_hash, schema_ver, body, generated_at
		FROM solver_metadata
		WHERE repo_url = ? AND package = ? AND dev_included = ?
	`)
	if err != nil {
		insertSolverStmt.Close()
		lookupMetaStmt.Close()
		insertMetaStmt.Close()
		lookupPkgStmt.Close()
		insertPkgStmt.Close()
		db.Close()
		return nil, fmt.Errorf("cache: prepare solver metadata lookup: %w", err)
	}

	return &Cache{
		db:               db,
		dir:              dir,
		insertPkgStmt:    insertPkgStmt,
		lookupPkgStmt:    lookupPkgStmt,
		insertMetaStmt:   insertMetaStmt,
		lookupMetaStmt:   lookupMetaStmt,
		insertSolverStmt: insertSolverStmt,
		lookupSolverStmt: lookupSolverStmt,
	}, nil
}

// Close closes the underlying database connection.
func (c *Cache) Close() error {
	if c.lookupSolverStmt != nil {
		_ = c.lookupSolverStmt.Close()
	}
	if c.insertSolverStmt != nil {
		_ = c.insertSolverStmt.Close()
	}
	if c.lookupMetaStmt != nil {
		_ = c.lookupMetaStmt.Close()
	}
	if c.insertMetaStmt != nil {
		_ = c.insertMetaStmt.Close()
	}
	if c.lookupPkgStmt != nil {
		_ = c.lookupPkgStmt.Close()
	}
	if c.insertPkgStmt != nil {
		_ = c.insertPkgStmt.Close()
	}
	return c.db.Close()
}

// Insert adds or updates a package entry in the cache.
func (c *Cache) Insert(name, version, distURL, distRef, cacheKey string) error {
	_, err := c.insertPkgStmt.Exec(name, version, distURL, distRef, cacheKey, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cache: insert %s@%s: %w", name, version, err)
	}
	return nil
}

// Lookup retrieves a cached package entry by name and version.
// Returns the entry and true if found, or a zero Entry and false if not.
func (c *Cache) Lookup(name, version string) (Entry, bool, error) {
	var e Entry
	err := c.lookupPkgStmt.QueryRow(name, version).Scan(
		&e.Name, &e.Version, &e.DistURL, &e.DistRef, &e.CacheKey, &e.CachedAt,
	)
	if err == sql.ErrNoRows {
		return Entry{}, false, nil
	}
	if err != nil {
		return Entry{}, false, fmt.Errorf("cache: lookup %s@%s: %w", name, version, err)
	}
	return e, true, nil
}

// LookupMetadata returns the cached P2 metadata for a package from a specific repo.
// Returns (etag, body, true, nil) if found, or ("", nil, false, nil) if not present.
func (c *Cache) LookupMetadata(repoURL, packageName string) (etag string, body []byte, ok bool, err error) {
	var e, b []byte
	scanErr := c.lookupMetaStmt.QueryRow(repoURL, packageName).Scan(&e, &b)
	if scanErr == sql.ErrNoRows {
		return "", nil, false, nil
	}
	if scanErr != nil {
		return "", nil, false, fmt.Errorf("cache: lookup metadata %s %s: %w", repoURL, packageName, scanErr)
	}
	return string(e), b, true, nil
}

// InsertMetadata stores or updates the cached P2 metadata for a package.
func (c *Cache) InsertMetadata(repoURL, packageName, etag string, body []byte) error {
	_, err := c.insertMetaStmt.Exec(repoURL, packageName, etag, body, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cache: insert metadata %s %s: %w", repoURL, packageName, err)
	}
	return nil
}

// LookupSolverMetadata returns the cached compact solver metadata for a package from a specific repo.
// Returns (entry, true, nil) if found, or (zero, false, nil) if not present.
func (c *Cache) LookupSolverMetadata(repoURL, packageName string, devIncluded bool) (SolverMetadataEntry, bool, error) {
	var (
		entry   SolverMetadataEntry
		devFlag int
	)
	if devIncluded {
		devFlag = 1
	}
	err := c.lookupSolverStmt.QueryRow(repoURL, packageName, devFlag).Scan(
		&entry.SourceETag, &entry.SourceHash, &entry.SchemaVer, &entry.Body, &entry.GeneratedAt,
	)
	if err == sql.ErrNoRows {
		return SolverMetadataEntry{}, false, nil
	}
	if err != nil {
		return SolverMetadataEntry{}, false, fmt.Errorf("cache: lookup solver metadata %s %s dev=%t: %w", repoURL, packageName, devIncluded, err)
	}
	entry.RepoURL = repoURL
	entry.Package = packageName
	entry.DevIncluded = devIncluded
	return entry, true, nil
}

// InsertSolverMetadata stores or updates compact solver metadata for a package.
func (c *Cache) InsertSolverMetadata(repoURL, packageName string, devIncluded bool, sourceETag, sourceHash string, schemaVer int, body []byte) error {
	devFlag := 0
	if devIncluded {
		devFlag = 1
	}
	_, err := c.insertSolverStmt.Exec(repoURL, packageName, devFlag, sourceETag, sourceHash, schemaVer, body, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cache: insert solver metadata %s %s dev=%t: %w", repoURL, packageName, devIncluded, err)
	}
	return nil
}

// PackageDir returns the cache directory for a given cache key: <dir>/files/<cacheKey>.
func (c *Cache) PackageDir(cacheKey string) string {
	return filepath.Join(c.dir, "files", cacheKey)
}

// ExtractedDir returns the extracted directory for a given cache key: <dir>/files/<cacheKey>/extracted.
func (c *Cache) ExtractedDir(cacheKey string) string {
	return filepath.Join(c.dir, "files", cacheKey, "extracted")
}

// HasExtracted reports whether the extracted directory for cacheKey exists on disk.
func (c *Cache) HasExtracted(cacheKey string) bool {
	info, err := os.Stat(c.ExtractedDir(cacheKey))
	return err == nil && info.IsDir()
}

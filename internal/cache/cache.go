package cache

import (
	"crypto/sha256"
	"database/sql"
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
	db  *sql.DB
	dir string // root cache directory
}

// CacheKey computes the cache key for a dist URL: hex-encoded SHA-256.
func CacheKey(distURL string) string {
	h := sha256.Sum256([]byte(distURL))
	return fmt.Sprintf("%x", h)
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

	return &Cache{db: db, dir: dir}, nil
}

// Close closes the underlying database connection.
func (c *Cache) Close() error {
	return c.db.Close()
}

// Insert adds or updates a package entry in the cache.
func (c *Cache) Insert(name, version, distURL, distRef, cacheKey string) error {
	const query = `
		INSERT INTO packages (name, version, dist_url, dist_ref, cache_key, cached_at)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name, version) DO UPDATE SET
			dist_url  = excluded.dist_url,
			dist_ref  = excluded.dist_ref,
			cache_key = excluded.cache_key,
			cached_at = excluded.cached_at
	`
	_, err := c.db.Exec(query, name, version, distURL, distRef, cacheKey, time.Now().Unix())
	if err != nil {
		return fmt.Errorf("cache: insert %s@%s: %w", name, version, err)
	}
	return nil
}

// Lookup retrieves a cached package entry by name and version.
// Returns the entry and true if found, or a zero Entry and false if not.
func (c *Cache) Lookup(name, version string) (Entry, bool, error) {
	const query = `
		SELECT name, version, dist_url, dist_ref, cache_key, cached_at
		FROM packages
		WHERE name = ? AND version = ?
	`
	var e Entry
	err := c.db.QueryRow(query, name, version).Scan(
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

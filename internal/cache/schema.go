package cache

// DDL statements for the vif cache database.

const createPackagesTable = `
CREATE TABLE IF NOT EXISTS packages (
    name       TEXT NOT NULL,
    version    TEXT NOT NULL,
    dist_url   TEXT NOT NULL,
    dist_ref   TEXT NOT NULL,
    cache_key  TEXT NOT NULL,
    cached_at  INTEGER NOT NULL,
    PRIMARY KEY (name, version)
);
`

const createMetadataTable = `
CREATE TABLE IF NOT EXISTS metadata (
    repo_url    TEXT NOT NULL,
    package     TEXT NOT NULL,
    etag        TEXT NOT NULL DEFAULT '',
    body        BLOB NOT NULL,
    fetched_at  INTEGER NOT NULL,
    PRIMARY KEY (repo_url, package)
);
`

const createSolverMetadataTable = `
CREATE TABLE IF NOT EXISTS solver_metadata (
    repo_url      TEXT NOT NULL,
    package       TEXT NOT NULL,
    dev_included  INTEGER NOT NULL,
    source_etag   TEXT NOT NULL DEFAULT '',
    source_hash   TEXT NOT NULL DEFAULT '',
    schema_ver    INTEGER NOT NULL,
    body          BLOB NOT NULL,
    generated_at  INTEGER NOT NULL,
    PRIMARY KEY (repo_url, package, dev_included)
);
`

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

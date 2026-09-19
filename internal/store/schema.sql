-- ============================================================================
-- flow-go — SQLite schema (data/flow.db)
--
-- Every table here is written by the running engine. Nothing in this database is
-- seeded with test fixtures: the Python version's /stats endpoint reported
-- 1600 credits and 8 accounts that were entirely rows left behind by
-- tests/test_worker_pool.py, which wrote to the production database because it
-- had no isolation. Tests here use a temporary file per test and never touch
-- this database.
-- ============================================================================

PRAGMA journal_mode = WAL;
PRAGMA synchronous = NORMAL;
PRAGMA busy_timeout = 5000;
PRAGMA temp_store = MEMORY;
PRAGMA foreign_keys = ON;

-- ----------------------------------------------------------------------------
-- accounts: one row per credential set the pool can route to.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS accounts (
    account_id          TEXT PRIMARY KEY,          -- label used in logs and the pool
    cookie_hash         TEXT,                      -- fingerprint of the cookie jar
    sku                 TEXT,                      -- G1_FREEMIUM | G1_TIER1 | ''
    credits             INTEGER,                   -- last observed balance, NULL if unknown
    credits_checked_at  DATETIME,                  -- when credits was last confirmed
    status              TEXT NOT NULL DEFAULT 'active',  -- active | parked | dead
    last_error          TEXT,
    total_requests      INTEGER NOT NULL DEFAULT 0,
    total_failures      INTEGER NOT NULL DEFAULT 0,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at        DATETIME
);

CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);

-- ----------------------------------------------------------------------------
-- generations: one row per submitted generation job.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS generations (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    job_id              TEXT UNIQUE NOT NULL,      -- caller-supplied or generated
    account_id          TEXT,
    kind                TEXT NOT NULL,             -- video | image | upsample | edit
    prompt              TEXT,
    model               TEXT,
    duration            INTEGER,                   -- seconds, video only
    aspect              TEXT,
    count               INTEGER NOT NULL DEFAULT 1,
    media_ids           TEXT,                      -- JSON array
    status              TEXT NOT NULL DEFAULT 'submitted',  -- submitted|succeeded|failed
    credits_spent       INTEGER,
    elapsed_ms          INTEGER,
    error               TEXT,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    finished_at         DATETIME
);

CREATE INDEX IF NOT EXISTS idx_generations_status ON generations(status);
CREATE INDEX IF NOT EXISTS idx_generations_created ON generations(created_at);
CREATE INDEX IF NOT EXISTS idx_generations_account ON generations(account_id);

-- ----------------------------------------------------------------------------
-- media: one row per produced or downloaded asset.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS media (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    generation_id       INTEGER REFERENCES generations(id) ON DELETE CASCADE,
    media_id            TEXT,
    kind                TEXT NOT NULL,             -- video | image
    prompt              TEXT,
    file_name           TEXT,
    file_path           TEXT,
    url                 TEXT,
    resolution          TEXT,                      -- 720p | 1080p | 4k
    bytes               INTEGER,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_media_kind ON media(kind);
CREATE INDEX IF NOT EXISTS idx_media_media_id ON media(media_id);
CREATE INDEX IF NOT EXISTS idx_media_created ON media(created_at);

-- ----------------------------------------------------------------------------
-- request_logs: upstream call accounting, used for latency and error reporting.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS request_logs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    account_id          TEXT,
    endpoint            TEXT NOT NULL,
    status_code         INTEGER,
    elapsed_ms          REAL,
    error               TEXT,
    created_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_reqlogs_created ON request_logs(created_at);
CREATE INDEX IF NOT EXISTS idx_reqlogs_status ON request_logs(status_code);

-- ----------------------------------------------------------------------------
-- settings: engine configuration that has to outlive a restart.
--
-- A single key/value table rather than a column somewhere, because what belongs
-- here is exactly the set of choices an operator makes at runtime and expects to
-- still hold tomorrow. The first one is the signed-in account: it used to live in
-- memory alone, so every restart silently reverted to the first signed-in
-- account — which is how a 1-credit account came to look like the only one.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS settings (
    key                 TEXT PRIMARY KEY,
    value               TEXT NOT NULL,
    updated_at          DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

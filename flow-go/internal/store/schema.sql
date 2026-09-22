-- ============================================================================
-- flow-go — SQLite schema (data/flow.db)
--
-- Every table here is written by the running engine. Nothing in this database is
-- seeded with test fixtures: the Python version's /stats endpoint reported
-- 1600 credits and 8 accounts that were entirely rows left behind by its own
-- worker-pool test, which wrote to the production database because it had no
-- isolation. That test and the rest of the Python tree are gone; the reason to
-- remember them is that this file must never grow a seed block. Tests here use a
-- temporary file per test and never touch this database.
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
    last_used_at        DATETIME,

    -- Identity.
    --
    -- identity_key is the anchor the account is matched on: a hash of the
    -- long-lived credential cookies, a GAIA id, or an explicit override. It is
    -- deliberately separate from account_id so a rotation can move the anchor
    -- without changing the label — which is what stops one account becoming
    -- several rows. cookie_hash above is now only a change detector.
    identity_key        TEXT,
    identity_source     TEXT,                      -- override|session|cookie-core|re-anchored|adopted-legacy
    superseded_by       TEXT,                      -- set when this anchor rotated into another
    last_authuser       INTEGER,                   -- the authuser index this row was last seen at
    sapisid_fingerprint TEXT                       -- hash of SAPISID; the continuity proof
);

CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);

-- NOTE: there is deliberately no index on accounts(identity_key) here.
--
-- This script runs before migrate() adds the identity columns to a database
-- that predates them, and CREATE INDEX on a column that does not exist yet is
-- not a no-op — SQLite rejects it with "no such column". A database created
-- before this change would then refuse to open at all. The index is created
-- after the ALTERs instead; see postColumnIndexes in db.go.

-- ----------------------------------------------------------------------------
-- account_anchors: every anchor and jar hash an account has ever presented.
--
-- This is the audit trail that makes re-anchoring safe. When an anchor rotates,
-- the old one is kept here rather than overwritten, so "these two rows are the
-- same account" is a recorded fact the operator can check instead of an
-- inference they have to trust.
-- ----------------------------------------------------------------------------
CREATE TABLE IF NOT EXISTS account_anchors (
    account_id   TEXT NOT NULL,
    identity_key TEXT NOT NULL,
    cookie_hash  TEXT,                             -- NULL for an anchor recorded before its jar hash was known
    source       TEXT,
    first_seen   DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (account_id, identity_key)
);

CREATE INDEX IF NOT EXISTS idx_anchors_key ON account_anchors(identity_key);

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
    -- Five values, not three. 'empty' is a job the transport accepted and
    -- produced nothing for, and 'ready' is a video whose render resolved and was
    -- downloaded. Both were missing from this comment and from the Stats query,
    -- which is how ten of the fifteen jobs in one database came to be counted in
    -- no bucket at all.
    status              TEXT NOT NULL DEFAULT 'submitted',  -- submitted|succeeded|failed|empty|ready
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

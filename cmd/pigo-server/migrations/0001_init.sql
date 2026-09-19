-- The initial schema. One script serves SQLite and PostgreSQL: it sticks to
-- their common subset (TEXT, BIGINT, INTEGER, DOUBLE PRECISION). Times are UTC
-- Unix nanoseconds; flags are 0/1 integers. Statements end with ";" at the end
-- of a line, which is how the upgrader splits them.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL,
    username_key  TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    BIGINT NOT NULL,
    disabled_at   BIGINT
);

-- Login tokens, stored only as hashes.
CREATE TABLE auth_sessions (
    token_hash TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    expires_at BIGINT NOT NULL
);
CREATE INDEX auth_sessions_user ON auth_sessions (user_id);

-- Provider keys as sealed records (AES-GCM, bound to owner and provider).
-- owner is '' for the administrator's shared pool.
CREATE TABLE credentials (
    owner    TEXT NOT NULL,
    provider TEXT NOT NULL,
    record   TEXT NOT NULL,
    PRIMARY KEY (owner, provider)
);

-- Low-traffic configuration documents, read and written whole. "server" holds
-- the defaults, switches, custom providers and the price table.
CREATE TABLE settings (
    name  TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

CREATE TABLE custom_models (
    scope      TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    id         TEXT NOT NULL,
    label      TEXT NOT NULL,
    provider   TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    PRIMARY KEY (scope, user_id, id)
);

-- One row per chat session; the metadata document (model, title, running and
-- last turn …) is JSON, so it can grow fields without a schema change. The
-- session's transcript is a file in its directory, not a table.
CREATE TABLE sessions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL,
    created_at BIGINT NOT NULL,
    meta       TEXT NOT NULL
);
CREATE INDEX sessions_user ON sessions (user_id);

-- The usage ledger: one row per model call, never updated.
CREATE TABLE ledger (
    id                TEXT PRIMARY KEY,
    at                BIGINT NOT NULL,
    user_id           TEXT NOT NULL,
    username          TEXT NOT NULL,
    session_id        TEXT NOT NULL,
    turn_id           TEXT NOT NULL,
    response_id       TEXT NOT NULL,
    provider          TEXT NOT NULL,
    model             TEXT NOT NULL,
    response_model    TEXT NOT NULL,
    kind              TEXT NOT NULL,
    status            TEXT NOT NULL,
    key_source        TEXT NOT NULL,
    billed_to         TEXT NOT NULL,
    input             BIGINT NOT NULL,
    cache_read        BIGINT NOT NULL,
    cache_write       BIGINT NOT NULL,
    output            BIGINT NOT NULL,
    reasoning         BIGINT NOT NULL,
    price             TEXT,
    priced            INTEGER NOT NULL,
    cost              BIGINT NOT NULL,
    upstream_cost_usd DOUBLE PRECISION,
    usage_anomaly     INTEGER NOT NULL
);
CREATE INDEX ledger_at ON ledger (at);
CREATE INDEX ledger_user_at ON ledger (user_id, at);
CREATE INDEX ledger_session ON ledger (session_id);

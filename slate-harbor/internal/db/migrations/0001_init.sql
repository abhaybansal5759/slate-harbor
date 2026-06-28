-- Wallets: one row per player. The materialized balance for fast reads + locking.
-- CHECK (balance >= 0) is a hard backstop: even a buggy app can never commit a negative balance.
CREATE TABLE IF NOT EXISTS wallets (
    player_id   TEXT PRIMARY KEY,
    balance     BIGINT NOT NULL DEFAULT 0 CHECK (balance >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Ledger: append-only log, one row per money movement (+credit, -debit).
-- This is the audit source of truth. Invariant: wallets.balance == SUM(ledger.delta) per player.
-- It's also how you'd detect last week's "double-granted currency" bug (RESILIENCE.md).
CREATE TABLE IF NOT EXISTS ledger (
    id               BIGSERIAL PRIMARY KEY,
    player_id        TEXT NOT NULL,
    delta            BIGINT NOT NULL,
    reason           TEXT,
    ref_type         TEXT NOT NULL,            -- 'credit' | 'purchase'
    ref_id           TEXT,                     -- itemId for purchases
    idempotency_key  TEXT,                     -- the request that produced this row
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_ledger_player ON ledger (player_id);

-- Inventory: items a player owns. (player_id, item_id) unique; purchase upserts quantity.
CREATE TABLE IF NOT EXISTS inventory (
    player_id   TEXT NOT NULL,
    item_id     TEXT NOT NULL,
    quantity    BIGINT NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (player_id, item_id)
);

-- Claimed rewards: the composite primary key (reward_id, player_id) IS the claim-once guarantee.
-- A second claim hits a uniqueness violation, which we translate into "already claimed".
CREATE TABLE IF NOT EXISTS claimed_rewards (
    reward_id   TEXT NOT NULL,
    player_id   TEXT NOT NULL,
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (reward_id, player_id)
);

-- Idempotency keys: the exactly-once engine for credit/purchase.
-- First request inserts the key AND does the work in the same transaction, then stores its response.
-- A duplicate request collides on the PK and we replay the stored response verbatim.
-- request_hash guards against a client reusing one key for a *different* body (a client bug we reject).
CREATE TABLE IF NOT EXISTS idempotency_keys (
    idempotency_key  TEXT PRIMARY KEY,
    request_hash     TEXT NOT NULL,
    response_status  INT  NOT NULL,
    response_body    JSONB NOT NULL,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_idem_created ON idempotency_keys (created_at);  -- for retention sweeps
# DESIGN.md — Durable Game Economy Service

## 1. Overview

A small wallet/economy backend exposing four HTTP endpoints (credit, purchase,
claim, read). The design goal is narrow and deliberate: **never lose or duplicate
a player's money or items** — under retried (duplicate) requests, concurrent
requests racing the same wallet, or a hard `kill -9` at any moment.

The guiding principle throughout: **correctness is enforced by Postgres, not by
application logic.** The Go service validates input, shapes responses, and opens
transactions; every invariant that protects money is a database mechanism (a
transaction boundary, a row lock, a unique constraint, a check constraint). This
keeps the surface area for correctness bugs small and auditable.

## 2. Architecture

```
HTTP client
   │  POST /v1/wallets/{id}/credit | /purchase | /v1/rewards/{id}/claim | GET /v1/wallets/{id}
   ▼
Go service (stdlib net/http, Go 1.25 path wildcards)
   │  - validates input at the boundary
   │  - one DB transaction per mutating request
   ▼
Postgres 16  (pgx/v5 connection pool)
   - wallets, ledger, inventory, claimed_rewards, idempotency_keys
   - data on a named Docker volume (survives container restart)
```

- **Language/runtime: Go.** Single static binary, trivial distroless image,
  and a concurrency model that makes the "many requests, same wallet" test natural.
- **Datastore: Postgres 16.** The core operation — purchase — is a multi-row,
  multi-step atomic action (read balance → check funds → debit → grant item →
  append ledger → record idempotency). That demands real multi-row ACID
  transactions, strong isolation, and guaranteed durability. Each requirement
  maps to a named Postgres mechanism (Section 4).
- **Driver: pgx/v5** connection pool.
- **Migrations:** the schema is embedded in the binary (`//go:embed`) and applied
  on startup. All DDL is `IF NOT EXISTS`, so startup is idempotent. For a service
  this size that is deliberately simpler than a migration framework; to evolve
  schemas over time the natural next step is golang-migrate or goose.

### Why not RethinkDB / other document stores
A document store has no general multi-document ACID transactions, so the atomic
debit-and-grant would have to be crammed into a single document with a hand-rolled
scheme — fighting the tool. SQLite would also be a legitimate, fully-ACID choice
(embedded, simple crash test); Postgres was chosen for the clearer demonstration
of explicit row-level locking under concurrency.

## 3. API contract

All bodies are JSON. All mutating endpoints reject unknown JSON fields and cap the
request body at 1 MiB.

| Method & path | Body | Success | Notable errors |
|---|---|---|---|
| `POST /v1/wallets/{playerId}/credit` | `{"amount": int>0, "reason": str}` | `200 {"balance": int}` | `400` bad input / missing `Idempotency-Key`; `422` key reused with different body |
| `POST /v1/wallets/{playerId}/purchase` | `{"itemId": str, "price": int>0}` | `200 {"balance": int, "itemId": str}` | `402` insufficient funds (no effect); `400`; `422` |
| `POST /v1/rewards/{rewardId}/claim` | `{"playerId": str}` | `200 {"rewardId","playerId","claimed":true,"alreadyClaimed":bool}` | `400` |
| `GET /v1/wallets/{playerId}` | — | `200 {"balance":int,"inventory":[...],"claimedRewards":[...]}` | — |

**Status codes chosen:** `200` success; `400` validation failure; `402` insufficient
funds (a well-defined business rejection, distinct from a malformed request);
`413` body too large; `422` idempotency key reused with a different request body;
`500` unexpected server error; `503` from `/readyz` when the DB is unreachable.

**Units:** currency and prices are non-negative integers (`BIGINT`). No fractional
currency — avoids rounding ambiguity in a ledger.

**Unknown player on GET:** returns a zero-state wallet (`balance 0`, empty arrays)
rather than `404`. A wallet conceptually always exists at 0; reads never fail for a
valid player; this pairs with lazy wallet creation on the first credit.

## 4. Atomicity & durability

### What is atomic
Each mutating request runs inside exactly one Postgres transaction. For a purchase,
the transaction contains: lock wallet → check funds → debit → grant item → append
ledger row → claim idempotency key. It is all-or-nothing: either every row change
commits together, or none do.

### Isolation level
- **credit / purchase: READ COMMITTED.**
  - *credit* increments with an atomic `UPDATE ... SET balance = balance + $amt`
    via `INSERT ... ON CONFLICT DO UPDATE`. The row lock taken by the upsert makes
    concurrent credits to one wallet serialize on that row, so there are no lost
    updates even at READ COMMITTED.
  - *purchase* takes an explicit `SELECT ... FOR UPDATE` on the wallet row before
    reading the balance. A second concurrent purchase on the same wallet blocks on
    that lock until the first transaction commits, so two purchases cannot both read
    the same balance and both decide they can afford it. This is what prevents
    double-spend and lost updates; SERIALIZABLE is unnecessary because the explicit
    row lock already serializes the only conflicting access pattern.
- **GET: REPEATABLE READ, read-only.** The three reads (balance, inventory,
  claimed rewards) run in one snapshot so a concurrent mutation cannot interleave
  between them and produce an inconsistent view.

### `CHECK (balance >= 0)`
A hard backstop independent of application logic: even a logic bug can never commit
a negative balance — the transaction aborts instead.

### Durability across `kill -9`
- Postgres is configured with its default `synchronous_commit = on`: each `COMMIT`
  is flushed to the write-ahead log (WAL) and fsync'd to disk **before** the client
  receives acknowledgement. A committed operation is therefore on durable storage by
  the time the caller sees success.
- Data lives on a **named Docker volume** (`dbdata`), so it survives container
  restart / re-creation.
- **Behaviour if killed mid-purchase:** the transaction was never committed, so on
  restart WAL recovery rolls back all of its partial work. There is never a debit
  without its grant, nor a grant without its debit — the half-completed purchase
  simply did not happen. A client that retries after the crash (same idempotency
  key) lands in one of two states, both correct:
  1. The original transaction *had* committed before the kill → the stored response
     is replayed; no second effect.
  2. The original transaction had *not* committed → the retry is the first
     successful attempt; exactly one effect.

This is verified by `crash_test.ps1` (kill → restart → assert committed credit
intact and post-crash retry idempotent) and by the audit invariant (Section 6).

## 5. Exactly-once / non-duplicate strategy

`credit` and `purchase` have no natural dedup key (crediting 100 twice may be two
real battle payouts or one retry), so the **client supplies an `Idempotency-Key`
header**; it is required, and a missing key is a `400`. (Generating one server-side
would defeat the purpose — a client retry would carry a fresh key and double-apply.)

Mechanism, per mutating request:
1. The work (wallet change, item grant, ledger append) and the **insert of the
   idempotency key row** happen in the **same transaction**, with the key insert as
   the **last** statement.
2. The `idempotency_keys` table has the key as PRIMARY KEY and stores
   `request_hash`, `response_status`, and `response_body` (JSONB). On success the
   transaction commits all of it together.
3. A **duplicate** request collides on the primary key. Postgres serializes this:
   the conflicting insert waits for the first transaction to finish, so by the time
   the duplicate sees the unique violation (`23505`), the stored response is
   guaranteed visible. The duplicate rolls back its own work and **replays the
   stored status + body verbatim**.
4. `request_hash` (SHA-256 over endpoint scope + playerId + raw body) guards against
   a client reusing one key for a *different* request — that is a client bug and is
   rejected with `422`, never applied.

**Purchase ordering subtlety (important):** for purchase, the idempotency key is
checked **before** the funds rule. A naive ordering (funds-check first) would cause
a duplicate of a *successful* purchase to be re-evaluated against the
already-debited balance and wrongly rejected as "insufficient funds." Instead, a
completed key — whether the stored outcome was a success (200) or a rejection (402)
— replays immediately. One key therefore maps to exactly one outcome for all time.
A client that wants to retry after topping up its balance must use a **new** key.

`claim` needs no header: it is naturally idempotent via the composite PRIMARY KEY
`(reward_id, player_id)`. The grant uses `INSERT ... ON CONFLICT DO NOTHING`; if zero
rows were inserted the reward was already claimed (`alreadyClaimed: true`), and no
second grant occurs. Concurrent first-claims serialize on the unique index — exactly
one inserts.

### Key retention
Idempotency keys are retained for a **72-hour** window — long enough to cover any
realistic client retry/backoff, short enough to bound table growth. The
`idx_idem_created` index on `created_at` supports an efficient periodic sweep of
expired keys. **The automated sweeper is intentionally out of scope for this
submission** (it would be a small periodic job / cron deleting
`created_at < now() - interval '72 hours'`); the retention *policy* and the index
that enables it are in place, and nothing in the hot path depends on the sweep.

## 6. Ledger & the audit invariant

The `ledger` table is **append-only**: one row per money movement (`+amount` for a
credit, `-price` for a purchase), tagged with `ref_type`, `ref_id`, and the
originating `idempotency_key`. It is the audit source of truth.

**Invariant:** for every player, `wallets.balance == SUM(ledger.delta)`.

This is both a continuous correctness check and a bug detector. The query

```sql
SELECT w.player_id, w.balance, COALESCE(SUM(l.delta),0) AS ledger_sum
FROM wallets w LEFT JOIN ledger l ON l.player_id = w.player_id
GROUP BY w.player_id, w.balance
HAVING w.balance <> COALESCE(SUM(l.delta),0);
```

returning **zero rows** means no operation was ever half-applied — every
materialized balance is exactly explained by its ledger history. It returns zero
rows after the full concurrency and crash test suites. The same invariant is the
tool that would have caught last week's "double-granted currency" bug (see
RESILIENCE.md).

## 7. Input safety

Validated at the HTTP boundary before any DB work:
- Body parsed with `DisallowUnknownFields`; garbage JSON → `400`.
- `amount` / `price` must be positive integers; `<= 0` → `400`. Decoded into
  `int64`, so values that overflow 64-bit integers fail to parse → `400`.
- Per-operation cap of 1,000,000,000 on amount/price (well under `BIGINT` range,
  leaving vast headroom against balance overflow); over the cap → `400`.
- `itemId` required and capped at 200 chars; `playerId` required on claim.
- Request body capped at 1 MiB via `http.MaxBytesReader`; oversized → `413`.
- Missing `Idempotency-Key` on credit/purchase → `400`.

None of these can crash the service or corrupt a wallet — they reject before a
transaction is opened.

## 8. Limits & known trade-offs

- Idempotency-key sweeper is not automated (Section 5) — documented as future work.
- Integer-only currency (no fractional units) — a deliberate simplification.
- `request_hash` is computed over the raw request bytes, so a legitimate retry must
  resend a byte-identical body. This is stricter than semantic comparison and
  matches normal client retry behaviour.
- Single Postgres instance — durability is local-disk WAL. High availability
  (replication, failover) is out of scope for this assessment.
- Liveness/readiness are split: `/healthz` (process up) and `/readyz` (DB
  reachable, returns `503` if not).

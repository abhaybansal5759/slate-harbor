# slate-harbor — Durable Game Economy Service

A small, crash-durable wallet/economy backend for a game. It handles a player
earning currency, spending it in a shop, and claiming a one-time reward — and is
built so it **never loses or duplicates a player's money or items**, even under
duplicate (retried) requests, concurrent requests racing the same wallet, or a hard
`kill -9` at any moment.

The simulated gameplay (`credit` = a battle payout) is intentionally trivial. The
real engineering is the durability and exactly-once guarantees. See **DESIGN.md**
for the how and why, and **RESILIENCE.md** for the distributed-systems reasoning.

## Stack

- **Go** (stdlib `net/http`, Go 1.25 path wildcards) — single static binary.
- **Postgres 16** (via `pgx/v5`) — all correctness invariants are enforced by the
  database (transactions, row locks, unique/check constraints).
- **Docker / Docker Compose** — the service and its database run together; data
  lives on a named volume so it survives restarts.

## Quick start

Requires Docker Desktop (or Docker Engine + Compose).

```bash
docker compose up --build
```

Wait for the logs to show `schema applied` and `listening on :8080`. Then confirm
the service is ready (this pings Postgres):

```bash
curl http://localhost:8080/readyz
# {"status":"ready"}
```

To stop: `Ctrl+C`, then `docker compose down`. Add `-v` to also wipe the database
volume for a clean slate: `docker compose down -v`.

## API

| Method & path | Body | Effect |
|---|---|---|
| `POST /v1/wallets/{playerId}/credit` | `{"amount": int>0, "reason": str}` | Add currency. Requires `Idempotency-Key` header. |
| `POST /v1/wallets/{playerId}/purchase` | `{"itemId": str, "price": int>0}` | Atomically debit `price` and grant the item. Insufficient funds → `402`, no effect. Requires `Idempotency-Key`. |
| `POST /v1/rewards/{rewardId}/claim` | `{"playerId": str}` | Grant a reward once per player. |
| `GET /v1/wallets/{playerId}` | — | Return `{"balance", "inventory", "claimedRewards"}`. |

Duplicate mutating requests (same `Idempotency-Key`) apply the effect exactly once
and return the same response as the first time. Full contract and status codes are
in DESIGN.md §3.

## Exercising it (curl)

> On Windows PowerShell, `Invoke-RestMethod` is more reliable than curl for JSON
> bodies — equivalents are shown in `examples.ps1`. The curl examples below are for
> bash/macOS/Linux.

```bash
# 1. Credit 100 (note the required Idempotency-Key)
curl -X POST localhost:8080/v1/wallets/p1/credit \
  -H 'Idempotency-Key: k1' -H 'Content-Type: application/json' \
  -d '{"amount":100,"reason":"battle"}'
# {"balance":100}

# 2. Send the SAME request again -> exactly-once: balance stays 100
curl -X POST localhost:8080/v1/wallets/p1/credit \
  -H 'Idempotency-Key: k1' -H 'Content-Type: application/json' \
  -d '{"amount":100,"reason":"battle"}'
# {"balance":100}

# 3. Purchase an item for 120 -> debits + grants atomically
curl -X POST localhost:8080/v1/wallets/p1/credit \
  -H 'Idempotency-Key: k2' -H 'Content-Type: application/json' \
  -d '{"amount":50,"reason":"battle"}'          # top up to 150
curl -X POST localhost:8080/v1/wallets/p1/purchase \
  -H 'Idempotency-Key: buy1' -H 'Content-Type: application/json' \
  -d '{"itemId":"sword","price":120}'
# {"balance":30,"itemId":"sword"}

# 4. Try to overspend -> clean rejection, no partial effect
curl -i -X POST localhost:8080/v1/wallets/p1/purchase \
  -H 'Idempotency-Key: buy2' -H 'Content-Type: application/json' \
  -d '{"itemId":"shield","price":999}'
# HTTP/1.1 402 ... {"error":"insufficient funds","balance":30,"price":999}

# 5. Claim a reward once
curl -X POST localhost:8080/v1/rewards/welcome/claim \
  -H 'Content-Type: application/json' -d '{"playerId":"p1"}'
# {"rewardId":"welcome","playerId":"p1","claimed":true,"alreadyClaimed":false}

# 6. Read final state
curl localhost:8080/v1/wallets/p1
# {"balance":30,"inventory":["sword"],"claimedRewards":["welcome"]}
```

## Tests

### Concurrency + duplicate (integration)
Exercises duplicate and concurrent requests against the same wallet, including the
key case: many purchases racing a balance that affords exactly one must yield
exactly one success and no negative balance.

```bash
docker compose up -d --build      # service must be running
go test ./test/ -v
```

### Crash durability
Credits a wallet, hard-kills the service (`docker kill`, i.e. SIGKILL = `kill -9`),
restarts it, and asserts the committed credit survived and a post-crash retry is
idempotent.

```powershell
# Windows PowerShell
powershell -ExecutionPolicy Bypass -File .\crash_test.ps1
```

### Audit invariant (manual)
For every player, `balance` must equal the sum of that player's ledger entries.
Zero rows = no operation was ever half-applied.

```bash
docker compose exec db psql -U wallet -d wallet -c \
"SELECT w.player_id, w.balance, COALESCE(SUM(l.delta),0) AS ledger_sum \
 FROM wallets w LEFT JOIN ledger l ON l.player_id=w.player_id \
 GROUP BY w.player_id,w.balance HAVING w.balance <> COALESCE(SUM(l.delta),0);"
```

## Project layout

```
slate-harbor/
├── cmd/server/main.go              HTTP handlers, routing, validation, startup
├── internal/db/
│   ├── db.go                       pgx pool, Connect/Migrate, Credit/Purchase/Claim/GetWallet
│   └── migrations/0001_init.sql    wallets, ledger, inventory, claimed_rewards, idempotency_keys
├── test/                           concurrency + duplicate integration tests
├── crash_test.ps1                  kill -9 / restart durability test
├── Dockerfile                      multi-stage build -> distroless runtime image
├── docker-compose.yml              app + postgres:16 with named dbdata volume
├── DESIGN.md                       architecture, datastore choice, isolation, exactly-once
├── RESILIENCE.md                   distributed exactly-once + double-grant bug analysis
└── AI_DISCLOSURE.md                declaration of AI-tool use
```

## Configuration

| Env var | Default | Purpose |
|---|---|---|
| `PORT` | `8080` | HTTP listen port |
| `DATABASE_URL` | `postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable` | Postgres DSN (Compose overrides host to `db`) |

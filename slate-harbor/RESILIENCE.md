# RESILIENCE.md

## 1. Keeping a purchase exactly-once when the item grant is a separate service

Today a purchase debits currency and grants the item inside one Postgres
transaction, so the two either both happen or neither does. The moment the item
grant becomes a network call to a separate inventory service, that guarantee is
gone: I can no longer commit "money left the wallet" and "item was granted" together,
because the inventory service has its own database and can't enlist in my
transaction.

### The partial-failure window

The dangerous gap opens the instant I commit the debit and stays open until I have a
*confirmed* result from the inventory service. If the process crashes, the request
times out, or the network drops anywhere in that window, I genuinely do not know
what happened on the other side. Three bad outcomes are possible:

- **Debited, grant never sent.** The player paid and got nothing.
- **Debited, grant succeeded, but the acknowledgement was lost.** If I blindly
  retry, I grant the item a second time.
- **Debited, grant permanently failed.** Player paid, no item, and no automatic
  recovery.

The naive approach — debit, then make the HTTP call, then mark it done — is broken
precisely because a crash between the debit and the call, or a lost ack after a
successful call, both leave me in one of those states.

### My approach: transactional outbox + an idempotent receiver

I close the window on my side first. In the same local transaction that debits the
wallet, I also insert a row into an **outbox** table describing the grant to perform:
the player, the item, and a unique `grant_id`. Because the debit and this
intent-to-grant commit atomically, I can never end up with a debit that has no
recorded grant to follow. If the process dies right after commit, the debit and the
outbox row are both durably on disk.

A separate worker then polls the outbox for unprocessed rows and calls the inventory
service, **retrying until it gets a definitive success**, and only then marks the row
done. That makes delivery *at-least-once* — retries and re-deliveries are expected.

The other half of the guarantee lives on the receiver: the inventory service must be
**idempotent on `grant_id`**. I send `grant_id` as the dedup key, so whether the
worker sends the grant once, twice, or ten times, the item is granted exactly once.

Put together: **at-least-once delivery plus an idempotent receiver gives me
effectively-once end to end.** I want to be honest about the framing — true
exactly-once across two systems with no shared transaction is impossible in the
strict sense, so the engineering goal is to make duplicates harmless rather than to
prevent them, and the outbox + dedup-key combination does exactly that.

For the case where a grant *permanently* fails (the item no longer exists, the
inventory service rejects it), the worker dead-letters the outbox row and I run a
**compensating credit** — a ledger entry that refunds the player. So the normal path
is forward-retry to completion, and compensation is the exception, not the rule.

### Why this over the alternatives

I considered making **saga/compensation** the primary strategy — debit, attempt the
grant inline, and refund if it fails. I prefer the outbox because the money has
already left the wallet and the *desired* end state is that the player gets their
item; forward-completion should be the main path, with a refund only as the
exceptional fallback. A compensation-first design also exposes a visible
debited-but-not-yet-granted state to the player and needs a careful definition of
"permanent failure" to decide when to give up.

I rejected **two-phase commit** outright: it needs an XA-style coordinator, it
blocks if the coordinator fails at the wrong moment, and most HTTP services
(including a typical inventory service) don't speak it. It's the wrong amount of
machinery for this problem.

### Retention note

Outbox rows and their `grant_id` keys must be kept until the grant is confirmed —
they cannot be swept on the normal 72-hour idempotency-key timer, because an
in-flight grant might still be retrying past that window. The two retention policies
are deliberately decoupled.

---

## 2. The double-granted-currency bug

Suppose a bug last week double-granted currency to some players. I need to find it,
fix it without taking the service down, and ideally have caught it sooner.

### Detecting it online

This is where the append-only ledger earns its place. The invariant from DESIGN.md
is `wallets.balance == SUM(ledger.delta)` per player, but there's an important
subtlety in *which* query actually catches this bug, and it depends on how the bug
manifested:

```sql
-- (a) materialized balance disagrees with ledger history
SELECT w.player_id, w.balance, COALESCE(SUM(l.delta),0) AS ledger_sum
FROM wallets w LEFT JOIN ledger l ON l.player_id = w.player_id
GROUP BY w.player_id, w.balance
HAVING w.balance <> COALESCE(SUM(l.delta),0);

-- (b) the same logical operation landed in the ledger more than once
SELECT idempotency_key, player_id, COUNT(*)
FROM ledger
WHERE idempotency_key IS NOT NULL
GROUP BY idempotency_key, player_id
HAVING COUNT(*) > 1;
```

If the bug bumped the balance twice but only wrote one ledger row, query (a) catches
it — the balance no longer matches the history. But if the bug wrote *two* ledger
rows (the more likely case for a double-apply), the balance and the ledger sum are
both inflated *consistently*, so query (a) passes and looks clean — and query (b),
the duplicate-key detector, is the one that actually surfaces it. I'd run both;
understanding that (a) alone can miss a fully-duplicated grant is the point.

### Correcting it online

No downtime is needed because correction is just reads plus a corrective write. For
each affected player I recompute the **authoritative** balance from the
*deduplicated* ledger — collapsing the duplicate rows so each real operation counts
once — and then write a single correcting adjustment in a transaction to bring the
materialized balance in line.

There's a judgment call on the over-granted currency, and I'd make it explicitly
rather than hide behind "it depends." First, stop the bleeding: deploy the fix (and
the unique constraint below) so no new duplicates can occur. Then for the existing
over-grants — if a player hasn't spent the phantom currency, I claw it back to the
correct balance. If they've already spent it, I generally honor it and absorb the
cost rather than driving their balance negative or clawing back items they bought in
good faith; punishing players for our bug is bad product and bad faith. The firm rule
underneath both cases is that no correction may leave a balance negative.

### What would have caught it sooner

Two layers. **Prevention:** a `UNIQUE` constraint on the idempotency key — which this
service already enforces — makes a double-apply impossible at the source, because the
second insert collides instead of granting again. The bug described here is largely
one that this design's exactly-once mechanism is built to prevent. **Early
detection:** a continuous reconciliation job running both queries above on a schedule
and alerting on any non-zero result, so a divergence is caught in minutes instead of
discovered a week later.

---

## 3. Summary

Both problems — distributed grants and recovering from a bad grant — come back to the
same idea: an append-only ledger as the single source of truth. Because the
authoritative state can always be recomputed and re-checked from that history, I can
close the cross-service failure window with an outbox, and I can detect and repair a
duplication bug online without ever guessing at what the correct balance should be.

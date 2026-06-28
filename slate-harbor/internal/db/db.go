package db

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/0001_init.sql
var schemaSQL string

// WalletView is the read model for GET /v1/wallets/{playerId}.
type WalletView struct {
	Balance        int      `json:"balance"`
	Inventory      []string `json:"inventory"`
	ClaimedRewards []string `json:"claimedRewards"`
}

type CreditResult struct {
	Status int
	Body   []byte
}

// Credit adds currency exactly-once. The idempotency key is claimed in the same
// transaction as the wallet update and ledger append; a duplicate rolls back and
// replays the original response.

func Credit(ctx context.Context, pool *pgxpool.Pool, playerID, idemKey, reqHash string, amount int64, reason string) (CreditResult, error) {
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return CreditResult{}, fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Create the wallet or atomically increment it. The row lock taken by
	//    ON CONFLICT DO UPDATE prevents lost updates under concurrency.
	var newBalance int64
	err = tx.QueryRow(ctx, `
		INSERT INTO wallets (player_id, balance) VALUES ($1, $2)
		ON CONFLICT (player_id)
		DO UPDATE SET balance = wallets.balance + EXCLUDED.balance, updated_at = now()
		RETURNING balance`,
		playerID, amount,
	).Scan(&newBalance)
	if err != nil {
		return CreditResult{}, fmt.Errorf("upsert wallet: %w", err)
	}

	// 2. Append to the append-only ledger (audit source of truth).
	if _, err = tx.Exec(ctx, `
		INSERT INTO ledger (player_id, delta, reason, ref_type, ref_id, idempotency_key)
		VALUES ($1, $2, $3, 'credit', NULL, $4)`,
		playerID, amount, reason, idemKey,
	); err != nil {
		return CreditResult{}, fmt.Errorf("insert ledger: %w", err)
	}

	// 3. The response we intend to return on success.
	body, err := json.Marshal(map[string]int64{"balance": newBalance})
	if err != nil {
		return CreditResult{}, fmt.Errorf("marshal response: %w", err)
	}

	// 4. Claim the key LAST. A PK collision => duplicate request => roll back all
	//    work above and replay the original response.
	if _, err = tx.Exec(ctx, `
		INSERT INTO idempotency_keys (idempotency_key, request_hash, response_status, response_body)
		VALUES ($1, $2, $3, $4)`,
		idemKey, reqHash, 200, body,
	); err != nil {
		if isUniqueViolation(err) {
			tx.Rollback(ctx)
			return replayResponse(ctx, pool, idemKey, reqHash)
		}
		return CreditResult{}, fmt.Errorf("claim idempotency key: %w", err)
	}

	if err = tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return replayResponse(ctx, pool, idemKey, reqHash)
		}
		return CreditResult{}, fmt.Errorf("commit: %w", err)
	}
	return CreditResult{Status: 200, Body: body}, nil
}

// replayResponse returns the stored response for an already-seen key. If the same
// key was reused with a different request body, that's a client bug: reject with 422,
// never apply the effect.
func replayResponse(ctx context.Context, pool *pgxpool.Pool, idemKey, reqHash string) (CreditResult, error) {
	var status int
	var body []byte
	var storedHash string
	if err := pool.QueryRow(ctx, `
		SELECT response_status, response_body, request_hash
		FROM idempotency_keys WHERE idempotency_key = $1`, idemKey,
	).Scan(&status, &body, &storedHash); err != nil {
		return CreditResult{}, fmt.Errorf("load stored response: %w", err)
	}
	if storedHash != reqHash {
		msg, _ := json.Marshal(map[string]string{"error": "idempotency key reused with a different request"})
		return CreditResult{Status: 422, Body: msg}, nil
	}
	return CreditResult{Status: status, Body: body}, nil
}

// isUniqueViolation reports whether err is a Postgres unique-constraint violation (23505).
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// GetWallet returns the wallet view for a player as a single consistent
// snapshot. The bool is true if a wallets row exists; a player who has never
// transacted returns (zero-state view, false, nil) so reads never fail.
func GetWallet(ctx context.Context, pool *pgxpool.Pool, playerID string) (WalletView, bool, error) {
	// init slices empty so they marshal as [] not null
	view := WalletView{Inventory: []string{}, ClaimedRewards: []string{}}

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return view, false, fmt.Errorf("begin read tx: %w", err)
	}
	defer tx.Rollback(ctx) // read-only: rollback is the clean close

	found := true
	err = tx.QueryRow(ctx,
		`SELECT balance FROM wallets WHERE player_id = $1`, playerID,
	).Scan(&view.Balance)
	if errors.Is(err, pgx.ErrNoRows) {
		found = false
	} else if err != nil {
		return view, false, fmt.Errorf("query balance: %w", err)
	}

	invRows, err := tx.Query(ctx,
		`SELECT item_id FROM inventory WHERE player_id = $1 ORDER BY item_id`, playerID)
	if err != nil {
		return view, found, fmt.Errorf("query inventory: %w", err)
	}
	defer invRows.Close()
	for invRows.Next() {
		var item string
		if err := invRows.Scan(&item); err != nil {
			return view, found, fmt.Errorf("scan inventory: %w", err)
		}
		view.Inventory = append(view.Inventory, item)
	}
	if err := invRows.Err(); err != nil {
		return view, found, fmt.Errorf("iterate inventory: %w", err)
	}
	invRows.Close()

	rwdRows, err := tx.Query(ctx,
		`SELECT reward_id FROM claimed_rewards WHERE player_id = $1 ORDER BY reward_id`, playerID)
	if err != nil {
		return view, found, fmt.Errorf("query claimed rewards: %w", err)
	}
	defer rwdRows.Close()
	for rwdRows.Next() {
		var reward string
		if err := rwdRows.Scan(&reward); err != nil {
			return view, found, fmt.Errorf("scan claimed reward: %w", err)
		}
		view.ClaimedRewards = append(view.ClaimedRewards, reward)
	}
	if err := rwdRows.Err(); err != nil {
		return view, found, fmt.Errorf("iterate claimed rewards: %w", err)
	}

	return view, found, nil
}

// Connect opens a pooled connection and verifies the DB is reachable.
func Connect(ctx context.Context, url string) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse db url: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		return nil, fmt.Errorf("ping: %w", err)
	}
	return pool, nil
}

// Migrate applies the schema. All DDL is IF NOT EXISTS, so running it on every
// startup is safe and idempotent — deliberately simpler than a migration framework
// for a service this size. (DESIGN.md: would adopt golang-migrate/goose to evolve schemas.)
func Migrate(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

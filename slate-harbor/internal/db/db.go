package db

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
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

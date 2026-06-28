package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/abhaybansal5759/slate-harbor/internal/db"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	maxBodyBytes    = 1 << 20 // 1 MiB request-body cap
	maxCreditAmount = 1_000_000_000
)

type creditRequest struct {
	Amount int64  `json:"amount"`
	Reason string `json:"reason"`
}

func handleCredit(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		playerID := r.PathValue("playerId")
		if playerID == "" {
			writeJSONError(w, http.StatusBadRequest, "playerId is required")
			return
		}
		idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		if idemKey == "" {
			writeJSONError(w, http.StatusBadRequest, "Idempotency-Key header is required")
			return
		}

		r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		var req creditRequest
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if req.Amount <= 0 {
			writeJSONError(w, http.StatusBadRequest, "amount must be a positive integer")
			return
		}
		if req.Amount > maxCreditAmount {
			writeJSONError(w, http.StatusBadRequest, "amount exceeds maximum allowed")
			return
		}

		// Binds this idempotency key to this exact request body; a key reused with
		// a different body is rejected in replayResponse.
		reqHash := hashRequest("credit", playerID, raw)

		res, err := db.Credit(r.Context(), pool, playerID, idemKey, reqHash, req.Amount, req.Reason)
		if err != nil {
			log.Printf("credit player=%q key=%q: %v", playerID, idemKey, err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(res.Status)
		_, _ = w.Write(res.Body)
	}
}
func hashRequest(scope, playerID string, rawBody []byte) string {
	h := sha256.New()
	h.Write([]byte(scope + "\x00" + playerID + "\x00"))
	h.Write(rawBody)
	return hex.EncodeToString(h.Sum(nil))
}

func main() {
	ctx := context.Background()

	dbURL := getenv("DATABASE_URL", "postgres://wallet:wallet@localhost:5432/wallet?sslmode=disable")
	pool, err := db.Connect(ctx, dbURL)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	defer pool.Close()

	if err := db.Migrate(ctx, pool); err != nil {
		log.Fatalf("migrate: %v", err)
	}
	log.Println("schema applied")

	mux := http.NewServeMux()

	// Liveness: process is up.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, `{"status":"ok"}`)
	})

	mux.HandleFunc("GET /v1/wallets/{playerId}", handleGetWallet(pool))
	mux.HandleFunc("POST /v1/wallets/{playerId}/credit", handleCredit(pool))


	// Readiness: DB is reachable.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		c, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := pool.Ping(c); err != nil {
			writeJSON(w, http.StatusServiceUnavailable, `{"status":"db_unavailable"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"status":"ready"}`)
	})

	addr := ":" + getenv("PORT", "8080")
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func handleGetWallet(pool *pgxpool.Pool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		playerID := r.PathValue("playerId")
		if playerID == "" {
			writeJSONError(w, http.StatusBadRequest, "playerId is required")
			return
		}

		view, _, err := db.GetWallet(r.Context(), pool, playerID)
		// flip to 404 here instead if you chose that:
		//   if !found { writeJSONError(w, http.StatusNotFound, "wallet not found"); return }
		if err != nil {
			log.Printf("get wallet %q: %v", playerID, err)
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(view)
	}
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

package test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var runID = fmt.Sprintf("%d", time.Now().UnixNano()) // makes tests rerunnable on a persistent DB

func baseURL() string {
	if v := os.Getenv("BASE_URL"); v != "" {
		return v
	}
	return "http://localhost:8080"
}

func post(t *testing.T, path, idemKey, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, baseURL()+path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if idemKey != "" {
		req.Header.Set("Idempotency-Key", idemKey)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

func balance(t *testing.T, player string) int64 {
	t.Helper()
	resp, err := http.Get(baseURL() + "/v1/wallets/" + player)
	if err != nil {
		t.Fatalf("get wallet: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Balance int64 `json:"balance"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.Balance
}

// Same idempotency key fired concurrently => the credit applies exactly once.
func TestConcurrentDuplicateCredit(t *testing.T) {
	player := "dupcredit-" + runID
	key := "dupcredit-key-" + runID
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(t, "/v1/wallets/"+player+"/credit", key, `{"amount":100,"reason":"x"}`) }()
	}
	wg.Wait()
	if got := balance(t, player); got != 100 {
		t.Fatalf("duplicate credit applied more than once: balance=%d want 100", got)
	}
}

// Distinct keys fired concurrently => no lost updates, all N apply.
func TestConcurrentDistinctCredit(t *testing.T) {
	player := "distinct-" + runID
	const n = 50
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			post(t, "/v1/wallets/"+player+"/credit", fmt.Sprintf("dk-%s-%d", runID, i), `{"amount":10,"reason":"x"}`)
		}(i)
	}
	wg.Wait()
	if got := balance(t, player); got != n*10 {
		t.Fatalf("lost update: balance=%d want %d", got, n*10)
	}
}

// THE key test: many purchases race a balance that affords exactly one.
// Exactly one 200, the rest 402, balance never negative.
func TestConcurrentPurchaseRace(t *testing.T) {
	player := "race-" + runID
	post(t, "/v1/wallets/"+player+"/credit", "racefund-"+runID, `{"amount":100,"reason":"x"}`)

	const n = 30
	var ok, rejected int64
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			status, _ := post(t, "/v1/wallets/"+player+"/purchase",
				fmt.Sprintf("race-%s-%d", runID, i), `{"itemId":"sword","price":100}`)
			switch status {
			case 200:
				atomic.AddInt64(&ok, 1)
			case 402:
				atomic.AddInt64(&rejected, 1)
			default:
				t.Errorf("unexpected status %d", status)
			}
		}(i)
	}
	wg.Wait()
	if ok != 1 {
		t.Fatalf("double-spend: %d succeeded, want exactly 1", ok)
	}
	if rejected != n-1 {
		t.Fatalf("want %d rejections, got %d", n-1, rejected)
	}
	if b := balance(t, player); b != 0 {
		t.Fatalf("balance should be 0, got %d (negative balance = corruption)", b)
	}
}

// Same key, concurrent purchases => exactly one debit.
func TestConcurrentDuplicatePurchase(t *testing.T) {
	player := "dupbuy-" + runID
	post(t, "/v1/wallets/"+player+"/credit", "dupbuyfund-"+runID, `{"amount":500,"reason":"x"}`)
	key := "dupbuy-key-" + runID
	const n = 40
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); post(t, "/v1/wallets/"+player+"/purchase", key, `{"itemId":"shield","price":100}`) }()
	}
	wg.Wait()
	if b := balance(t, player); b != 400 {
		t.Fatalf("duplicate purchase debited more than once: balance=%d want 400", b)
	}
}
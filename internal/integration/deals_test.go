package integration

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-flash/internal/deals"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
)

// rawDeal inserts a deal directly (below the API) and returns its id.
func (e *env) rawDeal(pieces, perShopper int) uuid.UUID {
	e.t.Helper()
	id := uuid.New()
	_, err := e.pool.Exec(context.Background(), `INSERT INTO deals.deals (id, code, name, aisle, brand, sale_date, price_cents, pieces_total, pieces_left, max_per_shopper, created_at)
		VALUES ($1,$2,'n','s','a',now(),5000,$3,$3,$4,now())`, id, code(), pieces, perShopper)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func reserve(e *env, deal, owner uuid.UUID, pieces int) deals.ReserveOutcome {
	out, err := e.app.Deals.Reserve(context.Background(), uuid.New(), messaging.ReservePieces{OrderID: uuid.New(), OwnerID: owner, DealID: deal, Pieces: pieces, Reference: "R"})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func TestConcurrentReservesNeverOversellAndAlwaysSumToTheStock(t *testing.T) {
	e := newEnv(t)
	deal := e.rawDeal(30, 3)
	var wg sync.WaitGroup
	var mu sync.Mutex
	held := 0
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if reserve(e, deal, uuid.New(), 1) == deals.Held {
				mu.Lock()
				held++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces_left FROM deals.deals WHERE id = $1`, deal).Scan(&left)
	if held != 30 || left != 0 {
		t.Fatalf("60 shoppers, 30 pieces: exactly 30 held and none left, got %d held and %d left", held, left)
	}
	if e.count(`SELECT count(*) FROM deals.piece_holds WHERE deal_id = $1`, deal) != 30 {
		t.Fatal("one hold per success")
	}
}

func TestTheSameShopperRacingThemselvesCannotPassTheCap(t *testing.T) {
	e := newEnv(t)
	deal := e.rawDeal(50, 2)
	owner := uuid.New()
	var wg sync.WaitGroup
	var mu sync.Mutex
	outcomes := map[deals.ReserveOutcome]int{}
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			o := reserve(e, deal, owner, 1)
			mu.Lock()
			outcomes[o]++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if outcomes[deals.Held] != 2 || outcomes[deals.CapReached] != 10 {
		t.Fatalf("a cap of 2 lets exactly 2 through: %v", outcomes)
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces_left FROM deals.deals WHERE id = $1`, deal).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces FROM deals.shoppers WHERE deal_id = $1 AND owner_id = $2`, deal, owner).Scan(&holding)
	if left != 48 || holding != 2 {
		t.Fatalf("refused tries leave no trace: %d left, shopper holds %d", left, holding)
	}
}

func TestACapRefusalDoesNotConsumeStockAndAStockRefusalDoesNotConsumeAllowance(t *testing.T) {
	e := newEnv(t)
	deal := e.rawDeal(3, 2)
	owner := uuid.New()
	if reserve(e, deal, owner, 3) != deals.CapReached {
		t.Fatal("asking for more than the cap in one go is a cap refusal")
	}
	other := uuid.New()
	if reserve(e, deal, other, 2) != deals.Held {
		t.Fatal("2 of 3 held")
	}
	if reserve(e, deal, owner, 2) != deals.Short {
		t.Fatal("1 left, 2 wanted, within the cap: short")
	}
	if reserve(e, deal, owner, 1) != deals.Held {
		t.Fatal("the allowance was not spent by the short try")
	}
	var left int
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces_left FROM deals.deals WHERE id = $1`, deal).Scan(&left)
	if left != 0 {
		t.Fatalf("all three are held: %d", left)
	}
}

func TestReleaseGivesBackStockAndAllowanceExactlyOnce(t *testing.T) {
	e := newEnv(t)
	deal := e.rawDeal(5, 2)
	owner, order := uuid.New(), uuid.New()
	if out, _ := e.app.Deals.Reserve(context.Background(), uuid.New(), messaging.ReservePieces{OrderID: order, OwnerID: owner, DealID: deal, Pieces: 2, Reference: "R"}); out != deals.Held {
		t.Fatal(out)
	}
	for i := 0; i < 3; i++ {
		_, err := e.app.Deals.Release(context.Background(), uuid.New(), messaging.ReleasePieces{OrderID: order, Reason: "refund"})
		if err != nil {
			t.Fatal(err)
		}
	}
	var left, holding int
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces_left FROM deals.deals WHERE id = $1`, deal).Scan(&left)
	_ = e.pool.QueryRow(context.Background(), `SELECT pieces FROM deals.shoppers WHERE deal_id = $1 AND owner_id = $2`, deal, owner).Scan(&holding)
	if left != 5 || holding != 0 {
		t.Fatalf("given back once, not three times: %d left, shopper holds %d", left, holding)
	}
	if e.outbox("deals", messaging.ReleasedKey) != 1 {
		t.Fatal("one released event")
	}
}

func TestOpeningADealValidatesItsCap(t *testing.T) {
	e := newEnv(t)
	adm := e.admin()
	good := map[string]any{"code": code(), "name": "n", "aisle": "s", "brand": "a", "priceCents": 100, "piecesTotal": 10}
	if r := e.post("/api/deals", good, adm.Token); r.Status != 201 || r.Body["maxPerShopper"] != float64(2) {
		t.Fatalf("the cap defaults to 2: %d %s", r.Status, r.Raw)
	}
	bad := map[string]any{"code": code(), "name": "n", "aisle": "s", "brand": "a", "priceCents": 100, "piecesTotal": 10, "maxPerShopper": 11}
	if r := e.post("/api/deals", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap above the pieces on sale: %d", r.Status)
	}
	bad["maxPerShopper"] = 0
	if r := e.post("/api/deals", bad, adm.Token); r.Status != 400 {
		t.Fatalf("a cap of zero: %d", r.Status)
	}
	if r := e.post("/api/deals", good, adm.Token); r.Status != 409 {
		t.Fatalf("the same code twice: %d", r.Status)
	}
	if r := e.post("/api/deals", good, e.register().Token); r.Status != 403 {
		t.Fatalf("a customer cannot open a deal: %d", r.Status)
	}
}

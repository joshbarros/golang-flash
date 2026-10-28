package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
)

func (e *env) hold(s session, deal, reference string, pieces int) reply {
	return e.post("/api/orders", map[string]any{"dealCode": deal, "reference": reference, "pieces": pieces, "hold": true}, s.Token)
}

// expireHolds moves the deadline of every pending hold into the past, so the next sweep takes them back.
func (e *env) expireHolds() {
	e.t.Helper()
	if _, err := e.pool.Exec(context.Background(), `UPDATE deals.piece_holds SET expires_at = now() - interval '1 second' WHERE NOT confirmed AND status = 'Held'`); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) sweep() int {
	e.t.Helper()
	n, err := e.app.Deals.Sweep(context.Background(), time.Now(), 50)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func TestAPendingHoldKeepsItsPiecesAndNoReceiptUntilConfirmed(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := e.hold(ana, deal, "SHP-1", 2).str("id")
	e.drain()
	o := e.order(ana, id)
	if o.str("status") != "Held" || o.Body["holdExpiresAt"] == nil || o.Body["receiptNumber"] != nil {
		t.Fatalf("held with a deadline and no receipt yet: %s", o.Raw)
	}
	if e.piecesLeft(deal) != 3 || e.count(`SELECT count(*) FROM receipts.receipts`) != 0 {
		t.Fatal("the pieces left the shelf at once; nothing is issued while the hold is pending")
	}
	if r := e.post("/api/orders/"+id+"/confirm", nil, ana.Token); r.Status != 202 {
		t.Fatalf("confirm: %d %s", r.Status, r.Raw)
	}
	e.drain()
	o = e.order(ana, id)
	if o.str("status") != "Sold" || o.str("receiptNumber") == "" || e.piecesLeft(deal) != 3 {
		t.Fatalf("confirmed: sold with a receipt, still 3 left: %s", o.Raw)
	}
	if e.sweep() != 0 {
		t.Fatal("a confirmed hold is never swept")
	}
}

func TestAnUnconfirmedHoldExpiresAndGivesBackPiecesAndAllowance(t *testing.T) {
	e := newEnv(t)
	deal := e.openDealCap(5, 2)
	ana := e.register()
	id := e.hold(ana, deal, "SHP-1", 2).str("id")
	e.drain()
	if e.piecesLeft(deal) != 3 {
		t.Fatal("held")
	}
	e.expireHolds()
	if n := e.sweep(); n != 1 {
		t.Fatalf("one hold expired: %d", n)
	}
	e.drain()
	o := e.order(ana, id)
	if o.str("status") != "Expired" || o.str("reason") != "The hold expired before it was confirmed." || o.Body["piecesReleasedAt"] == nil {
		t.Fatalf("expired: %s", o.Raw)
	}
	if e.piecesLeft(deal) != 5 {
		t.Fatal("the pieces are back on the shelf")
	}
	again := e.enter(ana, deal, "SHP-2", 2).str("id") // the allowance came back too: a cap of 2 allows 2 again
	e.drain()
	if status(e, ana, again) != "Sold" {
		t.Fatal("the shopper's allowance came back with the pieces")
	}
	if r := e.post("/api/orders/"+id+"/confirm", nil, ana.Token); r.Status != 409 || r.str("title") != "The hold has expired; start again." {
		t.Fatalf("an expired hold cannot be confirmed: %d %s", r.Status, r.Raw)
	}
}

func TestConfirmingAnOrderWithoutAPendingHoldIsAConflict(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := e.enter(ana, deal, "SHP-1", 1).str("id")
	e.drain()
	if r := e.post("/api/orders/"+id+"/confirm", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a sold order has nothing to confirm: %d", r.Status)
	}
	if r := e.post("/api/orders/"+id+"/confirm", nil, e.register().Token); r.Status != 404 {
		t.Fatalf("someone else gets 404: %d", r.Status)
	}
}

func TestSeveralInstancesSweepingAtOnceExpireEachHoldOnce(t *testing.T) {
	e := newEnv(t)
	deal := e.openDealCap(40, 40)
	var users []session
	for i := 0; i < 20; i++ {
		u := e.register()
		users = append(users, u)
		e.hold(u, deal, "SHP-"+uuid.NewString()[:8], 1)
	}
	e.drain()
	if e.piecesLeft(deal) != 20 {
		t.Fatal("20 pending holds")
	}
	e.expireHolds()
	var wg sync.WaitGroup
	var mu sync.Mutex
	total := 0
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n, err := e.app.Deals.Sweep(context.Background(), time.Now(), 3)
				if err != nil {
					t.Error(err)
					return
				}
				mu.Lock()
				total += n
				mu.Unlock()
				if n == 0 {
					return
				}
			}
		}()
	}
	wg.Wait()
	if total != 20 || e.piecesLeft(deal) != 40 {
		t.Fatalf("four sweepers, twenty holds: each expired exactly once (%d), every piece back (%v)", total, e.piecesLeft(deal))
	}
	e.drain()
	expired := 0
	for _, u := range users {
		var items []map[string]any
		_ = jsonInto(e.do(call{method: "GET", path: "/api/orders", token: u.Token}).Raw, &items)
		if len(items) == 1 && items[0]["status"] == "Expired" {
			expired++
		}
	}
	if expired != 20 {
		t.Fatalf("every order was told: %d", expired)
	}
}

func TestAConfirmAndTheSweeperRacingForTheSameHoldHaveExactlyOneWinner(t *testing.T) {
	confirmed, expired := 0, 0
	for round := 0; round < 12; round++ {
		e := newEnv(t)
		deal := e.openDealCap(5, 5)
		ana := e.register()
		id := uuid.MustParse(e.hold(ana, deal, "SHP-1", 2).str("id"))
		e.drain()
		// the deadline is a hair in the future for the confirm and in the past for the sweep: both may act, one must win
		deadline := time.Now().Add(150 * time.Millisecond)
		if _, err := e.pool.Exec(context.Background(), `UPDATE deals.piece_holds SET expires_at = $1 WHERE order_id = $2`, deadline, id); err != nil {
			t.Fatal(err)
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			time.Sleep(time.Until(deadline) - 2*time.Millisecond)
			_, _ = e.app.Deals.Confirm(context.Background(), uuid.New(), messaging.ConfirmPieces{OrderID: id}, time.Now())
		}()
		go func() {
			defer wg.Done()
			time.Sleep(time.Until(deadline) + time.Millisecond)
			_, _ = e.app.Deals.Sweep(context.Background(), time.Now(), 10)
		}()
		wg.Wait()
		e.drain()
		o := e.order(ana, id.String())
		switch o.str("status") {
		case "Sold":
			confirmed++
			if e.piecesLeft(deal) != 3 {
				t.Fatalf("round %d: sold, so the pieces stay held: %v", round, e.piecesLeft(deal))
			}
		case "Expired":
			expired++
			if e.piecesLeft(deal) != 5 || e.count(`SELECT count(*) FROM receipts.receipts`) != 0 {
				t.Fatalf("round %d: expired, so the pieces are back and no receipt exists: %v", round, e.piecesLeft(deal))
			}
		default:
			t.Fatalf("round %d: exactly one of confirm and expiry must win: %s", round, o.Raw)
		}
	}
	t.Logf("confirm won %d rounds, the sweeper %d", confirmed, expired)
}

func TestARefundOfAPendingHoldGivesBackThePiecesAndTheSweeperLeavesItAlone(t *testing.T) {
	e := newEnv(t)
	deal := e.openDealCap(5, 5)
	ana := e.register()
	id := e.hold(ana, deal, "SHP-1", 3).str("id")
	e.drain()
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Refunded" || e.piecesLeft(deal) != 5 {
		t.Fatal("refunded and the pieces are back")
	}
	e.expireHolds()
	if e.sweep() != 0 {
		t.Fatal("the hold was already released: nothing to expire")
	}
	e.drain()
	if status(e, ana, id) != "Refunded" {
		t.Fatal("the order stays Refunded")
	}
}

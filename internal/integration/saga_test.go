package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
	"github.com/joshbarros/golang-flash/internal/receipts"
)

func (e *env) outbox(schema, key string) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM `+schema+`.outbox_messages WHERE routing_key = $1`, key).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) count(sql string, args ...any) int {
	var n int
	if err := e.pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) piecesLeft(dealCode string) float64 {
	e.t.Helper()
	v, _ := e.get("/api/deals/"+dealCode, e.admin().Token).Body["piecesLeft"].(float64)
	return v
}

func status(e *env, s session, id string) string { return e.order(s, id).str("status") }

func TestTheWholeSagaFromOrderToSoldAndTheReceipt(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(10)
	ana := e.register()
	created := e.enter(ana, deal, "SHP-1", 2)
	id := created.str("id")
	if created.Status != 202 || created.str("status") != "Requested" || created.Body["totalCents"] != float64(11200) {
		t.Fatalf("accepted as Requested with the total, fee included: %s", created.Raw)
	}
	if dead := e.drain(); dead != 0 {
		t.Fatalf("nothing should dead-letter: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Sold" || !strings.HasPrefix(o.str("receiptNumber"), "FL-") || o.Body["reason"] != nil {
		t.Fatalf("sold with a receipt number and no leftover reason: %s", o.Raw)
	}
	if e.piecesLeft(deal) != 8 {
		t.Fatal("2 of 10 pieces are held")
	}
	c := e.get("/api/receipts/"+id, ana.Token)
	if c.Status != 200 || c.str("status") != "Issued" || c.str("number") != o.str("receiptNumber") {
		t.Fatalf("the receipt: %d %s", c.Status, c.Raw)
	}
}

func TestStockRunsOutAndTheLateOrderIsToldHowManyAreLeft(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(3)
	ana, bob := e.register(), e.register()
	a := e.enter(ana, deal, "SHP-A", 2).str("id")
	e.drain()
	b := e.enter(bob, deal, "SHP-B", 2).str("id")
	e.drain()
	if status(e, ana, a) != "Sold" {
		t.Fatal("the first order is sold")
	}
	o := e.order(bob, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Short: 2 pieces wanted, 1 left." {
		t.Fatalf("the second is short: %s", o.Raw)
	}
	if e.get("/api/receipts/"+b, bob.Token).Status != 404 {
		t.Fatal("no receipt for a refused order")
	}
	if e.piecesLeft(deal) != 1 {
		t.Fatal("the refused order took nothing")
	}
}

func TestOneShopperCannotHoldMoreThanTheCapAndARefusedTryCostsNoAllowance(t *testing.T) {
	e := newEnv(t)
	deal := e.openDealCap(10, 2)
	ana := e.register()
	a := e.enter(ana, deal, "SHP-1", 2).str("id")
	e.drain()
	b := e.enter(ana, deal, "SHP-2", 1).str("id")
	e.drain()
	if status(e, ana, a) != "Sold" {
		t.Fatal("up to the cap is fine")
	}
	o := e.order(ana, b)
	if o.str("status") != "Rejected" || o.str("reason") != "Cap: at most 2 pieces per shopper in this deal." {
		t.Fatalf("over the cap: %s", o.Raw)
	}
	if e.piecesLeft(deal) != 8 {
		t.Fatal("the capped order took no stock")
	}
	// the refund gives the allowance back: the same shopper can order again
	if r := e.post("/api/orders/"+a+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	c := e.enter(ana, deal, "SHP-3", 2).str("id")
	e.drain()
	if status(e, ana, c) != "Sold" || e.piecesLeft(deal) != 8 {
		t.Fatalf("after the refund the allowance is back: %s", status(e, ana, c))
	}
}

func TestPoisonIsRejectedAndThePiecesComeBack(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := e.enter(ana, deal, "SHP-POISON-1", 2).str("id")
	if dead := e.drain(); dead != 1 {
		t.Fatalf("the poison receipt is dead-lettered once: %d", dead)
	}
	o := e.order(ana, id)
	if o.str("status") != "Rejected" || !strings.Contains(o.str("reason"), "poison") || o.Body["piecesReleasedAt"] == nil {
		t.Fatalf("rejected, with the reason, and the release recorded: %s", o.Raw)
	}
	if e.piecesLeft(deal) != 5 {
		t.Fatal("the pieces are back")
	}
	if c := e.get("/api/receipts/"+id, ana.Token); c.str("status") != "Rejected" {
		t.Fatalf("the rejection is committed on the receipt: %s", c.Raw)
	}
	// the shopper's allowance came back with them
	again := e.enter(ana, deal, "SHP-2", 2).str("id")
	e.drain()
	if status(e, ana, again) != "Sold" {
		t.Fatal("the allowance is not stuck behind the poison order")
	}
}

func TestRefundAfterSaleReturnsThePiecesButKeepsTheReceipt(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := e.enter(ana, deal, "SHP-1", 2).str("id")
	e.drain()
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatalf("refund: %d %s", r.Status, r.Raw)
	}
	e.drain()
	if status(e, ana, id) != "Refunded" || e.piecesLeft(deal) != 5 {
		t.Fatal("refunded and the pieces are back on the shelf")
	}
	if c := e.get("/api/receipts/"+id, ana.Token); c.str("status") != "Issued" {
		t.Fatal("a refund does not roll back the receipt that was issued")
	}
	if r := e.post("/api/orders/"+id+"/refund", nil, ana.Token); r.Status != 409 {
		t.Fatalf("a second refund is a conflict: %d", r.Status)
	}
}

func TestRefundBeatsAFinalMarkThatArrivesLate(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, deal, "SHP-1", 1).str("id"))
	// run the reserve and the receipt, but stop before the order sees receipt.issued
	for pass := 0; pass < 4; pass++ {
		for _, row := range e.unsent() {
			if row.d.RoutingKey == messaging.IssuedKey {
				continue
			}
			e.markSent(row)
			_, _ = e.deliver(row.d.RoutingKey, row.d)
		}
	}
	if got := status(e, ana, id.String()); got != "Held" {
		t.Fatalf("held, receipt issued, final mark not yet landed: %s", got)
	}
	if r := e.post("/api/orders/"+id.String()+"/refund", nil, ana.Token); r.Status != 202 {
		t.Fatal(r.Status)
	}
	e.drain() // now the final mark arrives
	o := e.order(ana, id.String())
	if o.str("status") != "Refunded" || o.Body["receiptNumber"] != nil {
		t.Fatalf("the late final mark must not undo the refund: %s", o.Raw)
	}
	if c := e.get("/api/receipts/"+id.String(), ana.Token); c.str("status") != "Issued" {
		t.Fatal("the issued receipt stays")
	}
}

func TestADeliveryRepeatedByTheBrokerHasNoEffectTheSecondTime(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	e.enter(ana, deal, "SHP-1", 1)
	rows := e.unsent()
	if len(rows) != 1 {
		t.Fatal("one reserve command")
	}
	d := rows[0].d
	for i := 0; i < 3; i++ {
		if _, err := e.deliver(d.RoutingKey, d); err != nil {
			t.Fatal(err)
		}
	}
	if e.count(`SELECT count(*) FROM deals.piece_holds`) != 1 || e.outbox("deals", messaging.HeldKey) != 1 || e.piecesLeft(deal) != 4 {
		t.Fatal("at-least-once delivery, exactly-once effect: the inbox absorbs the repeats")
	}
}

func TestAFinalMarkBeforeTheHeldMarkIsRetriedNotDealped(t *testing.T) {
	e := newEnv(t)
	deal := e.openDeal(5)
	ana := e.register()
	id := uuid.MustParse(e.enter(ana, deal, "SHP-1", 1).str("id"))
	// the receipt is issued but the Held mark has not reached the order: the order is still Requested
	_, err := e.app.Orders.MarkSold(context.Background(), uuid.New(), messaging.ReceiptIssued{OrderID: id, Number: "FL-TEST"}, 0)
	var ooo *messaging.OutOfOrderError
	if err == nil || !errorsAs(err, &ooo) {
		t.Fatalf("the final mark must ask to be retried, got %v", err)
	}
	if status(e, ana, id.String()) != "Requested" {
		t.Fatal("and change nothing")
	}
	e.drain()
	if status(e, ana, id.String()) != "Sold" {
		t.Fatal("the normal flow still completes")
	}
}

func TestIssueIsIdempotentAndPoisonIsAFlagNotAnException(t *testing.T) {
	e := newEnv(t)
	id, owner := uuid.New(), uuid.New()
	msg := uuid.New()
	held := messaging.PiecesHeld{OrderID: id, OwnerID: owner, DealID: uuid.New(), Pieces: 1, Reference: "SHP-1"}
	first, err := e.app.Receipts.Issue(context.Background(), msg, held)
	again, err2 := e.app.Receipts.Issue(context.Background(), msg, held)
	if err != nil || err2 != nil || first != receipts.Issued || again != receipts.Duplicate {
		t.Fatalf("issue then duplicate: %v %v %v %v", first, err, again, err2)
	}
	poison := messaging.PiecesHeld{OrderID: uuid.New(), OwnerID: owner, DealID: uuid.New(), Pieces: 1, Reference: "xPOISONx"}
	if out, _ := e.app.Receipts.Issue(context.Background(), uuid.New(), poison); out != receipts.Rejected {
		t.Fatal("a poison reference is rejected")
	}
	if e.outbox("receipts", messaging.ReleaseKey) != 1 || e.outbox("receipts", messaging.RejectedKey) != 1 || e.outbox("receipts", messaging.IssuedKey) != 1 {
		t.Fatal("one issued, one release command and one rejection")
	}
}

package orders

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-flash/internal/deals"
	"github.com/joshbarros/golang-flash/internal/platform/httpx"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
	"github.com/joshbarros/golang-flash/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "orders"
	ConsumerName = "orders"
)

// Order statuses.
const (
	Requested = "Requested"
	Held      = "Held"
	Sold      = "Sold"
	Refunded  = "Refunded"
	Rejected  = "Rejected"
	Expired   = "Expired"
)

// Order is the public view of an order.
type Order struct {
	ID               uuid.UUID  `json:"id"`
	OwnerID          uuid.UUID  `json:"ownerId"`
	DealCode         string     `json:"dealCode"`
	Reference        string     `json:"reference"`
	Pieces           int        `json:"pieces"`
	TotalCents       int64      `json:"totalCents"`
	Status           string     `json:"status"`
	Reason           *string    `json:"reason"`
	ReceiptNumber    *string    `json:"receiptNumber"`
	CreatedAt        time.Time  `json:"createdAt"`
	UpdatedAt        time.Time  `json:"updatedAt"`
	PiecesReleasedAt *time.Time `json:"piecesReleasedAt"`
	// HoldExpiresAt is set while the hold waits for the shopper to confirm; past it the pieces go back.
	HoldExpiresAt *time.Time `json:"holdExpiresAt"`
}

// DealCatalog is the one thing Orders may ask the deals module.
type DealCatalog interface {
	Find(ctx context.Context, code string) (*deals.Info, error)
}

// Service is the HTTP side of the saga: it writes the order and its first outbox row in one local transaction.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Deals  DealCatalog
	Log    *slog.Logger

	// FinalMarkDelay is a latency knob for the final mark: Kubernetes sets it so the refund race window is wide enough to hit on purpose.
	FinalMarkDelay time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, catalog DealCatalog, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "orders"}, Deals: catalog, Log: log}
}

const orderColumns = `id, owner_id, deal_code, reference, pieces, total_cents, status, reason, receipt_number, created_at, updated_at, pieces_released_at, hold_expires_at`

func scanOrder(row pgx.Row) (Order, error) {
	var o Order
	err := row.Scan(&o.ID, &o.OwnerID, &o.DealCode, &o.Reference, &o.Pieces, &o.TotalCents, &o.Status, &o.Reason, &o.ReceiptNumber, &o.CreatedAt, &o.UpdatedAt, &o.PiecesReleasedAt, &o.HoldExpiresAt)
	return o, err
}

// CreateRequest is the body of POST /api/orders.
type CreateRequest struct {
	DealCode  string `json:"dealCode"`
	Reference string `json:"reference"`
	Pieces    int64  `json:"pieces"`
	Hold      bool   `json:"hold"`
}

// Create reserves pieces in the deal for the caller: validates, writes the order and its reserve command in one transaction.
func (s *Service) Create(ctx context.Context, caller *httpx.Principal, req CreateRequest) (Order, error) {
	if caller == nil {
		return Order{}, httpx.Fail(http.StatusUnauthorized, "Sign in to book.")
	}
	if strings.TrimSpace(req.DealCode) == "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "Deal code is required.")
	}
	if msg := Validate(req.Reference, req.Pieces); msg != "" {
		return Order{}, httpx.Fail(http.StatusBadRequest, "%s", msg)
	}
	deal, err := s.Deals.Find(ctx, strings.TrimSpace(req.DealCode))
	if err != nil {
		return Order{}, err
	}
	if deal == nil {
		return Order{}, httpx.Fail(http.StatusNotFound, "Deal '%s' does not exist.", req.DealCode)
	}
	now := time.Now().UTC()
	pieces := int(req.Pieces)
	reference := strings.TrimSpace(req.Reference)
	total, err := TotalCents(deal.PriceCents, pieces)
	if err != nil {
		return Order{}, httpx.Fail(http.StatusBadRequest, "The total is too large.")
	}
	id, _ := uuid.NewV7()
	o := Order{ID: id, OwnerID: caller.ID, DealCode: deal.Code, Reference: reference, Pieces: pieces, TotalCents: total, Status: Requested, CreatedAt: now, UpdatedAt: now}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO orders.orders (id, owner_id, deal_id, deal_code, reference, pieces, total_cents, status, created_at, updated_at)
			VALUES ($1,$2,$3,$4,$5,$6,$7,'Requested',$8,$8)`, o.ID, o.OwnerID, deal.ID, o.DealCode, o.Reference, o.Pieces, o.TotalCents, now); err != nil {
			return err
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReserveKey, o.ID,
			messaging.ReservePieces{OrderID: o.ID, OwnerID: caller.ID, DealID: deal.ID, Pieces: pieces, Reference: reference, Hold: req.Hold})
	})
	if err != nil {
		if pgdb.IsUniqueViolation(err) {
			return Order{}, httpx.Fail(http.StatusConflict, "Reference '%s' is already booked on %s.", reference, deal.Code)
		}
		return Order{}, err
	}
	return o, nil
}

// Get returns an order to its owner or to staff. Anyone else gets a 404, so ids cannot be probed.
func (s *Service) Get(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	o, err := scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !caller.CanAccess(o.OwnerID)) {
		return Order{}, httpx.Fail(http.StatusNotFound, "Order not found.")
	}
	return o, err
}

// List returns the caller's orders (staff: everyone's), newest first, a page at a time, with the total.
func (s *Service) List(ctx context.Context, caller *httpx.Principal, dealCode string, page, pageSize int) ([]Order, int, error) {
	if caller == nil {
		return nil, 0, httpx.Fail(http.StatusUnauthorized, "Sign in to see orders.")
	}
	where := []string{"true"}
	args := []any{}
	if !caller.IsStaff() {
		args = append(args, caller.ID)
		where = append(where, "owner_id = $"+strconv.Itoa(len(args)))
	}
	if strings.TrimSpace(dealCode) != "" {
		args = append(args, dealCode)
		where = append(where, "deal_code = $"+strconv.Itoa(len(args)))
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM orders.orders WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`SELECT %s FROM orders.orders WHERE %s ORDER BY created_at DESC, id DESC LIMIT $%d OFFSET $%d`,
		orderColumns, cond, len(args)-1, len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := []Order{}
	for rows.Next() {
		o, err := scanOrder(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, o)
	}
	return items, total, rows.Err()
}

// Confirm asks Deals to pin a pending hold (the shopper pays). It only asks: the hold may still be lost to the expiry
// sweeper, which races it for the same row.
func (s *Service) Confirm(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	o, err := s.Get(ctx, caller, id) // 404 unless the caller may see it
	if err != nil {
		return Order{}, err
	}
	if o.Status != Held || o.HoldExpiresAt == nil {
		if o.Status == Expired {
			return Order{}, httpx.Fail(http.StatusConflict, "The hold has expired; start again.")
		}
		return Order{}, httpx.Fail(http.StatusConflict, "An order in status %s has no pending hold to confirm.", o.Status)
	}
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		return s.Outbox.Enqueue(ctx, tx, messaging.ConfirmKey, id, messaging.ConfirmPieces{OrderID: id})
	})
	return o, err
}

// Refund moves Held or Sold to Refunded and asks Deals to give the pieces back. The receipt is not touched.
func (s *Service) Refund(ctx context.Context, caller *httpx.Principal, id uuid.UUID) (Order, error) {
	if _, err := s.Get(ctx, caller, id); err != nil { // 404 unless the caller may see it
		return Order{}, err
	}
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Refunded', reason = 'Refunded by request.', updated_at = now()
			WHERE id = $1 AND status IN ('Held','Sold')`, id)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			var current string
			if err := tx.QueryRow(ctx, `SELECT status FROM orders.orders WHERE id = $1`, id).Scan(&current); err != nil {
				return err
			}
			return httpx.Fail(http.StatusConflict, "An order in status %s cannot be refunded.", current)
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleaseKey, id, messaging.ReleasePieces{OrderID: id, Reason: "refund"})
	})
	if err != nil {
		return Order{}, err
	}
	return scanOrder(s.Pool.QueryRow(ctx, `SELECT `+orderColumns+` FROM orders.orders WHERE id = $1`, id))
}

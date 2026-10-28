package deals

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
	"github.com/joshbarros/golang-flash/internal/platform/pgdb"
)

// Schema and consumer names.
const (
	Schema       = "deals"
	ConsumerName = "deals"

	HoldHeld     = "Held"
	HoldReleased = "Released"
)

// ReserveOutcome is what a reserve did.
type ReserveOutcome int

// The ways a reserve can end.
const (
	Held ReserveOutcome = iota
	Short
	CapReached
	Duplicate
)

// Service is the deals module's logic. It is the only writer of pieces_left and of the per-shopper counts.
type Service struct {
	Pool   *pgxpool.Pool
	Outbox messaging.Outbox
	Log    *slog.Logger
	// HoldFor is how long a pending hold keeps its pieces (default ten minutes).
	HoldFor time.Duration
}

// NewService builds a Service with its outbox.
func NewService(pool *pgxpool.Pool, log *slog.Logger) *Service {
	return &Service{Pool: pool, Outbox: messaging.Outbox{Schema: Schema, Module: "deals"}, Log: log}
}

// Handle implements messaging.Handler for the deals queue.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	switch d.RoutingKey {
	case messaging.ReserveKey:
		cmd, err := messaging.Parse[messaging.ReservePieces](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Reserve(ctx, d.MessageID, cmd)
		return err
	case messaging.ConfirmKey:
		cmd, err := messaging.Parse[messaging.ConfirmPieces](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Confirm(ctx, d.MessageID, cmd, time.Now())
		return err
	case messaging.ReleaseKey:
		cmd, err := messaging.Parse[messaging.ReleasePieces](d.Body)
		if err != nil {
			return fmt.Errorf("%w: %v", messaging.ErrPoison, err)
		}
		_, err = s.Release(ctx, d.MessageID, cmd)
		return err
	}
	return fmt.Errorf("deals does not handle %q", d.RoutingKey)
}

var errRefused = errors.New("the cap or the stock said no")

// Reserve takes pieces from a deal in two atomic steps inside one transaction: a per-shopper cap upsert that only moves
// while the new total still fits, then one conditional UPDATE that matches only while the remaining stock covers the
// request. If either says no, the whole attempt rolls back (so a cap increment never outlives a failed reserve) and the
// refusal is recorded in a fresh transaction.
func (s *Service) Reserve(ctx context.Context, messageID uuid.UUID, cmd messaging.ReservePieces) (ReserveOutcome, error) {
	outcome := Held
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		// 1. the per-shopper cap: one atomic upsert that only moves while the new total still fits
		capTag, err := tx.Exec(ctx, `
			INSERT INTO deals.shoppers (deal_id, owner_id, pieces)
			SELECT d.id, $2, $3 FROM deals.deals d WHERE d.id = $1 AND d.max_per_shopper >= $3
			ON CONFLICT (deal_id, owner_id) DO UPDATE SET pieces = deals.shoppers.pieces + $3
			WHERE deals.shoppers.pieces + $3 <= (SELECT max_per_shopper FROM deals.deals WHERE id = $1)`,
			cmd.DealID, cmd.OwnerID, cmd.Pieces)
		if err != nil {
			return err
		}
		if capTag.RowsAffected() == 1 {
			// 2. the remaining pieces: one conditional UPDATE, the only writer of pieces_left
			stockTag, err := tx.Exec(ctx, `UPDATE deals.deals SET pieces_left = pieces_left - $2 WHERE id = $1 AND pieces_left >= $2`, cmd.DealID, cmd.Pieces)
			if err != nil {
				return err
			}
			if stockTag.RowsAffected() == 1 {
				now := time.Now().UTC()
				var expiresAt *time.Time
				if cmd.Hold {
					t := now.Add(s.holdFor())
					expiresAt = &t
				}
				if _, err := tx.Exec(ctx, `INSERT INTO deals.piece_holds (order_id, owner_id, deal_id, pieces, status, created_at, expires_at, reference, confirmed)
					VALUES ($1,$2,$3,$4,'Held',$5,$6,$7,$8)`,
					cmd.OrderID, cmd.OwnerID, cmd.DealID, cmd.Pieces, now, expiresAt, cmd.Reference, !cmd.Hold); err != nil {
					return err
				}
				return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.OrderID,
					messaging.PiecesHeld{OrderID: cmd.OrderID, OwnerID: cmd.OwnerID, DealID: cmd.DealID, Pieces: cmd.Pieces, Reference: cmd.Reference, ExpiresAt: expiresAt})
			}
		}
		return errRefused // roll the cap increment back with everything else
	})
	if err == nil {
		return outcome, nil
	}
	if !errors.Is(err, errRefused) {
		return 0, err
	}
	// The refusal is recorded in a fresh transaction (the inbox claim was rolled back with the attempt).
	err = pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		var left, limit int
		_ = tx.QueryRow(ctx, `SELECT pieces_left, max_per_shopper FROM deals.deals WHERE id = $1`, cmd.DealID).Scan(&left, &limit)
		var holding int
		_ = tx.QueryRow(ctx, `SELECT pieces FROM deals.shoppers WHERE deal_id = $1 AND owner_id = $2`, cmd.DealID, cmd.OwnerID).Scan(&holding)
		reason := "stock"
		outcome = Short
		if limit > 0 && holding+cmd.Pieces > limit {
			reason = "cap"
			outcome = CapReached
		}
		return s.Outbox.Enqueue(ctx, tx, messaging.ShortKey, cmd.OrderID,
			messaging.PiecesShort{OrderID: cmd.OrderID, Reference: cmd.Reference, RequestedPieces: cmd.Pieces, PiecesLeft: left, Reason: reason, PerShopperLimit: limit})
	})
	return outcome, err
}

// Release gives held pieces back exactly once, returning the shopper's allowance too. A second release changes nothing.
func (s *Service) Release(ctx context.Context, messageID uuid.UUID, cmd messaging.ReleasePieces) (bool, error) {
	released := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var dealID, ownerID uuid.UUID
		var pieces int
		err = tx.QueryRow(ctx, `
			UPDATE deals.piece_holds SET status = 'Released', released_at = now(), release_reason = $2
			WHERE order_id = $1 AND status = 'Held' RETURNING deal_id, owner_id, pieces`, cmd.OrderID, cmd.Reason).Scan(&dealID, &ownerID, &pieces)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // nothing held, or already released
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deals.deals SET pieces_left = pieces_left + $2 WHERE id = $1 AND pieces_left + $2 <= pieces_total`, dealID, pieces); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE deals.shoppers SET pieces = pieces - $3 WHERE deal_id = $1 AND owner_id = $2 AND pieces >= $3`, dealID, ownerID, pieces); err != nil {
			return err
		}
		released = true
		return s.Outbox.Enqueue(ctx, tx, messaging.ReleasedKey, cmd.OrderID, messaging.PiecesReleased{OrderID: cmd.OrderID, Pieces: pieces, Reason: cmd.Reason})
	})
	return released, err
}

// holdFor is how long a pending hold keeps its pieces.
func (s *Service) holdFor() time.Duration {
	if s.HoldFor > 0 {
		return s.HoldFor
	}
	return 10 * time.Minute
}

// Confirm is the shopper paying: it pins a pending hold. One conditional UPDATE, so it races the sweeper safely: whichever
// reaches the row first wins. Pinning re-announces the hold without an expiry, which is what issues the receipt.
func (s *Service) Confirm(ctx context.Context, messageID uuid.UUID, cmd messaging.ConfirmPieces, now time.Time) (bool, error) {
	pinned := false
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil || !first {
			return err
		}
		var ownerID, dealID uuid.UUID
		var pieces int
		var reference string
		err = tx.QueryRow(ctx, `
			UPDATE deals.piece_holds SET confirmed = true
			WHERE order_id = $1 AND status = 'Held' AND NOT confirmed AND expires_at > $2
			RETURNING owner_id, deal_id, pieces, reference`, cmd.OrderID, now).Scan(&ownerID, &dealID, &pieces, &reference)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil // already expired, released or confirmed
		}
		if err != nil {
			return err
		}
		pinned = true
		return s.Outbox.Enqueue(ctx, tx, messaging.HeldKey, cmd.OrderID,
			messaging.PiecesHeld{OrderID: cmd.OrderID, OwnerID: ownerID, DealID: dealID, Pieces: pieces, Reference: reference})
	})
	return pinned, err
}

// Sweep takes back the pieces of holds nobody confirmed in time. Each hold is claimed by a conditional UPDATE ... RETURNING
// inside one transaction (FOR UPDATE SKIP LOCKED), so two instances, or a confirm, can never both act on the same hold.
// It returns how many holds expired.
func (s *Service) Sweep(ctx context.Context, now time.Time, batch int) (int, error) {
	n := 0
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		rows, err := tx.Query(ctx, `
			UPDATE deals.piece_holds SET status = 'Released', released_at = $1, release_reason = 'expired'
			WHERE order_id IN (
				SELECT order_id FROM deals.piece_holds
				WHERE status = 'Held' AND NOT confirmed AND expires_at <= $1
				ORDER BY expires_at LIMIT $2 FOR UPDATE SKIP LOCKED)
			RETURNING order_id, deal_id, owner_id, pieces`, now, batch)
		if err != nil {
			return err
		}
		type expired struct {
			order, deal, owner uuid.UUID
			pieces             int
		}
		var holds []expired
		for rows.Next() {
			var h expired
			if err := rows.Scan(&h.order, &h.deal, &h.owner, &h.pieces); err != nil {
				rows.Close()
				return err
			}
			holds = append(holds, h)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, h := range holds {
			if _, err := tx.Exec(ctx, `UPDATE deals.deals SET pieces_left = pieces_left + $2 WHERE id = $1 AND pieces_left + $2 <= pieces_total`, h.deal, h.pieces); err != nil {
				return err
			}
			if _, err := tx.Exec(ctx, `UPDATE deals.shoppers SET pieces = pieces - $3 WHERE deal_id = $1 AND owner_id = $2 AND pieces >= $3`, h.deal, h.owner, h.pieces); err != nil {
				return err
			}
			if err := s.Outbox.Enqueue(ctx, tx, messaging.ExpiredKey, h.order, messaging.PiecesExpired{OrderID: h.order, Pieces: h.pieces}); err != nil {
				return err
			}
		}
		n = len(holds)
		return nil
	})
	return n, err
}

// Run sweeps expired holds every interval until the context ends. Every replica runs one.
func (s *Service) Run(ctx context.Context, interval time.Duration) {
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := s.Sweep(ctx, time.Now(), 50); err != nil {
				if ctx.Err() == nil {
					s.Log.Warn("sweeping expired holds failed; it will be tried again", "error", err)
				}
			} else if n > 0 {
				s.Log.Info("expired unconfirmed holds", "count", n)
			}
		}
	}
}

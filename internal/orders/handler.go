package orders

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/joshbarros/golang-flash/internal/platform/messaging"
	"github.com/joshbarros/golang-flash/internal/platform/pgdb"
)

// MarkOutcome is what a mark did.
type MarkOutcome int

// The ways a mark can end.
const (
	Marked MarkOutcome = iota
	Skipped
	Duplicate
)

// Handle consumes the events that move an order forward. Each step runs only from the status it expects.
func (s *Service) Handle(ctx context.Context, d messaging.Delivery) error {
	poison := func(err error) error { return fmt.Errorf("%w: %v", messaging.ErrPoison, err) }
	switch d.RoutingKey {
	case messaging.HeldKey:
		m, err := messaging.Parse[messaging.PiecesHeld](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkHeld(ctx, d.MessageID, m)
		return err
	case messaging.ExpiredKey:
		m, err := messaging.Parse[messaging.PiecesExpired](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkExpired(ctx, d.MessageID, m)
		return err
	case messaging.ShortKey:
		m, err := messaging.Parse[messaging.PiecesShort](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkShort(ctx, d.MessageID, m)
		return err
	case messaging.IssuedKey:
		m, err := messaging.Parse[messaging.ReceiptIssued](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkSold(ctx, d.MessageID, m, s.FinalMarkDelay)
		return err
	case messaging.RejectedKey:
		m, err := messaging.Parse[messaging.ReceiptRejected](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkRejected(ctx, d.MessageID, m)
		return err
	case messaging.ReleasedKey:
		m, err := messaging.Parse[messaging.PiecesReleased](d.Body)
		if err != nil {
			return poison(err)
		}
		_, err = s.MarkReleased(ctx, d.MessageID, m)
		return err
	}
	return fmt.Errorf("orders does not handle %q", d.RoutingKey)
}

// step runs update inside one transaction with the inbox receipt. When update changes nothing and the order is still waiting
// for an earlier mark (Requested), the message arrived too early: it is retried, not dropped.
func (s *Service) step(ctx context.Context, messageID uuid.UUID, orderForOrderCheck *uuid.UUID, update func(pgx.Tx) (int64, error)) (MarkOutcome, error) {
	outcome := Marked
	err := pgdb.Tx(ctx, s.Pool, func(tx pgx.Tx) error {
		first, err := s.Outbox.Claim(ctx, tx, ConsumerName, messageID)
		if err != nil {
			return err
		}
		if !first {
			outcome = Duplicate
			return nil
		}
		changed, err := update(tx)
		if err != nil {
			return err
		}
		if changed == 0 && orderForOrderCheck != nil {
			var status string
			err := tx.QueryRow(ctx, `SELECT status FROM orders.orders WHERE id = $1`, *orderForOrderCheck).Scan(&status)
			if err == nil && status == Requested {
				return &messaging.OutOfOrderError{Reason: "The final mark arrived before the Held mark."}
			}
		}
		if changed == 0 {
			outcome = Skipped
		}
		return nil
	})
	return outcome, err
}

// MarkHeld: the pieces are held. Held marks arrive twice for a pending hold: first with the expiry, again (no expiry) once it
// is confirmed. Only the first changes the status.
func (s *Service) MarkHeld(ctx context.Context, messageID uuid.UUID, m messaging.PiecesHeld) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Held', reason = NULL, hold_expires_at = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.OrderID, m.ExpiresAt)
		return tag.RowsAffected(), err
	})
}

// MarkExpired: the sweeper took the pieces back. Only a Held order expires; one that was refunded or sold stays as it is.
func (s *Service) MarkExpired(ctx context.Context, messageID uuid.UUID, m messaging.PiecesExpired) (MarkOutcome, error) {
	return s.step(ctx, messageID, &m.OrderID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Expired', reason = 'The hold expired before it was confirmed.', pieces_released_at = now(), updated_at = now() WHERE id = $1 AND status = 'Held'`, m.OrderID)
		return tag.RowsAffected(), err
	})
}

// ShortReason words why the reserve was refused.
func ShortReason(m messaging.PiecesShort) string {
	if m.Reason == "cap" {
		return fmt.Sprintf("Cap: at most %d pieces per shopper in this deal.", m.PerShopperLimit)
	}
	return fmt.Sprintf("Short: %d pieces wanted, %d left.", m.RequestedPieces, m.PiecesLeft)
}

// MarkShort: the cap or the stock said no.
func (s *Service) MarkShort(ctx context.Context, messageID uuid.UUID, m messaging.PiecesShort) (MarkOutcome, error) {
	reason := ShortReason(m)
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status = 'Requested'`, m.OrderID, reason)
		return tag.RowsAffected(), err
	})
}

// MarkSold is the final mark. If a refund got there first the row is no longer Held and nothing changes.
func (s *Service) MarkSold(ctx context.Context, messageID uuid.UUID, m messaging.ReceiptIssued, delay time.Duration) (MarkOutcome, error) {
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return Skipped, ctx.Err()
		}
	}
	return s.step(ctx, messageID, &m.OrderID, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Sold', receipt_number = $2, updated_at = now() WHERE id = $1 AND status = 'Held'`, m.OrderID, m.Number)
		return tag.RowsAffected(), err
	})
}

// MarkRejected: the receipt was refused (poison).
func (s *Service) MarkRejected(ctx context.Context, messageID uuid.UUID, m messaging.ReceiptRejected) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET status = 'Rejected', reason = $2, updated_at = now() WHERE id = $1 AND status IN ('Held','Requested')`, m.OrderID, m.Reason)
		return tag.RowsAffected(), err
	})
}

// MarkReleased records that the pieces came back.
func (s *Service) MarkReleased(ctx context.Context, messageID uuid.UUID, m messaging.PiecesReleased) (MarkOutcome, error) {
	return s.step(ctx, messageID, nil, func(tx pgx.Tx) (int64, error) {
		tag, err := tx.Exec(ctx, `UPDATE orders.orders SET pieces_released_at = now() WHERE id = $1 AND pieces_released_at IS NULL`, m.OrderID)
		return tag.RowsAffected(), err
	})
}

var _ = errors.New

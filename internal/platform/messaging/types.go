// Package messaging is the transactional outbox, the inbox, and the RabbitMQ and Kafka plumbing around them.
package messaging

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Names of the exchanges, the Kafka topic and the routing keys. They are the contract between the modules.
const (
	Exchange        = "flash.work"
	DeadLetterExch  = "flash.dlx"
	RequeueExchange = "flash.requeue"
	KafkaTopic      = "flash.events"
	RetryDelayMs    = 2000
	MaxAttempts     = 3
	ReserveKey      = "pieces.reserve"
	ReleaseKey      = "pieces.release"
	HeldKey         = "pieces.held"
	ConfirmKey      = "pieces.confirm"
	ExpiredKey      = "pieces.expired"
	ShortKey        = "pieces.short"
	ReleasedKey     = "pieces.released"
	IssuedKey       = "receipt.issued"
	RejectedKey     = "receipt.rejected"
	DealsQueue      = "deals.commands"
	ReceiptsQueue   = "receipts.work"
	OrdersQueue     = "orders.events"
	PoisonToken     = "POISON"
	attemptHeader   = "x-attempt"
	outboxBatchSize = 50
	relayIdleDelay  = 200 * time.Millisecond
	relayErrorDelay = time.Second
	trimEvery       = 10 * time.Minute
)

// Delivery is one message handed to a Handler.
type Delivery struct {
	MessageID  uuid.UUID
	RoutingKey string
	Body       []byte
}

// Handler consumes one message. Returning ErrPoison dead-letters it at once; any other error retries it.
type Handler interface {
	Handle(ctx context.Context, d Delivery) error
}

// HandlerFunc adapts a function to Handler.
type HandlerFunc func(ctx context.Context, d Delivery) error

// Handle implements Handler.
func (f HandlerFunc) Handle(ctx context.Context, d Delivery) error { return f(ctx, d) }

// ErrPoison marks a message whose rejection is already committed: it must be dead-lettered, not retried.
var ErrPoison = errors.New("poison message")

// OutOfOrderError means the message arrived before the state it depends on; it is retried later.
type OutOfOrderError struct{ Reason string }

func (e *OutOfOrderError) Error() string { return e.Reason }

// IsPoison reports whether the reference carries the poison token (a case-sensitive substring).
func IsPoison(reference string) bool { return strings.Contains(reference, PoisonToken) }

// FailureAction is what a consumer does with a message whose handler failed.
type FailureAction int

// The two things a failed message can meet.
const (
	ActionRetry FailureAction = iota
	ActionDeadLetter
)

// Decide: a poison message is dead-lettered at once; anything else is retried until it has been tried MaxAttempts times.
func Decide(priorAttempts int, err error) FailureAction {
	if errors.Is(err, ErrPoison) {
		return ActionDeadLetter
	}
	if priorAttempts+1 >= MaxAttempts {
		return ActionDeadLetter
	}
	return ActionRetry
}

// ---- the messages ----

// ReservePieces asks the deal to hold pieces.
type ReservePieces struct {
	OrderID   uuid.UUID `json:"orderId"`
	OwnerID   uuid.UUID `json:"ownerId"`
	DealID    uuid.UUID `json:"dealId"`
	Pieces    int       `json:"pieces"`
	Reference string    `json:"reference"`
	Hold      bool      `json:"hold"`
}

// PiecesHeld says the pieces are held. ExpiresAt is set while the hold waits for the shopper to confirm.
type PiecesHeld struct {
	ExpiresAt *time.Time `json:"expiresAt"`
	OrderID   uuid.UUID  `json:"orderId"`
	OwnerID   uuid.UUID  `json:"ownerId"`
	DealID    uuid.UUID  `json:"dealId"`
	Pieces    int        `json:"pieces"`
	Reference string     `json:"reference"`
}

// ConfirmPieces is the shopper paying for a pending hold.
type ConfirmPieces struct {
	OrderID uuid.UUID `json:"orderId"`
}

// PiecesExpired says an unconfirmed hold ran out and its pieces went back on the shelf.
type PiecesExpired struct {
	OrderID uuid.UUID `json:"orderId"`
	Pieces  int       `json:"pieces"`
}

// PiecesShort says the reserve was refused. Reason: "stock" or "cap".
type PiecesShort struct {
	OrderID         uuid.UUID `json:"orderId"`
	Reference       string    `json:"reference"`
	RequestedPieces int       `json:"requestedPieces"`
	PiecesLeft      int       `json:"piecesLeft"`
	// PerShopperLimit is the cap of the deal.
	PerShopperLimit int    `json:"perShopperLimit"`
	Reason          string `json:"reason"`
}

// ReleasePieces asks the deal to give held pieces back.
type ReleasePieces struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// PiecesReleased says pieces (possibly none) were given back.
type PiecesReleased struct {
	OrderID uuid.UUID `json:"orderId"`
	Pieces  int       `json:"pieces"`
	Reason  string    `json:"reason"`
}

// ReceiptIssued says the receipt exists.
type ReceiptIssued struct {
	OrderID uuid.UUID `json:"orderId"`
	Number  string    `json:"number"`
}

// ReceiptRejected says the receipt was refused (the poison rule).
type ReceiptRejected struct {
	OrderID uuid.UUID `json:"orderId"`
	Reason  string    `json:"reason"`
}

// Parse decodes a message body.
func Parse[T any](body []byte) (T, error) {
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		return v, err
	}
	return v, nil
}

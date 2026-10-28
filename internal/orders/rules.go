// Package orders is the HTTP side of the saga and the handler that moves an order forward as events arrive.
package orders

import (
	"errors"
	"fmt"
	"math"
	"strings"
)

// Order rules.
const (
	MaxPiecesPerOrder     = 6
	MaxReferenceLength    = 40
	ServiceFeeBasisPoints = 1_200
)

// ErrOverflow is returned instead of wrapping.
var ErrOverflow = errors.New("arithmetic overflow")

// Validate returns an error message, or "" when the request is acceptable. The piece count is compared as int64 on purpose:
// narrowing 4294967297 to int32 would wrap to 1 and pass.
func Validate(reference string, pieces int64) string {
	trimmed := strings.TrimSpace(reference)
	switch {
	case trimmed == "":
		return "Shopper reference is required."
	case len([]rune(trimmed)) > MaxReferenceLength:
		return fmt.Sprintf("Shopper reference must be at most %d characters.", MaxReferenceLength)
	case pieces < 1:
		return "An order needs at least 1 piece."
	case pieces > MaxPiecesPerOrder:
		return fmt.Sprintf("An order takes at most %d pieces.", MaxPiecesPerOrder)
	}
	return ""
}

// TotalCents is face value plus a 12% service fee truncated to a whole cent. Overflow is an error, not a wrap.
func TotalCents(priceCents int64, pieces int) (int64, error) {
	if priceCents < 0 || pieces < 0 {
		return 0, ErrOverflow
	}
	if pieces != 0 && priceCents > math.MaxInt64/int64(pieces) {
		return 0, ErrOverflow
	}
	face := priceCents * int64(pieces)
	if face > math.MaxInt64/ServiceFeeBasisPoints {
		return 0, ErrOverflow
	}
	fee := face * ServiceFeeBasisPoints / 10_000
	if face > math.MaxInt64-fee {
		return 0, ErrOverflow
	}
	return face + fee, nil
}

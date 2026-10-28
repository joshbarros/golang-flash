package deals

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/joshbarros/golang-flash/internal/platform/httpx"
)

const (
	maxPiecesTotal    = 5000
	maxPriceCents     = 1_000_000
	defaultPerShopper = 2
)

// Deal is the public view of a deal.
type Deal struct {
	ID            uuid.UUID `json:"id"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Aisle         string    `json:"aisle"`
	Brand         string    `json:"brand"`
	SaleDate      time.Time `json:"saleDate"`
	PriceCents    int64     `json:"priceCents"`
	PiecesTotal   int       `json:"piecesTotal"`
	PiecesLeft    int       `json:"piecesLeft"`
	MaxPerShopper int       `json:"maxPerShopper"`
}

// Info is what other modules may ask the deals module (Orders never reads this schema).
type Info struct {
	ID         uuid.UUID
	Code       string
	PriceCents int64
}

// Catalog answers the one cross-module read: a deal's existence and price.
type Catalog struct{ Pool *pgxpool.Pool }

// Find looks a deal up by code. It returns nil when there is none.
func (c Catalog) Find(ctx context.Context, code string) (*Info, error) {
	var i Info
	err := c.Pool.QueryRow(ctx, `SELECT id, code, price_cents FROM deals.deals WHERE code = $1`, code).Scan(&i.ID, &i.Code, &i.PriceCents)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &i, err
}

// API is the deals HTTP surface.
type API struct {
	Pool *pgxpool.Pool
	Auth httpx.Authenticator
	Log  *slog.Logger
}

// Routes registers the deal endpoints; every one needs a signed-in user.
func (a *API) Routes(mux *http.ServeMux) {
	authed := func(h httpx.HandlerFunc) http.HandlerFunc {
		return httpx.Authenticated(a.Log, a.Auth, httpx.Wrap(a.Log, h))
	}
	mux.HandleFunc("GET /api/deals", authed(a.list))
	mux.HandleFunc("GET /api/deals/{code}", authed(a.get))
	mux.HandleFunc("POST /api/deals", httpx.Authenticated(a.Log, a.Auth, httpx.Staff(httpx.Wrap(a.Log, a.create))))
}

const dealSelect = `SELECT id, code, name, aisle, brand, sale_date, price_cents, pieces_total, pieces_left, max_per_shopper FROM deals.deals`

func scanDeal(row pgx.Row) (Deal, error) {
	var d Deal
	err := row.Scan(&d.ID, &d.Code, &d.Name, &d.Aisle, &d.Brand, &d.SaleDate, &d.PriceCents, &d.PiecesTotal, &d.PiecesLeft, &d.MaxPerShopper)
	return d, err
}

func (a *API) list(w http.ResponseWriter, r *http.Request) error {
	rows, err := a.Pool.Query(r.Context(), dealSelect+` ORDER BY sale_date, code`)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Deal{}
	for rows.Next() {
		d, err := scanDeal(rows)
		if err != nil {
			return err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, out)
}

func (a *API) get(w http.ResponseWriter, r *http.Request) error {
	d, err := scanDeal(a.Pool.QueryRow(r.Context(), dealSelect+` WHERE code = $1`, r.PathValue("code")))
	if errors.Is(err, pgx.ErrNoRows) {
		return httpx.Fail(http.StatusNotFound, "Not found.")
	}
	if err != nil {
		return err
	}
	return httpx.JSON(w, http.StatusOK, d)
}

type createRequest struct {
	Code          string     `json:"code"`
	Name          string     `json:"name"`
	Aisle         string     `json:"aisle"`
	Brand         string     `json:"brand"`
	SaleDate      *time.Time `json:"saleDate"`
	PiecesTotal   int64      `json:"piecesTotal"`
	PriceCents    int64      `json:"priceCents"`
	MaxPerShopper *int64     `json:"maxPerShopper"`
}

// create opens a deal. Staff only. This is the only place pieces_left is initialised; after that only the reserve and
// release handlers write it.
func (a *API) create(w http.ResponseWriter, r *http.Request) error {
	var req createRequest
	if err := httpx.Decode(r, &req); err != nil {
		return err
	}
	code := strings.TrimSpace(req.Code)
	if code == "" || len(code) > 20 {
		return httpx.Fail(http.StatusBadRequest, "Code is required and at most 20 characters.")
	}
	if strings.TrimSpace(req.Name) == "" || strings.TrimSpace(req.Aisle) == "" || strings.TrimSpace(req.Brand) == "" {
		return httpx.Fail(http.StatusBadRequest, "Name, aisle and brand are required.")
	}
	if req.PiecesTotal < 1 || req.PiecesTotal > maxPiecesTotal {
		return httpx.Fail(http.StatusBadRequest, "Pieces on sale must be between 1 and %d pieces.", maxPiecesTotal)
	}
	if req.PriceCents < 1 || req.PriceCents > maxPriceCents {
		return httpx.Fail(http.StatusBadRequest, "Price must be between 1 and %d cents per piece.", maxPriceCents)
	}
	limit := int64(defaultPerShopper)
	if req.MaxPerShopper != nil {
		limit = *req.MaxPerShopper
	}
	if limit < 1 || limit > req.PiecesTotal {
		return httpx.Fail(http.StatusBadRequest, "Per-shopper cap must be at least 1 and no more than the pieces on sale.")
	}
	release := time.Now().AddDate(0, 0, 14)
	if req.SaleDate != nil {
		release = *req.SaleDate
	}
	id, _ := uuid.NewV7()
	_, err := a.Pool.Exec(r.Context(), `
		INSERT INTO deals.deals (id, code, name, aisle, brand, sale_date, price_cents, pieces_total, pieces_left, max_per_shopper, created_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,$9,now())`,
		id, code, strings.TrimSpace(req.Name), strings.TrimSpace(req.Aisle), strings.TrimSpace(req.Brand), release, req.PriceCents, req.PiecesTotal, limit)
	if err != nil {
		var coder interface{ SQLState() string }
		if errors.As(err, &coder) && coder.SQLState() == "23505" {
			return httpx.Fail(http.StatusConflict, "Deal '%s' already exists.", code)
		}
		return err
	}
	d, err := scanDeal(a.Pool.QueryRow(r.Context(), dealSelect+` WHERE id = $1`, id))
	if err != nil {
		return err
	}
	w.Header().Set("Location", "/api/deals/"+code)
	return httpx.JSON(w, http.StatusCreated, d)
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 2a): orders service — COD-first checkout from the cart,
// the order state machine, and buyer/seller order views.
//
// Design notes:
//   - Checkout splits a mixed-seller cart into one order per seller
//     (each seller fulfils independently; orders never merge back).
//   - Prices are snapshotted at checkout into order_items: the cart
//     shows live prices, the order is a legal record and never moves
//     with later repricing.
//   - Stock is re-verified per line inside the checkout transaction
//     (inventory row locked FOR UPDATE; available = quantity -
//     reserved), then reserved. Two racing checkouts can never
//     double-book the same unit.
//   - The state machine is role-aware (buyer vs the seller's user):
//     created   --seller--> confirmed --seller--> shipped
//     --buyer/seller--> cancelled
//     confirmed --seller--> cancelled
//     shipped   --seller--> delivered
//     delivered --buyer/seller--> returned
//     Stock side effects: cancel/return releases the reservation;
//     deliver converts it into a sale (quantity and reserved both
//     drop, so a delivered order never holds phantom stock).
//   - COD-first: payment_method is always 'cod' until the F4 payment
//     adapter ships (post-investor onboarding, fail-closed).
package orders

import (
	"context"
	"errors"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/events"
)

// Status is an order lifecycle status.
type Status string

// Order statuses, in lifecycle order.
const (
	StatusCreated   = "created"
	StatusConfirmed = "confirmed"
	StatusShipped   = "shipped"
	StatusDelivered = "delivered"
	StatusCancelled = "cancelled"
	StatusReturned  = "returned"
)

var (
	// ErrCartEmpty: checkout with no buyable lines in the cart.
	ErrCartEmpty = errors.New("orders: cart is empty")
	// ErrOrderNotFound: no order with that id.
	ErrOrderNotFound = errors.New("orders: order not found")
	// ErrForbidden: the caller is neither the buyer nor the seller.
	ErrForbidden = errors.New("orders: not allowed for this order")
	// ErrInvalidStatus: the requested status is not a known status.
	ErrInvalidStatus = errors.New("orders: invalid status")
	// ErrInvalidTransition: the (from, to, role) triple is not allowed.
	ErrInvalidTransition = errors.New("orders: transition not allowed")
	// ErrVariantUnavailable: a checkout line's variant or product is
	// not 'active' at checkout time.
	ErrVariantUnavailable = errors.New("orders: variant not available")
	// ErrInsufficientStock: a checkout line exceeds available stock
	// (quantity - reserved) at checkout time.
	ErrInsufficientStock = errors.New("orders: insufficient stock")
)

type role int

const (
	roleNone role = iota
	roleBuyer
	roleSeller
	// roleSystem is the order-saga orchestrator (F6 part 2b): the only
	// actor allowed to drive COD orders without being the buyer or the
	// seller's user. Its edges are the two created-state moves the saga
	// needs (auto-confirm, compensate-cancel); it can never take any
	// other edge, and it is never assigned via actorRole — only the
	// explicit TransitionSystem method sets it.
	roleSystem
)

// transition is one legal state-machine edge and who may take it.
type transition struct {
	from string
	to   string
	by   []role
}

// transitions is the full, closed edge list — the only transitions
// that exist. Anything else is ErrInvalidTransition.
var transitions = []transition{
	{StatusCreated, StatusConfirmed, []role{roleSeller, roleSystem}},
	{StatusCreated, StatusCancelled, []role{roleBuyer, roleSeller, roleSystem}},
	{StatusConfirmed, StatusShipped, []role{roleSeller}},
	{StatusConfirmed, StatusCancelled, []role{roleSeller}},
	{StatusShipped, StatusDelivered, []role{roleSeller}},
	{StatusDelivered, StatusReturned, []role{roleBuyer, roleSeller}},
}

// knownStatus reports whether s is one of the six statuses.
func knownStatus(s string) bool {
	switch s {
	case StatusCreated, StatusConfirmed, StatusShipped,
		StatusDelivered, StatusCancelled, StatusReturned:
		return true
	}
	return false
}

// Item is one order line: the immutable checkout-time snapshot.
type Item struct {
	VariantID      string `json:"variant_id"`
	ProductID      string `json:"product_id"`
	SKU            string `json:"sku"`
	Title          string `json:"title"`
	PricePaise     int64  `json:"price_paise"`
	MRPPaise       *int64 `json:"mrp_paise,omitempty"`
	Quantity       int    `json:"quantity"`
	LineTotalPaise int64  `json:"line_total_paise"`
}

// Order is the full order view, items included.
type Order struct {
	ID            string    `json:"id"`
	BuyerID       string    `json:"buyer_id"`
	SellerID      string    `json:"seller_id"`
	Status        string    `json:"status"`
	PaymentMethod string    `json:"payment_method"`
	TotalPaise    int64     `json:"total_paise"`
	Items         []*Item   `json:"items"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Summary is the list-view shape (no items): lists must stay cheap.
type Summary struct {
	ID            string    `json:"id"`
	BuyerID       string    `json:"buyer_id"`
	SellerID      string    `json:"seller_id"`
	Status        string    `json:"status"`
	PaymentMethod string    `json:"payment_method"`
	TotalPaise    int64     `json:"total_paise"`
	LineCount     int       `json:"line_count"`
	CreatedAt     time.Time `json:"created_at"`
}

// Service owns the orders tables.
type Service struct {
	pool *db.Pool
	// EventHook fires AFTER the database write commits — once per
	// order from Checkout, once per Transition. The zero value (nil)
	// disables event emission. The hook receives the request context;
	// implementations must detach and bound their own deadline (see
	// cmd/api/main.go) so publishing can never delay the response.
	EventHook func(ctx context.Context, ev events.OrderEvent)
}

// NewService wires the service to the shared pool.
func NewService(pool *db.Pool) *Service { return &Service{pool: pool} }

// emitEvent builds the order event envelope and fires the hook. It
// is a no-op when no hook is installed (unit tests, disabled bus).
func (s *Service) emitEvent(ctx context.Context, event string, o *Order) {
	if s.EventHook == nil {
		return
	}
	s.EventHook(ctx, events.OrderEvent{
		Event:         event,
		EventID:       events.NewEventID(),
		OrderID:       o.ID,
		BuyerID:       o.BuyerID,
		SellerID:      o.SellerID,
		Status:        o.Status,
		TotalPaise:    o.TotalPaise,
		PaymentMethod: o.PaymentMethod,
		OccurredAt:    time.Now().Unix(),
		Version:       1,
	})
}

// lineIn is one verified checkout line (cart data + live snapshot).
type lineIn struct {
	variantID, productID, sellerID pgtype.UUID
	sku, title                     string
	pricePaise                     int64
	mrpPaise                       *int64
	qty                            int
}

// Checkout converts the buyer's cart into one order per seller (COD).
// The whole flow — line verification, stock re-check, reservation,
// order + item writes, cart clear — runs in a single transaction, so
// a failed checkout leaves no partial order and no reserved stock.
func (s *Service) Checkout(ctx context.Context, buyerID pgtype.UUID) ([]*Order, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Live cart lines (buyable join only): archived variants/products
	// cannot be checked out; the buyer sees them as unavailable in the
	// cart view and must remove them first. Explicit is safer than
	// silently dropping lines the buyer meant to order.
	rows, err := tx.Query(ctx, `
		SELECT ci.variant_id, ci.quantity,
		       v.product_id, v.sku, v.price_paise, v.mrp_paise,
		       p.title, p.seller_id
		  FROM cart_items ci
		  JOIN product_variants v ON v.id = ci.variant_id
		  JOIN products p ON p.id = v.product_id
		 WHERE ci.user_id = $1
		   AND v.status = 'active' AND p.status = 'active'
		 ORDER BY p.seller_id, ci.added_at, ci.variant_id`,
		buyerID)
	if err != nil {
		return nil, err
	}
	lines := []*lineIn{}
	for rows.Next() {
		var ln lineIn
		var mrp pgtype.Int8
		if err := rows.Scan(&ln.variantID, &ln.qty,
			&ln.productID, &ln.sku, &ln.pricePaise, &mrp,
			&ln.title, &ln.sellerID); err != nil {
			rows.Close()
			return nil, err
		}
		if mrp.Valid {
			v := mrp.Int64
			ln.mrpPaise = &v
		}
		lines = append(lines, &ln)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, ErrCartEmpty
	}

	// Re-verify stock per line with the inventory row locked: a seller
	// stock-down (or a racing checkout) between cart-add and checkout
	// cannot oversell. Overflow fails the whole checkout (the cart
	// stays intact, the buyer adjusts and retries).
	for _, ln := range lines {
		var quantity, reserved int
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(quantity, 0), COALESCE(reserved, 0)
			  FROM inventory
			 WHERE variant_id = $1
			 FOR UPDATE`, ln.variantID).Scan(&quantity, &reserved)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, ErrInsufficientStock
			}
			return nil, err
		}
		if available := quantity - reserved; ln.qty > available {
			return nil, ErrInsufficientStock
		}
	}

	// Group by seller: one order per seller, deterministic (by seller
	// id, then original cart order — the rows were already ordered).
	groups := map[string][]*lineIn{}
	order := []string{}
	for _, ln := range lines {
		key := ln.sellerID.String()
		if _, seen := groups[key]; !seen {
			order = append(order, key)
		}
		groups[key] = append(groups[key], ln)
	}
	sort.Strings(order)

	out := make([]*Order, 0, len(order))
	for _, key := range order {
		group := groups[key]
		var total int64
		for _, ln := range group {
			total += ln.pricePaise * int64(ln.qty)
		}
		var ord Order
		var id, buyerOut, sellerOut pgtype.UUID
		err := tx.QueryRow(ctx, `
			INSERT INTO orders (buyer_id, seller_id, status, payment_method, total_paise)
			VALUES ($1, $2, 'created', 'cod', $3)
			RETURNING id, buyer_id, seller_id, created_at, updated_at`,
			buyerID, group[0].sellerID, total).Scan(
			&id, &buyerOut, &sellerOut, &ord.CreatedAt, &ord.UpdatedAt)
		if err != nil {
			return nil, err
		}
		ord.ID = id.String()
		ord.BuyerID = buyerOut.String()
		ord.SellerID = sellerOut.String()
		ord.Status = StatusCreated
		ord.PaymentMethod = "cod"
		ord.TotalPaise = total
		ord.Items = make([]*Item, 0, len(group))
		for _, ln := range group {
			lineTotal := ln.pricePaise * int64(ln.qty)
			if _, err := tx.Exec(ctx, `
				INSERT INTO order_items
				       (order_id, variant_id, product_id, sku, title,
				        price_paise, mrp_paise, quantity, line_total_paise)
				VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
				id, ln.variantID, ln.productID, ln.sku, ln.title,
				ln.pricePaise, ln.mrpPaise, ln.qty, lineTotal); err != nil {
				return nil, err
			}
			// Reserve the units: future checkouts see
			// (quantity - reserved) shrink immediately.
			if _, err := tx.Exec(ctx, `
				UPDATE inventory SET reserved = reserved + $2
				 WHERE variant_id = $1`, ln.variantID, ln.qty); err != nil {
				return nil, err
			}
			ord.Items = append(ord.Items, &Item{
				VariantID:      ln.variantID.String(),
				ProductID:      ln.productID.String(),
				SKU:            ln.sku,
				Title:          ln.title,
				PricePaise:     ln.pricePaise,
				MRPPaise:       ln.mrpPaise,
				Quantity:       ln.qty,
				LineTotalPaise: lineTotal,
			})
		}
		out = append(out, &ord)
	}

	// The cart is consumed by checkout: a completed checkout empties
	// it (the row is dropped; items cascade) so re-checkout is
	// naturally impossible.
	if _, err := tx.Exec(ctx, `DELETE FROM carts WHERE user_id = $1`, buyerID); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	// Events fire only after the commit lands: the bus is a
	// notification channel, Postgres is the system of record.
	for _, ord := range out {
		s.emitEvent(ctx, events.OrderCreated, ord)
	}
	return out, nil
}

// actorRole resolves the caller's role for an order: buyer, the
// seller's user, or none (→ ErrForbidden at the call sites).
func actorRole(callerID, buyerID, sellerUserID pgtype.UUID) role {
	if callerID == buyerID {
		return roleBuyer
	}
	if sellerUserID.Valid && callerID == sellerUserID {
		return roleSeller
	}
	return roleNone
}

// edgeFor finds the machine edge (from → to) and reports whether the
// role may take it.
func edgeFor(from, to string, r role) bool {
	for _, t := range transitions {
		if t.from == from && t.to == to {
			for _, allowed := range t.by {
				if allowed == r {
					return true
				}
			}
			return false
		}
	}
	return false
}

// Transition moves an order along the state machine (seller/buyer
// roles enforced) and applies the stock side effects: cancel/return
// releases the reservation; deliver converts it into a sale.
func (s *Service) Transition(ctx context.Context, callerID, orderID pgtype.UUID, to string) (*Order, error) {
	return s.transitionWith(ctx, callerID, orderID, to, false)
}

// TransitionSystem drives an order transition as the order-saga
// orchestrator (F6 part 2b): it bypasses the buyer/seller identity
// check but keeps the state machine and the stock side effects
// intact. Only the created→confirmed (COD auto-confirm) and
// created→cancelled (compensation) edges admit roleSystem — anything
// else is ErrInvalidTransition, so the saga can never take an edge
// the design didn't intend.
func (s *Service) TransitionSystem(ctx context.Context, orderID pgtype.UUID, to string) (*Order, error) {
	return s.transitionWith(ctx, pgtype.UUID{}, orderID, to, true)
}

// transitionWith is the single transition implementation. system=true
// runs as roleSystem (the saga); otherwise the caller's role is
// resolved from their identity as before.
func (s *Service) transitionWith(ctx context.Context, callerID, orderID pgtype.UUID, to string, system bool) (*Order, error) {
	if !knownStatus(to) {
		return nil, ErrInvalidStatus
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var ord Order
	var id, buyerID, sellerID, sellerUserID pgtype.UUID
	var status string
	err = tx.QueryRow(ctx, `
		SELECT o.id, o.buyer_id, o.seller_id, s.user_id, o.status
		  FROM orders o
		  JOIN sellers s ON s.id = o.seller_id
		 WHERE o.id = $1
		 FOR UPDATE OF o`,
		orderID).Scan(&id, &buyerID, &sellerID, &sellerUserID, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrOrderNotFound
		}
		return nil, err
	}
	var r role
	if system {
		r = roleSystem
	} else {
		r = actorRole(callerID, buyerID, sellerUserID)
		if r == roleNone {
			return nil, ErrForbidden
		}
	}
	if !edgeFor(status, to, r) {
		return nil, ErrInvalidTransition
	}

	// Stock side effects, per line (inventory rows locked so a racing
	// transition on a second order cannot interleave arithmetic).
	var items []struct {
		variantID pgtype.UUID
		qty       int
	}
	rows, err := tx.Query(ctx, `
		SELECT variant_id, quantity FROM order_items WHERE order_id = $1`,
		orderID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var it struct {
			variantID pgtype.UUID
			qty       int
		}
		if err := rows.Scan(&it.variantID, &it.qty); err != nil {
			rows.Close()
			return nil, err
		}
		items = append(items, it)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	switch to {
	case StatusCancelled, StatusReturned:
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				UPDATE inventory SET reserved = GREATEST(reserved - $2, 0)
				 WHERE variant_id = $1`, it.variantID, it.qty); err != nil {
				return nil, err
			}
		}
	case StatusDelivered:
		for _, it := range items {
			if _, err := tx.Exec(ctx, `
				UPDATE inventory
				   SET reserved = GREATEST(reserved - $2, 0),
				       quantity = GREATEST(quantity - $2, 0)
				 WHERE variant_id = $1`, it.variantID, it.qty); err != nil {
				return nil, err
			}
		}
	}

	var updatedAt, createdAt time.Time
	var total int64
	err = tx.QueryRow(ctx, `
		UPDATE orders SET status = $2 WHERE id = $1
		RETURNING status, total_paise, created_at, updated_at`,
		orderID, to).Scan(&status, &total, &createdAt, &updatedAt)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	ord.ID = id.String()
	ord.BuyerID = buyerID.String()
	ord.SellerID = sellerID.String()
	ord.Status = status
	ord.PaymentMethod = "cod"
	ord.TotalPaise = total
	ord.CreatedAt = createdAt
	ord.UpdatedAt = updatedAt
	ord.Items, err = s.itemsFor(ctx, orderID)
	if err != nil {
		return nil, err
	}
	// Fire after the commit above: consumers see only durable state.
	s.emitEvent(ctx, events.OrderStatusChanged, &ord)
	return &ord, nil
}

// itemsFor reads the immutable order items (snapshot data).
func (s *Service) itemsFor(ctx context.Context, orderID pgtype.UUID) ([]*Item, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT variant_id, product_id, sku, title,
		       price_paise, mrp_paise, quantity, line_total_paise
		  FROM order_items
		 WHERE order_id = $1
		 ORDER BY variant_id`, orderID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := []*Item{}
	for rows.Next() {
		var it Item
		var variantID, productID pgtype.UUID
		var mrp pgtype.Int8
		if err := rows.Scan(&variantID, &productID, &it.SKU, &it.Title,
			&it.PricePaise, &mrp, &it.Quantity, &it.LineTotalPaise); err != nil {
			return nil, err
		}
		it.VariantID = variantID.String()
		it.ProductID = productID.String()
		if mrp.Valid {
			v := mrp.Int64
			it.MRPPaise = &v
		}
		items = append(items, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

// Get returns the full order view for the buyer or the seller only;
// everyone else gets ErrForbidden.
func (s *Service) Get(ctx context.Context, callerID, orderID pgtype.UUID) (*Order, error) {
	ord, buyerID, sellerUserID, err := s.loadOrder(ctx, orderID)
	if err != nil {
		return nil, err
	}
	if actorRole(callerID, buyerID, sellerUserID) == roleNone {
		return nil, ErrForbidden
	}
	return ord, nil
}

// GetSystem reads the full order view as the order-saga orchestrator
// (F6 part 2b): no buyer/seller identity check. Internal use only —
// never exposed on an HTTP route.
func (s *Service) GetSystem(ctx context.Context, orderID pgtype.UUID) (*Order, error) {
	ord, _, _, err := s.loadOrder(ctx, orderID)
	return ord, err
}

// loadOrder reads the full order view plus the buyer/seller user ids
// the role check needs, without any authorization. Get and GetSystem
// apply their own authorization on top.
func (s *Service) loadOrder(ctx context.Context, orderID pgtype.UUID) (*Order, pgtype.UUID, pgtype.UUID, error) {
	var ord Order
	var id, buyerID, sellerID, sellerUserID pgtype.UUID
	var status string
	var total int64
	var createdAt, updatedAt time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT o.id, o.buyer_id, o.seller_id, s.user_id,
		       o.status, o.total_paise, o.created_at, o.updated_at
		  FROM orders o
		  JOIN sellers s ON s.id = o.seller_id
		 WHERE o.id = $1`,
		orderID).Scan(&id, &buyerID, &sellerID, &sellerUserID,
		&status, &total, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, pgtype.UUID{}, pgtype.UUID{}, ErrOrderNotFound
		}
		return nil, pgtype.UUID{}, pgtype.UUID{}, err
	}
	ord.ID = id.String()
	ord.BuyerID = buyerID.String()
	ord.SellerID = sellerID.String()
	ord.Status = status
	ord.PaymentMethod = "cod"
	ord.TotalPaise = total
	ord.CreatedAt = createdAt
	ord.UpdatedAt = updatedAt
	ord.Items, err = s.itemsFor(ctx, orderID)
	if err != nil {
		return nil, pgtype.UUID{}, pgtype.UUID{}, err
	}
	return &ord, buyerID, sellerUserID, nil
}

// maxListLimit caps list pages: order lists are a dashboard surface,
// not an export API.
const maxListLimit = 50

// ListBuyer returns the caller's orders, newest first.
func (s *Service) ListBuyer(ctx context.Context, buyerID pgtype.UUID, limit, offset int) ([]*Summary, error) {
	limit, offset = clampPage(limit, offset)
	return s.list(ctx, `WHERE o.buyer_id = $1`, buyerID, limit, offset)
}

// ListSeller returns orders for the caller's seller account, newest
// first. A non-seller gets an empty list (not an error): the role is
// advisory on a list endpoint.
func (s *Service) ListSeller(ctx context.Context, callerUserID pgtype.UUID, limit, offset int) ([]*Summary, error) {
	limit, offset = clampPage(limit, offset)
	var sellerID pgtype.UUID
	err := s.pool.QueryRow(ctx,
		`SELECT id FROM sellers WHERE user_id = $1`, callerUserID).Scan(&sellerID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return []*Summary{}, nil
		}
		return nil, err
	}
	return s.list(ctx, `WHERE o.seller_id = $1`, sellerID, limit, offset)
}

func (s *Service) list(ctx context.Context, where string, who pgtype.UUID, limit, offset int) ([]*Summary, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT o.id, o.buyer_id, o.seller_id, o.status,
		       o.total_paise, o.created_at, COUNT(oi.variant_id)
		  FROM orders o
		  LEFT JOIN order_items oi ON oi.order_id = o.id
		`+where+`
		 GROUP BY o.id, o.buyer_id, o.seller_id, o.status,
		          o.total_paise, o.created_at
		 ORDER BY o.created_at DESC, o.id DESC
		 LIMIT $2 OFFSET $3`, who, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Summary{}
	for rows.Next() {
		var sm Summary
		var id, buyerID, sellerID pgtype.UUID
		if err := rows.Scan(&id, &buyerID, &sellerID, &sm.Status,
			&sm.TotalPaise, &sm.CreatedAt, &sm.LineCount); err != nil {
			return nil, err
		}
		sm.ID = id.String()
		sm.BuyerID = buyerID.String()
		sm.SellerID = sellerID.String()
		sm.PaymentMethod = "cod"
		out = append(out, &sm)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func clampPage(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = 20
	}
	if limit > maxListLimit {
		limit = maxListLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

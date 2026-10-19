// SPDX-License-Identifier: AGPL-3.0-or-later
// F3 (part 2a): orders service tests.
//
// Integration tests are gated on TEST_DATABASE_URL (same convention as
// the cart tests): they run the full service against real PG16 and
// skip cleanly otherwise. The error-code mapping test is pure and
// always runs.
package orders

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/cart"
	"github.com/mintzocloud-coder/sutra/services/api/internal/catalog"
	"github.com/mintzocloud-coder/sutra/services/api/internal/db"
	"github.com/mintzocloud-coder/sutra/services/api/internal/users"
)

func testPool(t *testing.T) *db.Pool {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL not set; skipping orders integration test")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pool, err := db.Open(ctx, dsn, 5, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("db.Migrate: %v", err)
	}
	t.Cleanup(func() { pool.Close() })
	return pool
}

func uniqueSubject(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("orders-test-%d-%s", time.Now().UnixNano(), t.Name())
}

func provisionUser(t *testing.T, ctx context.Context, pool *db.Pool, subject string) pgtype.UUID {
	t.Helper()
	u, err := users.Provision(ctx, pool, subject, "")
	if err != nil {
		t.Fatalf("users.Provision: %v", err)
	}
	var id pgtype.UUID
	if err := id.Scan(u.ID); err != nil {
		t.Fatalf("scan user id: %v", err)
	}
	return id
}

func mustUUID(t *testing.T, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := id.Scan(s); err != nil {
		t.Fatalf("bad uuid %q: %v", s, err)
	}
	return id
}

// sellableVariant builds seller → active product → active variant →
// stocked, returning the variant id and the SELLER'S USER id (so the
// test can drive both sides of the state machine).
func sellableVariant(t *testing.T, ctx context.Context, pool *db.Pool, cat *catalog.Service, sellerUser pgtype.UUID, price int64, stock int) pgtype.UUID {
	t.Helper()
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "orders-store-" + tag[len(tag)-8:]
	if _, err := cat.Register(ctx, sellerUser, "Orders Test Store "+tag[len(tag)-6:], slug, ""); err != nil {
		t.Fatalf("Register: %v", err)
	}
	p, err := cat.CreateProduct(ctx, sellerUser, "Orders Widget "+tag[len(tag)-6:], "A widget for orders tests.", "", "active")
	if err != nil {
		t.Fatalf("CreateProduct: %v", err)
	}
	v, err := cat.AddVariant(ctx, sellerUser, mustUUID(t, p.ID), "OSKU-"+tag[len(tag)-10:], price, nil, nil, "")
	if err != nil {
		t.Fatalf("AddVariant: %v", err)
	}
	if _, err := cat.AdjustInventory(ctx, sellerUser, mustUUID(t, v.ID), stock); err != nil {
		t.Fatalf("AdjustInventory: %v", err)
	}
	return mustUUID(t, v.ID)
}

func inventoryOf(t *testing.T, ctx context.Context, pool *db.Pool, variantID pgtype.UUID) (quantity, reserved int) {
	t.Helper()
	if err := pool.QueryRow(ctx,
		`SELECT quantity, reserved FROM inventory WHERE variant_id = $1`,
		variantID).Scan(&quantity, &reserved); err != nil {
		t.Fatalf("inventory read: %v", err)
	}
	return quantity, reserved
}

// checkoutPair provisions a buyer + seller with one stocked variant and
// puts qty units in the buyer's cart, returning everything the state
// machine tests need.
func checkoutPair(t *testing.T, ctx context.Context, pool *db.Pool, price int64, stock int) (cartSvc *cart.Service, svc *Service, buyer, sellerUser pgtype.UUID, variantID pgtype.UUID) {
	t.Helper()
	cat := catalog.NewService(pool)
	cartSvc = cart.NewService(pool)
	svc = NewService(pool)
	sellerUser = provisionUser(t, ctx, pool, uniqueSubject(t))
	buyer = provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID = sellableVariant(t, ctx, pool, cat, sellerUser, price, stock)
	return cartSvc, svc, buyer, sellerUser, variantID
}

func TestCheckoutCOD(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, _, variantID := checkoutPair(t, ctx, pool, 49900, 5)

	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 2); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if len(ords) != 1 {
		t.Fatalf("orders = %d, want 1 (single-seller cart)", len(ords))
	}
	o := ords[0]
	if o.Status != StatusCreated || o.PaymentMethod != "cod" {
		t.Errorf("status/payment = %s/%s, want created/cod", o.Status, o.PaymentMethod)
	}
	if o.TotalPaise != 99800 {
		t.Errorf("total = %d, want 99800", o.TotalPaise)
	}
	if len(o.Items) != 1 || o.Items[0].Quantity != 2 || o.Items[0].PricePaise != 49900 {
		t.Errorf("items = %+v, want 1 line × 2 @ 49900", o.Items)
	}
	if o.Items[0].LineTotalPaise != 99800 {
		t.Errorf("line total = %d, want 99800", o.Items[0].LineTotalPaise)
	}
	if o.Items[0].Title == "" || o.Items[0].SKU == "" {
		t.Errorf("snapshot missing title/sku: %+v", o.Items[0])
	}
	// Cart is consumed by checkout.
	c, err := cartSvc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("cart Get: %v", err)
	}
	if len(c.Items) != 0 || c.ItemCount != 0 {
		t.Errorf("cart not cleared: %+v", c)
	}
	// Stock is reserved, not consumed.
	q, r := inventoryOf(t, ctx, pool, variantID)
	if q != 5 || r != 2 {
		t.Errorf("inventory = (%d, %d), want (5, 2)", q, r)
	}
}

func TestCheckoutMultiSellerSplit(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	cartSvc := cart.NewService(pool)
	svc := NewService(pool)
	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	v1 := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 10000, 10)
	v2 := sellableVariant(t, ctx, pool, cat, provisionUser(t, ctx, pool, uniqueSubject(t)), 20000, 10)

	if _, err := cartSvc.AddItem(ctx, buyer, v1, 1); err != nil {
		t.Fatalf("AddItem v1: %v", err)
	}
	if _, err := cartSvc.AddItem(ctx, buyer, v2, 3); err != nil {
		t.Fatalf("AddItem v2: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if len(ords) != 2 {
		t.Fatalf("orders = %d, want 2 (mixed-seller cart splits)", len(ords))
	}
	totals := map[int64]int{}
	for _, o := range ords {
		if o.SellerID == "" || o.BuyerID == "" {
			t.Errorf("order missing ids: %+v", o)
		}
		totals[o.TotalPaise]++
	}
	if totals[10000] != 1 || totals[60000] != 1 {
		t.Errorf("order totals = %v, want one 10000 and one 60000", totals)
	}
}

func TestCheckoutPriceSnapshot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	cartSvc := cart.NewService(pool)
	svc := NewService(pool)
	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	sellerUser := provisionUser(t, ctx, pool, uniqueSubject(t))
	variantID := sellableVariant(t, ctx, pool, cat, sellerUser, 10000, 10)

	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 2); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	// Seller reprices between cart-add and checkout.
	newPrice := int64(12500)
	if _, err := cat.UpdateVariant(ctx, sellerUser, variantID,
		catalog.VariantPatch{PricePaise: &newPrice}); err != nil {
		t.Fatalf("UpdateVariant: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	o := ords[0]
	if o.TotalPaise != 25000 || o.Items[0].PricePaise != 12500 {
		t.Errorf("snapshot = %d @ %d, want 25000 @ 12500 (checkout-time price)",
			o.TotalPaise, o.Items[0].PricePaise)
	}
}

func TestCheckoutInsufficientStock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cat := catalog.NewService(pool)
	cartSvc, svc, buyer, sellerUser, variantID := checkoutPair(t, ctx, pool, 10000, 5)

	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 4); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	// Stock drops below the cart line between add and checkout.
	if _, err := cat.AdjustInventory(ctx, sellerUser, variantID, -3); err != nil {
		t.Fatalf("AdjustInventory: %v", err)
	}
	if _, err := svc.Checkout(ctx, buyer); !errors.Is(err, ErrInsufficientStock) {
		t.Fatalf("Checkout err = %v, want ErrInsufficientStock", err)
	}
	// The cart stays intact so the buyer can adjust and retry.
	c, err := cartSvc.Get(ctx, buyer)
	if err != nil {
		t.Fatalf("cart Get: %v", err)
	}
	if len(c.Items) != 1 {
		t.Errorf("cart lost the line after failed checkout: %+v", c)
	}
	// Nothing reserved.
	_, r := inventoryOf(t, ctx, pool, variantID)
	if r != 0 {
		t.Errorf("reserved = %d after failed checkout, want 0", r)
	}
}

func TestCheckoutEmptyCart(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	buyer := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Checkout(ctx, buyer); !errors.Is(err, ErrCartEmpty) {
		t.Fatalf("Checkout err = %v, want ErrCartEmpty", err)
	}
}

// machineRoundTrip drives an order through the full lifecycle and
// returns the final order.
func machineRoundTrip(t *testing.T, ctx context.Context, pool *db.Pool, svc *Service, cartSvc *cart.Service, buyer, sellerUser pgtype.UUID, variantID pgtype.UUID, qty int) *Order {
	t.Helper()
	if _, err := cartSvc.AddItem(ctx, buyer, variantID, qty); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	id := mustUUID(t, ords[0].ID)
	for _, to := range []string{StatusConfirmed, StatusShipped, StatusDelivered} {
		o, err := svc.Transition(ctx, sellerUser, id, to)
		if err != nil {
			t.Fatalf("Transition to %s: %v", to, err)
		}
		if o.Status != to {
			t.Fatalf("status = %s, want %s", o.Status, to)
		}
	}
	o, err := svc.Get(ctx, buyer, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return o
}

func TestActorRoleBothRolesPrefersSeller(t *testing.T) {
	// Pure: no database needed. Self-purchase (checkout does not forbid
	// the seller's user from buying their own listings) must resolve to
	// the most-privileged applicable role, or the merchant could never
	// confirm/ship/deliver their own order.
	var buyer, seller, stranger pgtype.UUID
	for i := range 16 {
		buyer.Bytes[i] = byte(i + 1)
		seller.Bytes[i] = byte(i + 65)
		stranger.Bytes[i] = byte(i + 129)
	}
	buyer.Valid, seller.Valid, stranger.Valid = true, true, true
	if got := actorRole(buyer, buyer, seller); got != roleBuyer {
		t.Errorf("buyer-only: got %v, want roleBuyer", got)
	}
	if got := actorRole(seller, buyer, seller); got != roleSeller {
		t.Errorf("seller-only: got %v, want roleSeller", got)
	}
	if got := actorRole(stranger, buyer, seller); got != roleNone {
		t.Errorf("stranger: got %v, want roleNone", got)
	}
	// The self-purchase case: caller is both buyer and the seller's
	// user → seller wins.
	if got := actorRole(seller, seller, seller); got != roleSeller {
		t.Errorf("both roles: got %v, want roleSeller", got)
	}
	// Every buyer edge must also be a seller edge, so preferring the
	// seller never strands the buyer's legitimate moves.
	for _, tr := range transitions {
		for _, r := range tr.by {
			if r == roleBuyer {
				if !edgeFor(tr.from, tr.to, roleSeller) {
					t.Errorf("buyer edge %s→%s has no seller counterpart; buyer-seller resolution would lose it", tr.from, tr.to)
				}
			}
		}
	}
}

func TestOrderSelfPurchaseLifecycle(t *testing.T) {
	// Regression: a merchant buys from their own store (one identity,
	// the exact shape of the F3 smoke harness). Before the
	// actorRole fix, the caller resolved to roleBuyer and the very
	// first seller fulfillment edge (created→confirmed) failed with
	// ErrInvalidTransition.
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, _, sellerUser, variantID := checkoutPair(t, ctx, pool, 10000, 10)

	o := machineRoundTrip(t, ctx, pool, svc, cartSvc, sellerUser, sellerUser, variantID, 3)
	if o.Status != StatusDelivered {
		t.Fatalf("final status = %s, want delivered", o.Status)
	}
	// The merchant can still cancel their own fresh order: the seller
	// edge covers the buyer one.
	if _, err := cartSvc.AddItem(ctx, sellerUser, variantID, 1); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, sellerUser)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if _, err := svc.Transition(ctx, sellerUser, mustUUID(t, ords[0].ID), StatusCancelled); err != nil {
		t.Errorf("self-purchase cancel: %v", err)
	}
}

func TestOrderLifecycleAndStock(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, sellerUser, variantID := checkoutPair(t, ctx, pool, 10000, 10)

	o := machineRoundTrip(t, ctx, pool, svc, cartSvc, buyer, sellerUser, variantID, 3)
	if o.Status != StatusDelivered {
		t.Fatalf("final status = %s, want delivered", o.Status)
	}
	// Delivered converts the reservation into a sale.
	q, r := inventoryOf(t, ctx, pool, variantID)
	if q != 7 || r != 0 {
		t.Errorf("inventory = (%d, %d), want (7, 0)", q, r)
	}
	// Buyer can no longer cancel; seller cannot either.
	if _, err := svc.Transition(ctx, buyer, mustUUID(t, o.ID), StatusCancelled); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("buyer cancel delivered: err = %v, want ErrInvalidTransition", err)
	}
	// But the buyer can return a delivered order; that releases the
	// (already converted) reservation — a no-op on stock arithmetic.
	o2, err := svc.Transition(ctx, buyer, mustUUID(t, o.ID), StatusReturned)
	if err != nil {
		t.Fatalf("return: %v", err)
	}
	if o2.Status != StatusReturned {
		t.Errorf("status = %s, want returned", o2.Status)
	}
	q, r = inventoryOf(t, ctx, pool, variantID)
	if q != 7 || r != 0 {
		t.Errorf("inventory after return = (%d, %d), want (7, 0)", q, r)
	}
}

func TestOrderCancelReleasesReservation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, sellerUser, variantID := checkoutPair(t, ctx, pool, 10000, 10)

	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 4); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	id := mustUUID(t, ords[0].ID)
	if _, err := svc.Transition(ctx, buyer, id, StatusCancelled); err != nil {
		t.Fatalf("buyer cancel: %v", err)
	}
	q, r := inventoryOf(t, ctx, pool, variantID)
	if q != 10 || r != 0 {
		t.Errorf("inventory = (%d, %d), want (10, 0)", q, r)
	}
	// A cancelled order is terminal.
	if _, err := svc.Transition(ctx, sellerUser, id, StatusConfirmed); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("confirm cancelled: err = %v, want ErrInvalidTransition", err)
	}
}

func TestOrderRoles(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, sellerUser, variantID := checkoutPair(t, ctx, pool, 10000, 10)
	stranger := provisionUser(t, ctx, pool, uniqueSubject(t))

	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 1); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	id := mustUUID(t, ords[0].ID)

	// The buyer cannot confirm; only the seller can.
	if _, err := svc.Transition(ctx, buyer, id, StatusConfirmed); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("buyer confirm: err = %v, want ErrInvalidTransition", err)
	}
	// A stranger sees nothing.
	if _, err := svc.Get(ctx, stranger, id); !errors.Is(err, ErrForbidden) {
		t.Errorf("stranger get: err = %v, want ErrForbidden", err)
	}
	if _, err := svc.Transition(ctx, stranger, id, StatusCancelled); !errors.Is(err, ErrForbidden) {
		t.Errorf("stranger cancel: err = %v, want ErrForbidden", err)
	}
	// Bad status names fail before touching the machine.
	if _, err := svc.Transition(ctx, sellerUser, id, "teleported"); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("bad status: err = %v, want ErrInvalidStatus", err)
	}
	// Ghost order id is a 404-shaped error.
	if _, err := svc.Get(ctx, buyer, mustUUID(t, "00000000-0000-0000-0000-000000000000")); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("ghost get: err = %v, want ErrOrderNotFound", err)
	}
	// The seller CAN see the buyer's order (fulfilment view).
	o, err := svc.Get(ctx, sellerUser, id)
	if err != nil {
		t.Fatalf("seller get: %v", err)
	}
	if o.BuyerID == "" || o.SellerID == "" {
		t.Errorf("seller view missing ids: %+v", o)
	}
	// List views agree.
	buyerList, err := svc.ListBuyer(ctx, buyer, 20, 0)
	if err != nil || len(buyerList) != 1 {
		t.Errorf("buyer list = %v, %v, want 1", buyerList, err)
	}
	sellerList, err := svc.ListSeller(ctx, sellerUser, 20, 0)
	if err != nil || len(sellerList) != 1 {
		t.Errorf("seller list = %v, %v, want 1", sellerList, err)
	}
	nonSellerList, err := svc.ListSeller(ctx, stranger, 20, 0)
	if err != nil || len(nonSellerList) != 0 {
		t.Errorf("stranger seller list = %v, %v, want empty", nonSellerList, err)
	}
}

func TestOrderErrorCodes(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrCartEmpty, http.StatusUnprocessableEntity, "cart_empty"},
		{ErrOrderNotFound, http.StatusNotFound, "order_not_found"},
		{ErrForbidden, http.StatusForbidden, "forbidden"},
		{ErrInvalidStatus, http.StatusUnprocessableEntity, "invalid_status"},
		{ErrInvalidTransition, http.StatusUnprocessableEntity, "invalid_transition"},
		{ErrVariantUnavailable, http.StatusUnprocessableEntity, "variant_not_available"},
		{ErrInsufficientStock, http.StatusConflict, "insufficient_stock"},
	}
	for _, tc := range cases {
		status, code := orderErrorCode(tc.err)
		if status != tc.status || code != tc.code {
			t.Errorf("orderErrorCode(%v) = (%d, %q), want (%d, %q)",
				tc.err, status, code, tc.status, tc.code)
		}
	}
	if status, _ := orderErrorCode(errors.New("boom")); status != http.StatusInternalServerError {
		t.Errorf("unknown error status = %d, want 500", status)
	}
}

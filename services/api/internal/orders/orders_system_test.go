// SPDX-License-Identifier: AGPL-3.0-or-later
package orders

// Integration tests for the saga's system-role surface (F6 part 2b):
// GetSystem + TransitionSystem. Gated on TEST_DATABASE_URL, same
// convention as the rest of this package's integration tests.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/mintzocloud-coder/sutra/services/api/internal/cart"
)

// systemOrder builds one checked-out order and returns its id.
func systemOrder(t *testing.T, ctx context.Context, cartSvc *cart.Service, svc *Service, buyer, variantID pgtype.UUID) pgtype.UUID {
	t.Helper()
	if _, err := cartSvc.AddItem(ctx, buyer, variantID, 2); err != nil {
		t.Fatalf("AddItem: %v", err)
	}
	ords, err := svc.Checkout(ctx, buyer)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	if len(ords) != 1 {
		t.Fatalf("orders = %d, want 1", len(ords))
	}
	var id pgtype.UUID
	if err := id.Scan(ords[0].ID); err != nil {
		t.Fatalf("scan order id: %v", err)
	}
	return id
}

// GetSystem returns the full view (order + items) without a caller.
func TestGetSystem_ReturnsOrderAndItems(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, _, variantID := checkoutPair(t, ctx, pool, 49900, 5)
	orderID := systemOrder(t, ctx, cartSvc, svc, buyer, variantID)

	ord, err := svc.GetSystem(ctx, orderID)
	if err != nil {
		t.Fatalf("GetSystem: %v", err)
	}
	if ord.Status != StatusCreated || ord.TotalPaise != 99800 {
		t.Errorf("status/total = %s/%d, want created/99800", ord.Status, ord.TotalPaise)
	}
	if len(ord.Items) != 1 || ord.Items[0].Quantity != 2 {
		t.Errorf("items = %+v, want 1 line × 2", ord.Items)
	}
}

// GetSystem on a ghost id → ErrOrderNotFound (not a role error).
func TestGetSystem_GhostNotFound(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	svc := NewService(pool)
	var ghost pgtype.UUID
	_ = ghost.Scan("00000000-0000-0000-0000-000000000099")
	if _, err := svc.GetSystem(ctx, ghost); !errors.Is(err, ErrOrderNotFound) {
		t.Errorf("GetSystem ghost = %v, want ErrOrderNotFound", err)
	}
}

// TransitionSystem drives created→confirmed (the saga's COD
// auto-confirm) without any user identity.
func TestTransitionSystem_Confirm(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, _, variantID := checkoutPair(t, ctx, pool, 49900, 5)
	orderID := systemOrder(t, ctx, cartSvc, svc, buyer, variantID)

	ord, err := svc.TransitionSystem(ctx, orderID, StatusConfirmed)
	if err != nil {
		t.Fatalf("TransitionSystem confirm: %v", err)
	}
	if ord.Status != StatusConfirmed {
		t.Errorf("status = %s, want confirmed", ord.Status)
	}
}

// The system role cannot take edges the design didn't intend:
// confirmed→shipped stays seller-only.
func TestTransitionSystem_EdgeClosed(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, _, variantID := checkoutPair(t, ctx, pool, 49900, 5)
	orderID := systemOrder(t, ctx, cartSvc, svc, buyer, variantID)

	if _, err := svc.TransitionSystem(ctx, orderID, StatusConfirmed); err != nil {
		t.Fatalf("TransitionSystem confirm: %v", err)
	}
	if _, err := svc.TransitionSystem(ctx, orderID, StatusShipped); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("system confirmed→shipped = %v, want ErrInvalidTransition", err)
	}
}

// TransitionSystem drives created→cancelled (the saga's compensation)
// and the cancel side effect releases the reservation.
func TestTransitionSystem_CancelReleasesReservation(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, _, variantID := checkoutPair(t, ctx, pool, 49900, 5)
	orderID := systemOrder(t, ctx, cartSvc, svc, buyer, variantID)

	if _, err := svc.TransitionSystem(ctx, orderID, StatusCancelled); err != nil {
		t.Fatalf("TransitionSystem cancel: %v", err)
	}
	q, r := inventoryOf(t, ctx, pool, variantID)
	if q != 5 || r != 0 {
		t.Errorf("inventory = (%d, %d), want (5, 0) after system cancel", q, r)
	}
}

// The user-facing Transition is unchanged: the seller still confirms,
// and a stranger is still forbidden.
func TestTransition_RolesUnchanged(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	cartSvc, svc, buyer, sellerUser, variantID := checkoutPair(t, ctx, pool, 49900, 5)
	orderID := systemOrder(t, ctx, cartSvc, svc, buyer, variantID)

	if _, err := svc.Transition(ctx, sellerUser, orderID, StatusConfirmed); err != nil {
		t.Errorf("seller confirm via Transition: %v", err)
	}
	stranger := provisionUser(t, ctx, pool, uniqueSubject(t))
	if _, err := svc.Transition(ctx, stranger, orderID, StatusShipped); !errors.Is(err, ErrForbidden) {
		t.Errorf("stranger transition = %v, want ErrForbidden", err)
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Package logistics defines the logistics-gateway seam for Sutra commerce (F5).
//
// Real logistics-partner onboarding (Delhivery/Shiprocket/etc.) is an
// operational decision that lands after investors: until the company has a
// signed agreement, live API credentials, and seller pickup points
// registered, the only provider is the fail-closed stub. Constructing any
// other provider — or calling any method on the stub — is an explicit,
// typed error, never a silent no-op. This package therefore cannot book a
// pickup, move a parcel, or report a fake delivery in any environment.
//
// v1 is COD-first: most orders will be cash-on-delivery, so the booking
// surface carries COD intent explicitly (COD + CODAmountPaise) and the
// tracking surface reports what the courier collected at the door
// (CODCollectedPaise), which feeds the COD-remittance reconciliation
// fields landing with the shipment tracking model in F5 part 2. Money is
// integer paise everywhere (same convention as internal/payments).
//
// Post-investors wiring: implement Provider against the chosen partner's
// API (shipment booking + label generation, tracking webhooks, pickup
// cancellation), register it in NewProvider under LOGISTICS_PROVIDER, and
// document the onboarding artefacts in docs/logistics.md. The stub must
// keep failing closed even then, so "stub" stays a valid
// LOGISTICS_PROVIDER for tests and dev.
package logistics

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Service levels for a shipment booking.
const (
	// ServiceStandard is surface/ground movement (cheapest, slowest).
	ServiceStandard = "standard"
	// ServiceExpress is air/priority movement.
	ServiceExpress = "express"
)

// Provider is the logistics-gateway seam: shipment booking, tracking,
// and pre-pickup cancellation.
//
// Implementations must be safe to call concurrently and must never log
// full credential values. A nil error means the partner accepted the
// operation — callers treat a nil error on CreateShipment as "the
// courier will attempt pickup", exactly like payments.Provider's
// nil-means-accepted contract.
type Provider interface {
	// CreateShipment books a pickup for an order and returns the
	// courier's shipment + tracking identifiers. A nil error means the
	// partner accepted the booking — never a stub pretending to book.
	CreateShipment(ctx context.Context, p ShipmentParams) (*ShipmentResult, error)
	// Track returns the partner's latest status for a booked shipment.
	// A nil error means "the partner answered", not "the parcel is
	// delivered" — callers read Status, not the error, for outcomes.
	Track(ctx context.Context, trackingID string) (*TrackingUpdate, error)
	// Cancel cancels a booked shipment before pickup. A nil error means
	// the partner accepted the cancellation; post-pickup cancellations
	// must be rejected by the implementation with an error.
	Cancel(ctx context.Context, shipmentID string) error
}

// ShipmentParams describes a shipment the seller wants the courier to
// pick up. OrderRef is the idempotency key at the partner: retried
// bookings reuse it, and a retry must return the existing booking,
// never a duplicate.
type ShipmentParams struct {
	// OrderRef is our internal order id (uuid string) — the idempotency
	// key at the logistics partner.
	OrderRef string
	// SellerID is the Sutra seller id; the partner's pickup-point
	// mapping for it is configured at onboarding.
	SellerID string
	// Service is ServiceStandard or ServiceExpress.
	Service string
	// WeightGrams is the dead weight of the parcel (> 0).
	WeightGrams int64
	// LengthCm, WidthCm, HeightCm are the parcel dimensions in
	// centimetres (> 0) for volumetric-weight billing.
	LengthCm int32
	WidthCm  int32
	HeightCm int32
	// COD marks cash-on-delivery shipments. CODAmountPaise is the amount
	// the courier must collect from the buyer at delivery (0 when
	// COD is false); it feeds the COD-remittance reconciliation in
	// F5 part 2.
	COD            bool
	CODAmountPaise int64
	// Recipient is the delivery address. RecipientPhone is E.164;
	// Pincode is the 6-digit Indian postal code.
	RecipientName  string
	RecipientPhone string
	AddressLine1   string
	AddressLine2   string
	City           string
	State          string
	Pincode        string
}

// ShipmentResult is the partner's answer to CreateShipment.
type ShipmentResult struct {
	// ProviderShipmentID is the partner's shipment/order id.
	ProviderShipmentID string
	// TrackingID is the courier tracking id shown to the buyer.
	TrackingID string
	// CourierName is the partner/courier handling the parcel.
	CourierName string
	// Service echoes the booked service level.
	Service string
	// LabelURL is the partner-hosted shipping-label URL (used by the
	// seller dashboard; empty for the stub, which books nothing).
	LabelURL string
	// EstimatedDeliveryDays is the partner's SLA in days.
	EstimatedDeliveryDays int
}

// TrackingStatus is the normalized lifecycle status of a shipment.
type TrackingStatus string

const (
	// TrackingCreated means the booking exists but no pickup scan yet.
	TrackingCreated TrackingStatus = "created"
	// TrackingPickedUp means the courier collected the parcel.
	TrackingPickedUp TrackingStatus = "picked_up"
	// TrackingInTransit means the parcel is moving between hubs.
	TrackingInTransit TrackingStatus = "in_transit"
	// TrackingOutForDelivery means the parcel is on the delivery run.
	TrackingOutForDelivery TrackingStatus = "out_for_delivery"
	// TrackingDelivered means the buyer received the parcel (or the
	// courier collected COD, whichever applies).
	TrackingDelivered TrackingStatus = "delivered"
	// TrackingFailedDelivery means a delivery attempt failed and the
	// parcel awaits the next attempt or return.
	TrackingFailedDelivery TrackingStatus = "failed_delivery"
	// TrackingCancelled means the booking was cancelled pre-pickup.
	TrackingCancelled TrackingStatus = "cancelled"
	// TrackingReturned means the parcel is being returned to the seller
	// (RTO — common on COD refusal).
	TrackingReturned TrackingStatus = "returned"
)

// TrackingUpdate is the partner's latest word on a shipment.
type TrackingUpdate struct {
	TrackingID string
	// Status is the normalized lifecycle status.
	Status TrackingStatus
	// StatusDetail is the partner's raw scan code (kept for debugging;
	// never parsed by business logic).
	StatusDetail string
	// Location is where the latest scan happened (city/hub name).
	Location string
	// At is the partner's timestamp for the latest scan.
	At time.Time
	// CODCollectedPaise is what the courier collected from the buyer at
	// delivery (> 0 only on COD deliveries in "delivered"); the F5 part 2
	// remittance model reconciles this against CODAmountPaise booked.
	CODCollectedPaise int64
}

// ErrLogisticsDisabled is returned by every StubProvider method. It is a
// distinct type (not a sentinel) so callers and logs can distinguish
// "deliberately unwired" from transport failures — exactly like
// otp.ErrSMSSendingDisabled and payments.ErrPaymentsDisabled.
type ErrLogisticsDisabled struct {
	// Reason explains why logistics operations are disabled in this
	// environment.
	Reason string
}

func (e *ErrLogisticsDisabled) Error() string {
	return "logistics: shipment operations disabled: " + e.Reason
}

// StubProvider is the fail-closed Provider: every method returns
// ErrLogisticsDisabled. It exists so the logistics seam is fully wired
// (config → constructor → call sites) long before a partner is
// onboarded, without any code path that could book a pickup, fake a
// delivery, or invent COD collection.
type StubProvider struct{}

var errStubDisabled = &ErrLogisticsDisabled{
	Reason: "no logistics partner is wired — partner onboarding is " +
		"post-investors (see docs/logistics.md)",
}

// CreateShipment always fails closed — it never returns a usable booking.
func (StubProvider) CreateShipment(_ context.Context, _ ShipmentParams) (*ShipmentResult, error) {
	return nil, errStubDisabled
}

// Track always fails closed — it never invents a status.
func (StubProvider) Track(_ context.Context, _ string) (*TrackingUpdate, error) {
	return nil, errStubDisabled
}

// Cancel always fails closed — it never pretends to cancel.
func (StubProvider) Cancel(_ context.Context, _ string) error {
	return errStubDisabled
}

// ProviderConfig carries the future partner credentials. Until a
// logistics onboarding completes, the values are loaded, never logged
// (not even the API key), and ignored by the stub — a real provider name
// refuses to boot regardless of whether keys are present.
type ProviderConfig struct {
	APIKey string
	Secret string
	// Env is "sandbox" or "prod"; "" means unset. The real adapter will
	// refuse to boot without APIKey+Secret; sandbox-vs-prod selects the
	// endpoint it talks to.
	Env string
}

var errUnknownProvider = errors.New("logistics: unknown logistics provider")

// NewProvider builds the Provider for name with the partner credentials.
// Only "stub" (and the empty default, which maps to it) exists until a
// logistics partner is onboarded; anything else — including "delhivery"
// or "shiprocket" — is a fail-closed error so a misconfigured
// LOGISTICS_PROVIDER refuses to boot instead of silently booking
// nothing. A malformed LOGISTICS_ENV is likewise a boot-time error: bad
// config must shout, never be silently ignored.
func NewProvider(name string, cfg ProviderConfig) (Provider, error) {
	switch cfg.Env {
	case "", "sandbox", "prod":
		// Valid: unset (dev default) or an explicit environment.
	default:
		return nil, fmt.Errorf("logistics: invalid LOGISTICS_ENV %q: must be \"sandbox\" or \"prod\"", cfg.Env)
	}
	switch name {
	case "", "stub":
		return StubProvider{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (logistics partner onboarding is post-investors; until then only \"stub\" boots — see docs/logistics.md)",
			errUnknownProvider, name)
	}
}

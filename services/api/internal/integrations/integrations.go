// SPDX-License-Identifier: AGPL-3.0-or-later
// Package integrations defines the business-integrations seam for Sutra's
// Wave-5 business tier (H2): WhatsApp Cloud API (DIRECT, no BSP), the
// Tally on-premise bridge, and Zoho Books.
//
// Real credentials for all three are post-investors: until the company
// has signed agreements and live keys, the only registered provider for
// each seam is the fail-closed stub. Constructing a real provider name —
// or calling any method on any stub — is an explicit, typed error, never
// a silent no-op. This package therefore cannot send a WhatsApp message,
// post a Tally voucher, or create a Zoho invoice in any environment.
//
// WhatsApp (direct, no BSP): Meta's Cloud API is the only route — no
// business-service-provider middlemen. Messages go through graph API
// version v22.0+ with a WhatsApp Business Account phone number id. No
// account exists yet; the onboarding checklist is in docs/integrations.md.
//
// Tally (on-premise bridge): Tally Prime exposes its XML-over-HTTP API
// only on the merchant's own machine. The bridge is a small local agent
// the seller installs (design in docs/integrations.md) that accepts
// signed jobs from this seam and posts vouchers to Tally; until that
// agent is deployed at a merchant, the seam fails closed.
//
// Zoho Books (default): the accounting ledger for Indian merchants.
// OAuth 2.0 via a Zoho self-client, INR organization. Onboarding is
// post-investors; until then the seam fails closed.
//
// Post-investors wiring: implement WhatsAppProvider against the Meta
// Cloud API, TallyBridge against the bridge-agent HTTP contract, and
// ZohoBooksProvider against the Zoho Books v3 API; register each under
// its env var; document the onboarding artefacts in
// docs/integrations.md. The stubs must keep failing closed even then,
// so "stub" stays a valid provider name for tests and dev.
package integrations

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrIntegrationDisabled is returned by every stub method and by every
// real-provider constructor while the integration is unwired. It is a
// distinct type (not a sentinel) so callers and logs can distinguish
// "deliberately unwired" from transport failures — exactly like
// otp.ErrSMSSendingDisabled and payments.ErrPaymentsDisabled.
type ErrIntegrationDisabled struct {
	// Integration names the seam ("whatsapp", "tally", "zoho-books").
	Integration string
	// Reason explains why the integration is disabled in this
	// environment; it always points at docs/integrations.md.
	Reason string
}

func (e *ErrIntegrationDisabled) Error() string {
	return "integrations: " + e.Integration + " disabled: " + e.Reason
}

func disabled(integration, reason string) *ErrIntegrationDisabled {
	return &ErrIntegrationDisabled{Integration: integration, Reason: reason}
}

var errUnknownProvider = errors.New("integrations: unknown provider")

// ---------------------------------------------------------------------------
// WhatsApp Cloud API (direct, no BSP)
// ---------------------------------------------------------------------------

// WhatsAppPhoneLength caps recipient numbers to a sane E.164 length.
const WhatsAppPhoneLength = 16

// ValidatePhone enforces E.164-ish shape for WhatsApp recipients:
// a leading '+' followed by 8–15 digits. Meta's API rejects the rest,
// so the real provider re-validates; the stub rejects nothing — it
// refuses the whole call.
func ValidatePhone(phone string) error {
	if len(phone) < 9 || len(phone) > WhatsAppPhoneLength || !strings.HasPrefix(phone, "+") {
		return fmt.Errorf("invalid phone %q: want E.164 (+<8-15 digits>)", phone)
	}
	for _, r := range phone[1:] {
		if r < '0' || r > '9' {
			return fmt.Errorf("invalid phone %q: want E.164 (+<8-15 digits>)", phone)
		}
	}
	return nil
}

// VerifyWebhookSignature validates a Meta webhook callback: Meta signs
// the raw request body with the app secret as HMAC-SHA256 and sends it
// in the X-Hub-Signature-256 header as "sha256=<hex>". This is pure
// crypto — it works with no network and no credentials beyond the
// verify secret, so it is real code, not a stub seam. It never logs the
// secret or the full signature.
func VerifyWebhookSignature(rawBody []byte, appSecret, header string) error {
	const prefix = "sha256="
	if appSecret == "" {
		return errors.New("integrations: webhook verify secret is unset — refusing to accept any callback")
	}
	if !strings.HasPrefix(header, prefix) {
		return errors.New("integrations: bad X-Hub-Signature-256: missing sha256= prefix")
	}
	sig, err := hex.DecodeString(strings.TrimPrefix(header, prefix))
	if err != nil {
		return errors.New("integrations: bad X-Hub-Signature-256: not hex")
	}
	mac := hmac.New(sha256.New, []byte(appSecret))
	mac.Write(rawBody)
	if !hmac.Equal(mac.Sum(nil), sig) {
		return errors.New("integrations: webhook signature mismatch")
	}
	return nil
}

// WhatsAppText is a free-form text message. Meta permits free-form
// messages only inside the 24-hour customer-service window; outside it
// the real provider sends an approved template instead — the stub
// refuses either way.
type WhatsAppText struct {
	To   string
	Body string
}

// TemplateComponent is one named parameter block of an approved
// template (body/header/button parameters per Meta's template API).
type TemplateComponent struct {
	Type       string   // "body", "header", "button"
	Parameters []string // ordered text parameters
}

// WhatsAppTemplate is a pre-approved marketing/utility template send.
type WhatsAppTemplate struct {
	To         string
	Name       string // approved template name
	Language   string // e.g. "en", "hi", "gu"
	Components []TemplateComponent
}

// WhatsAppProvider is the Meta Cloud API seam: order updates and
// support replies to buyers who opted in. Implementations must be safe
// to call concurrently and must never log the access token. A nil
// error means Meta accepted the message (message id returned).
type WhatsAppProvider interface {
	SendText(ctx context.Context, m WhatsAppText) (messageID string, err error)
	SendTemplate(ctx context.Context, m WhatsAppTemplate) (messageID string, err error)
}

// WhatsAppConfig carries the future Meta credentials. Until the
// post-investors onboarding they are loaded, never logged, and ignored
// by the stub — a real provider name refuses to boot regardless of
// whether keys are present.
type WhatsAppConfig struct {
	PhoneNumberID string
	AccessToken   string
	// WebhookVerifySecret is the Meta app secret used to HMAC-check
	// inbound webhook callbacks (see VerifyWebhookSignature).
	WebhookVerifySecret string
}

var errWhatsAppStub = disabled("whatsapp", "no Meta Cloud API credentials are wired — WhatsApp onboarding is post-investors (see docs/integrations.md)")

// WhatsAppStub is the fail-closed provider: every method returns
// ErrIntegrationDisabled. It never touches the network.
type WhatsAppStub struct{}

// SendText always fails closed — it never sends a message.
func (WhatsAppStub) SendText(_ context.Context, _ WhatsAppText) (string, error) {
	return "", errWhatsAppStub
}

// SendTemplate always fails closed — it never sends a template.
func (WhatsAppStub) SendTemplate(_ context.Context, _ WhatsAppTemplate) (string, error) {
	return "", errWhatsAppStub
}

// NewWhatsApp builds the WhatsApp provider for name. Only "stub" (and
// the empty default) exists until the post-investors Meta onboarding;
// anything else — including "cloud-api" — is a fail-closed error so a
// misconfigured WHATSAPP_PROVIDER refuses to boot instead of silently
// delivering nothing to buyers.
func NewWhatsApp(name string, _ WhatsAppConfig) (WhatsAppProvider, error) {
	switch name {
	case "", "stub":
		return WhatsAppStub{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (WhatsApp Cloud API onboarding is post-investors; until then only \"stub\" boots — see docs/integrations.md)",
			errUnknownProvider, name)
	}
}

// ---------------------------------------------------------------------------
// Tally on-premise bridge
// ---------------------------------------------------------------------------

// TallyVoucherKind names the voucher type posted to Tally.
type TallyVoucherKind string

const (
	// TallyVoucherSales posts a sales invoice into Tally.
	TallyVoucherSales TallyVoucherKind = "sales"
	// TallyVoucherJournal posts a journal entry (fees, adjustments).
	TallyVoucherJournal TallyVoucherKind = "journal"
)

// TallyLine is one ledger line of a voucher; amounts are integer paise
// (same convention as internal/catalog).
type TallyLine struct {
	LedgerName  string
	AmountPaise int64 // signed: positive debit, negative credit
}

// TallyVoucher is a voucher posted through the bridge agent. Date is
// YYYYMMDD per Tally's XML API.
type TallyVoucher struct {
	Kind        TallyVoucherKind
	Date        string // YYYYMMDD
	ReferenceNo string // our order id — the dedupe key at the bridge
	Narration   string
	Lines       []TallyLine
}

// TallyBridge is the on-premise seam: the seller's local bridge agent
// accepts signed jobs and posts vouchers to Tally Prime's XML API. A nil
// error means the bridge agent accepted the job (bridge job id
// returned); Tally-side posting is the agent's asynchronous job.
type TallyBridge interface {
	PostVoucher(ctx context.Context, v TallyVoucher) (jobID string, err error)
}

// TallyConfig carries the future bridge-agent connection details.
type TallyConfig struct {
	// BridgeURL is the local bridge agent's base URL
	// (e.g. https://localhost:9443) at the merchant's machine.
	BridgeURL string
	// SharedSecret signs jobs to the bridge agent (HMAC); never logged.
	SharedSecret string
}

var errTallyStub = disabled("tally", "no on-premise bridge agent is registered — Tally bridge deployment is post-investors (see docs/integrations.md)")

// TallyStub is the fail-closed bridge: every method returns
// ErrIntegrationDisabled. It never contacts any Tally installation.
type TallyStub struct{}

// PostVoucher always fails closed — it never posts a voucher.
func (TallyStub) PostVoucher(_ context.Context, _ TallyVoucher) (string, error) {
	return "", errTallyStub
}

// NewTally builds the Tally bridge for name. Only "stub" (and the
// empty default) exists until a merchant's bridge agent is deployed
// post-investors; anything else — including "bridge" — is a
// fail-closed error so a misconfigured TALLY_PROVIDER refuses to boot.
func NewTally(name string, _ TallyConfig) (TallyBridge, error) {
	switch name {
	case "", "stub":
		return TallyStub{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (Tally bridge deployment is post-investors; until then only \"stub\" boots — see docs/integrations.md)",
			errUnknownProvider, name)
	}
}

// ---------------------------------------------------------------------------
// Zoho Books (default accounting ledger)
// ---------------------------------------------------------------------------

// ZohoContact is the customer record in Zoho Books.
type ZohoContact struct {
	DisplayName string
	Email       string
	Phone       string // E.164
}

// ZohoInvoiceLine is one line of a customer invoice; amounts integer
// paise, INR-only until the international launch.
type ZohoInvoiceLine struct {
	Name        string
	Quantity    int
	AmountPaise int64
}

// ZohoInvoice is a customer invoice to create in Zoho Books.
type ZohoInvoice struct {
	ContactID   string
	ReferenceNo string // our order id — the dedupe key
	Currency    string // pinned "INR"
	Lines       []ZohoInvoiceLine
	DueDate     time.Time
}

// ZohoBooksProvider is the accounting seam: customer contacts and
// invoices for paid orders. A nil error means Zoho accepted the
// operation (Zoho id returned). Implementations must be safe to call
// concurrently and must never log the OAuth tokens.
type ZohoBooksProvider interface {
	CreateContact(ctx context.Context, c ZohoContact) (contactID string, err error)
	CreateInvoice(ctx context.Context, inv ZohoInvoice) (invoiceID string, err error)
}

// ZohoConfig carries the future Zoho OAuth credentials. Until the
// post-investors onboarding they are loaded, never logged, and ignored
// by the stub — a real provider name refuses to boot regardless of
// whether keys are present.
type ZohoConfig struct {
	ClientID     string
	ClientSecret string
	RefreshToken string
	// OrgID is the Zoho Books organization id (INR org).
	OrgID string
}

var errZohoStub = disabled("zoho-books", "no Zoho OAuth credentials are wired — Zoho Books onboarding is post-investors (see docs/integrations.md)")

// ZohoStub is the fail-closed provider: every method returns
// ErrIntegrationDisabled. It never contacts Zoho.
type ZohoStub struct{}

// CreateContact always fails closed — it never creates a contact.
func (ZohoStub) CreateContact(_ context.Context, _ ZohoContact) (string, error) {
	return "", errZohoStub
}

// CreateInvoice always fails closed — it never creates an invoice.
func (ZohoStub) CreateInvoice(_ context.Context, _ ZohoInvoice) (string, error) {
	return "", errZohoStub
}

// NewZoho builds the Zoho Books provider for name. Only "stub" (and
// the empty default) exists until the post-investors Zoho onboarding;
// anything else — including "books" — is a fail-closed error so a
// misconfigured ZOHO_PROVIDER refuses to boot instead of silently
// booking nothing.
func NewZoho(name string, _ ZohoConfig) (ZohoBooksProvider, error) {
	switch name {
	case "", "stub":
		return ZohoStub{}, nil
	default:
		return nil, fmt.Errorf("%w: %q (Zoho Books onboarding is post-investors; until then only \"stub\" boots — see docs/integrations.md)",
			errUnknownProvider, name)
	}
}

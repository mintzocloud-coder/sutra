// SPDX-License-Identifier: AGPL-3.0-or-later
package subscriptions

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestNewProviderStub(t *testing.T) {
	for _, name := range []string{"", "stub"} {
		p, err := NewProvider(name)
		if err != nil {
			t.Fatalf("NewProvider(%q): unexpected error: %v", name, err)
		}
		if _, ok := p.(StubProvider); !ok {
			t.Fatalf("NewProvider(%q): expected StubProvider, got %T", name, p)
		}
	}
}

func TestNewProviderRejectsCashfreeUntilOnboarding(t *testing.T) {
	// "cashfree" before the post-investors onboarding must refuse to
	// boot — a half-wired provider name is exactly how fake money
	// movement happens.
	for _, name := range []string{"cashfree", "Cashfree", "razorpay", "stripe", "bogus"} {
		if _, err := NewProvider(name); err == nil {
			t.Fatalf("NewProvider(%q): expected fail-closed error, got nil", name)
		} else if !strings.Contains(err.Error(), "post-investors") {
			t.Fatalf("NewProvider(%q): error should name the post-investors gate, got %q", name, err)
		}
	}
}

func TestStubEveryMethodFailsClosed(t *testing.T) {
	p, _ := NewProvider("stub")
	ctx := context.Background()

	if _, err := p.CreateSubscription(ctx, CreateParams{MemberID: "m", PlanID: "p", AmountPaise: 9900}); err == nil {
		t.Fatal("CreateSubscription: expected fail-closed error")
	}
	if err := p.CancelSubscription(ctx, "sub_123", true); err == nil {
		t.Fatal("CancelSubscription: expected fail-closed error")
	}
	if _, err := p.VerifyWebhook(ctx, []byte(`{}`), "sig"); err == nil {
		t.Fatal("VerifyWebhook: expected fail-closed error")
	}
}

func TestStubErrorIsTyped(t *testing.T) {
	p, _ := NewProvider("stub")
	_, err := p.CreateSubscription(context.Background(), CreateParams{})
	var disabled *ErrSubscriptionsDisabled
	if !errors.As(err, &disabled) {
		t.Fatalf("expected *ErrSubscriptionsDisabled, got %T (%v)", err, err)
	}
	if !strings.Contains(disabled.Error(), "UPI AutoPay") {
		t.Fatalf("typed error should name the gated rail, got %q", disabled.Error())
	}
}

func TestValidatePlanInput(t *testing.T) {
	ok := func(name, desc string, price int64, period BillingPeriod, cur string) bool {
		return ValidatePlanInput(name, desc, price, period, cur) == nil
	}
	if !ok("Insiders", "Backstage access", 9900, BillingMonthly, "INR") {
		t.Fatal("valid monthly plan rejected")
	}
	if !ok("Insiders", "", 118800, BillingYearly, "INR") {
		t.Fatal("valid yearly plan rejected")
	}
	if ok("", "x", 9900, BillingMonthly, "INR") {
		t.Fatal("empty name accepted")
	}
	if ok(strings.Repeat("n", 81), "x", 9900, BillingMonthly, "INR") {
		t.Fatal("81-char name accepted")
	}
	if ok("x", strings.Repeat("d", 501), 9900, BillingMonthly, "INR") {
		t.Fatal("501-char description accepted")
	}
	if ok("x", "x", 0, BillingMonthly, "INR") {
		t.Fatal("zero price accepted")
	}
	if ok("x", "x", -100, BillingMonthly, "INR") {
		t.Fatal("negative price accepted")
	}
	if ok("x", "x", 9900, "weekly", "INR") {
		t.Fatal("weekly period accepted")
	}
	if ok("x", "x", 9900, BillingMonthly, "USD") {
		t.Fatal("non-INR currency accepted")
	}
	// Rune (not byte) counting: 81 emoji must fail, 80 must pass.
	if ok(strings.Repeat("🎬", 81), "x", 9900, BillingMonthly, "INR") {
		t.Fatal("81-rune emoji name accepted")
	}
	if !ok(strings.Repeat("🎬", 80), "x", 9900, BillingMonthly, "INR") {
		t.Fatal("80-rune emoji name rejected")
	}
}

func TestSplitShares(t *testing.T) {
	cases := []struct {
		amount                int64
		wantCreator, wantPlat int64
	}{
		{10000, 7000, 3000},    // clean split
		{9900, 6930, 2970},     // ₹99 plan
		{199, 139, 60},         // floor(199*0.7)=139, remainder 60
		{1, 0, 1},              // 1 paise: creator gets 0, platform 1 — sums exactly
		{3, 2, 1},              // floor(2.1)=2
		{118800, 83160, 35640}, // yearly plan
	}
	for _, c := range cases {
		creator, plat, err := SplitShares(c.amount)
		if err != nil {
			t.Fatalf("SplitShares(%d): %v", c.amount, err)
		}
		if creator != c.wantCreator || plat != c.wantPlat {
			t.Fatalf("SplitShares(%d) = (%d,%d), want (%d,%d)", c.amount, creator, plat, c.wantCreator, c.wantPlat)
		}
		if creator+plat != c.amount {
			t.Fatalf("SplitShares(%d): shares %d+%d != amount", c.amount, creator, plat)
		}
	}
	for _, bad := range []int64{0, -1, -9999} {
		if _, _, err := SplitShares(bad); err == nil {
			t.Fatalf("SplitShares(%d): expected error", bad)
		}
	}
}

func TestCanTransition(t *testing.T) {
	must := map[[2]SubscriptionStatus]bool{
		{StatusPending, StatusActive}:    true,
		{StatusPending, StatusCancelled}: true,
		{StatusPending, StatusPending}:   false, // no self-loops: flips must mean something
		{StatusActive, StatusPastDue}:    true,
		{StatusActive, StatusCancelled}:  true,
		{StatusActive, StatusPending}:    false, // never regress to pending
		{StatusPastDue, StatusActive}:    true,  // recovery
		{StatusPastDue, StatusCancelled}: true,
		{StatusPastDue, StatusPending}:   false,
		{StatusCancelled, StatusActive}:  false, // terminal: resurrection needs a new row
		{StatusCancelled, StatusPastDue}: false,
		{"", StatusActive}:               false,
		{StatusActive, "bogus"}:          false,
	}
	for pair, want := range must {
		if got := CanTransition(pair[0], pair[1]); got != want {
			t.Fatalf("CanTransition(%q→%q) = %v, want %v", pair[0], pair[1], got, want)
		}
	}
}

func TestEntitlementsFor(t *testing.T) {
	grants := EntitlementsFor([]string{"creator-1", "creator-2", "creator-1", "", "creator-2"})
	if len(grants) != 2 {
		t.Fatalf("expected 2 deduped grants, got %d (%v)", len(grants), grants)
	}
	for _, g := range grants {
		if !strings.HasPrefix(g, "member_of:") {
			t.Fatalf("grant %q missing member_of: prefix", g)
		}
	}
	if len(EntitlementsFor(nil)) != 0 {
		t.Fatal("nil input should yield no grants")
	}
	if len(EntitlementsFor([]string{"", ""})) != 0 {
		t.Fatal("empty ids should yield no grants")
	}
}

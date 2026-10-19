// SPDX-License-Identifier: AGPL-3.0-or-later
package ads

import (
	"reflect"
	"testing"
)

func TestRunClassicTwoBidder(t *testing.T) {
	res, err := Run(Config{Slots: 1}, []Bid{
		{BidderID: "a", BidPaise: 100, Quality: 1.0},
		{BidderID: "b", BidPaise: 80, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].BidderID != "a" {
		t.Fatalf("winner = %+v, want bidder a", res)
	}
	// GSP: ceil(80/1.0) + 1 = 81 paise. Winner never pays their own bid.
	if res[0].PricePaise != 81 {
		t.Fatalf("price = %d, want 81", res[0].PricePaise)
	}
	if res[0].Slot != 1 || res[0].RankScore != 100 || res[0].RunnerUpScore != 80 {
		t.Fatalf("result fields wrong: %+v", res[0])
	}
}

func TestRunQualityBeatsRawBid(t *testing.T) {
	// a bids more but is low quality; b wins on rank score.
	res, err := Run(Config{Slots: 1}, []Bid{
		{BidderID: "a", BidPaise: 100, Quality: 0.5}, // score 50
		{BidderID: "b", BidPaise: 60, Quality: 1.0},  // score 60
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].BidderID != "b" {
		t.Fatalf("winner = %+v, want bidder b", res)
	}
	// Price reflects b's own quality: ceil(50/1.0)+1 = 51 — NOT 81.
	if res[0].PricePaise != 51 {
		t.Fatalf("price = %d, want 51", res[0].PricePaise)
	}
}

func TestRunSingleBidderPaysReserve(t *testing.T) {
	res, err := Run(Config{Slots: 1, ReservePaise: 50}, []Bid{
		{BidderID: "a", BidPaise: 60, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("results = %+v, want one winner", res)
	}
	// No competition: ceil(0/1)+1 = 1, clamped up to the reserve.
	if res[0].PricePaise != 50 {
		t.Fatalf("price = %d, want reserve 50", res[0].PricePaise)
	}
}

func TestRunBidBelowReserveExcluded(t *testing.T) {
	res, err := Run(Config{Slots: 1, ReservePaise: 50}, []Bid{
		{BidderID: "cheap", BidPaise: 49, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("results = %+v, want no winner", res)
	}
}

func TestRunJunkBidsCannotSetPrices(t *testing.T) {
	// The junk bid is excluded BEFORE ranking, so it must not inflate
	// the real winner's price: winner pays the reserve, not the junk + 1.
	res, err := Run(Config{Slots: 1}, []Bid{
		{BidderID: "real", BidPaise: 100, Quality: 1.0},
		{BidderID: "junk", BidPaise: 40, Quality: 1.0}, // below default reserve 50
		{BidderID: "neg", BidPaise: -10, Quality: 1.0},
		{BidderID: "noqual", BidPaise: 200, Quality: 0},
		{BidderID: "overqual", BidPaise: 200, Quality: 1.5},
		{BidderID: "", BidPaise: 200, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].BidderID != "real" {
		t.Fatalf("winner = %+v, want bidder real", res)
	}
	if res[0].PricePaise != ReserveDefaultPaise {
		t.Fatalf("price = %d, want default reserve %d", res[0].PricePaise, ReserveDefaultPaise)
	}
}

func TestRunTieBreakDeterministic(t *testing.T) {
	bids := []Bid{
		{BidderID: "zzz", BidPaise: 100, Quality: 0.5}, // score 50
		{BidderID: "aaa", BidPaise: 50, Quality: 1.0},  // score 50
	}
	res, err := Run(Config{Slots: 2}, bids)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %+v, want 2 winners", res)
	}
	// Same score: higher quality first → aaa wins slot 1.
	if res[0].BidderID != "aaa" || res[1].BidderID != "zzz" {
		t.Fatalf("order = %v %v, want aaa then zzz", res[0].BidderID, res[1].BidderID)
	}
	// Same input twice → identical output (replayable for billing disputes).
	res2, err := Run(Config{Slots: 2}, bids)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res, res2) {
		t.Fatalf("non-deterministic: %+v vs %+v", res, res2)
	}
}

func TestRunTieBreakByBidderID(t *testing.T) {
	// Identical score AND quality: lexicographically smaller ID wins.
	res, err := Run(Config{Slots: 2}, []Bid{
		{BidderID: "beta", BidPaise: 100, Quality: 0.5},  // score 50
		{BidderID: "alpha", BidPaise: 100, Quality: 0.5}, // score 50
	})
	if err != nil {
		t.Fatal(err)
	}
	if res[0].BidderID != "alpha" || res[1].BidderID != "beta" {
		t.Fatalf("order = %v %v, want alpha then beta", res[0].BidderID, res[1].BidderID)
	}
}

func TestRunMultiSlotCascade(t *testing.T) {
	res, err := Run(Config{Slots: 2}, []Bid{
		{BidderID: "a", BidPaise: 100, Quality: 1.0}, // score 100
		{BidderID: "b", BidPaise: 80, Quality: 1.0},  // score 80
		{BidderID: "c", BidPaise: 60, Quality: 1.0},  // score 60
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 2 {
		t.Fatalf("results = %+v, want 2 winners", res)
	}
	// a displaces b: ceil(80/1)+1 = 81. b displaces c: ceil(60/1)+1 = 61.
	if res[0].BidderID != "a" || res[0].Slot != 1 || res[0].PricePaise != 81 {
		t.Fatalf("slot1 = %+v, want a/81", res[0])
	}
	if res[1].BidderID != "b" || res[1].Slot != 2 || res[1].PricePaise != 61 {
		t.Fatalf("slot2 = %+v, want b/61", res[1])
	}
}

func TestRunPriceNeverExceedsOwnBid(t *testing.T) {
	// a: score 54, b: score 53.1. Raw GSP math gives 59+1 = 60 = a's bid.
	res, err := Run(Config{Slots: 1}, []Bid{
		{BidderID: "a", BidPaise: 60, Quality: 0.9},
		{BidderID: "b", BidPaise: 59, Quality: 0.9},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].BidderID != "a" {
		t.Fatalf("winner = %+v, want bidder a", res)
	}
	if res[0].PricePaise != 60 || res[0].PricePaise > 60 {
		t.Fatalf("price = %d, want <= own bid 60", res[0].PricePaise)
	}
}

func TestRunMoreSlotsThanBidders(t *testing.T) {
	res, err := Run(Config{Slots: 5}, []Bid{
		{BidderID: "a", BidPaise: 100, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || res[0].Slot != 1 {
		t.Fatalf("results = %+v, want single slot-1 winner", res)
	}
}

func TestRunZeroSlotsRanksNothing(t *testing.T) {
	res, err := Run(Config{Slots: 0}, []Bid{
		{BidderID: "a", BidPaise: 100, Quality: 1.0},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("results = %+v, want empty", res)
	}
}

func TestRunNegativeSlotsError(t *testing.T) {
	if _, err := Run(Config{Slots: -1}, nil); err == nil {
		t.Fatal("want error for negative slots, got nil")
	}
}

func TestRunEmptyBids(t *testing.T) {
	res, err := Run(Config{Slots: 3}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Fatalf("results = %+v, want empty", res)
	}
}

func TestWinnerWrapper(t *testing.T) {
	r, ok, err := Winner([]Bid{
		{BidderID: "a", BidPaise: 100, Quality: 1.0},
		{BidderID: "b", BidPaise: 70, Quality: 0.8}, // score 56
	}, 0) // 0 → default reserve
	if err != nil || !ok {
		t.Fatalf("err=%v ok=%v", err, ok)
	}
	// ceil(56/1.0)+1 = 57.
	if r.BidderID != "a" || r.PricePaise != 57 {
		t.Fatalf("result = %+v, want a/57", r)
	}
}

func TestWinnerWrapperNoWinner(t *testing.T) {
	_, ok, err := Winner([]Bid{{BidderID: "a", BidPaise: 10, Quality: 1.0}}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("want no winner when bid is below reserve")
	}
}

// SPDX-License-Identifier: AGPL-3.0-or-later
// Package ads implements Sutra's first-party ads platform.
//
// Design targets (founder directive): an auction core that is simpler to
// reason about, fully deterministic, and more auditable than the black-box
// equivalents in market ad platforms. Part 1 ships the auction kernel;
// the advertiser API (campaigns, budgets, targeting), creative serving,
// reporting and the ad-serving HTTP surface are later parts of H3.
//
// Money is integer paise (India-first platform). Quality is a float64 in
// (0,1]: the platform's relevance × landing-experience score, supplied by
// the ranking service (Phase H later part). The auction itself must not
// compute quality — it only consumes it, so rank fairness stays inspectable.
package ads

import (
	"errors"
	"math"
	"sort"
)

// Money units: paise. One paise is the minimum price increment.
const paisePerRupee = 100

// ReserveDefaultPaise is the platform-wide default reserve (floor) price:
// ₹0.50 CPM-equivalent per impression slot. A reserve stops junk bidders
// from winning slots for free when competition is thin — the failure mode
// that makes naive second-price auctions worthless on low-traffic surfaces.
const ReserveDefaultPaise = 50

// Bid is one advertiser's sealed bid for a single ad slot auction.
type Bid struct {
	// BidderID identifies the advertiser (e.g. "adv_9f3c"). Must be non-empty.
	BidderID string
	// BidPaise is the maximum the advertiser will pay per won impression.
	// Must be > 0.
	BidPaise int64
	// Quality is the platform-assigned relevance score in (0,1].
	// Must be > 0 and <= 1. Zero-quality ads never win (spam filter).
	Quality float64
}

// Config tunes one auction run.
type Config struct {
	// Slots is the number of ad slots being auctioned (usually 1–3).
	// Slots <= 0 means "rank everything, fill nothing" — valid for dry runs.
	Slots int
	// ReservePaise is the per-impression floor price. Bids below it are
	// excluded before ranking. <= 0 falls back to ReserveDefaultPaise.
	ReservePaise int64
}

// Result is one winner's outcome.
type Result struct {
	BidderID string
	// Slot is the 1-based slot rank (1 = top). Slots fill in rank order.
	Slot int
	// PricePaise is what the winner pays per impression: the GSP price.
	PricePaise int64
	// RankScore is the winner's own rank score (bid × quality), exposed
	// so billing reports can show advertisers *why* they won or lost.
	RankScore float64
	// RunnerUpScore is the rank score of the bidder displaced below this
	// winner (0 for the last winner). Drives the GSP price; exposing it
	// makes the auction auditable per-slot.
	RunnerUpScore float64
}

// Run executes a Generalized Second Price auction over the bids.
//
// Ranking: rankScore = bidPaise × quality, descending.
// Pricing: winner of slot i pays ceil(rankScore(i+1) / quality(i)) + 1
// paise, floored at the reserve. The winner never pays more than their
// own bid (GSP truthfulness property — provable because the displaced
// bidder's rank score is always <= the winner's own).
//
// Exclusions: non-positive bids, out-of-range quality, and bids below the
// reserve are dropped before ranking (they cannot win, and they must not
// set prices for real winners — a junk bid must never inflate someone
// else's bill).
//
// Tie-breaks are fully deterministic: higher quality wins first, then
// lexicographically smaller BidderID. No map iteration, no time source,
// no randomness — the same input always produces the same result,
// which is what makes every auction replayable in billing disputes.
func Run(cfg Config, bids []Bid) ([]Result, error) {
	if cfg.Slots < 0 {
		return nil, errors.New("ads: slots cannot be negative")
	}
	reserve := cfg.ReservePaise
	if reserve <= 0 {
		reserve = ReserveDefaultPaise
	}

	// eligible filters and snapshots the rank score once, so later
	// price math cannot drift from the ranking that produced it.
	type candidate struct {
		bid   Bid
		score float64
	}
	var eligible []candidate
	for _, b := range bids {
		if b.BidderID == "" || b.BidPaise <= 0 {
			continue
		}
		if b.Quality <= 0 || b.Quality > 1 {
			continue
		}
		if b.BidPaise < reserve {
			continue
		}
		eligible = append(eligible, candidate{bid: b, score: float64(b.BidPaise) * b.Quality})
	}

	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].score != eligible[j].score {
			return eligible[i].score > eligible[j].score
		}
		if eligible[i].bid.Quality != eligible[j].bid.Quality {
			return eligible[i].bid.Quality > eligible[j].bid.Quality
		}
		return eligible[i].bid.BidderID < eligible[j].bid.BidderID
	})

	winners := cfg.Slots
	if winners > len(eligible) {
		winners = len(eligible)
	}
	out := make([]Result, 0, winners)
	for i := 0; i < winners; i++ {
		w := eligible[i]
		var runnerUp float64
		if i+1 < len(eligible) {
			runnerUp = eligible[i+1].score
		}
		// GSP price: ceil(displaced score / own quality) + 1 paise.
		// Clamp: never above own bid (float rounding paranoia), never
		// below reserve (thin-competition floor).
		price := int64(math.Ceil(runnerUp/w.bid.Quality)) + 1
		if price < reserve {
			price = reserve
		}
		if price > w.bid.BidPaise {
			price = w.bid.BidPaise
		}
		out = append(out, Result{
			BidderID:      w.bid.BidderID,
			Slot:          i + 1,
			PricePaise:    price,
			RankScore:     w.score,
			RunnerUpScore: runnerUp,
		})
	}
	return out, nil
}

// Winner is a convenience wrapper for the overwhelmingly common
// single-slot case (one ad slot per feed insertion point).
func Winner(bids []Bid, reservePaise int64) (Result, bool, error) {
	res, err := Run(Config{Slots: 1, ReservePaise: reservePaise}, bids)
	if err != nil {
		return Result{}, false, err
	}
	if len(res) == 0 {
		return Result{}, false, nil
	}
	return res[0], true, nil
}

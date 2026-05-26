package core

import (
	"testing"

	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/stats"
)

func statWith(pairs map[stats.Stat]float64) stats.Stats {
	var s stats.Stats
	for k, v := range pairs {
		s[k] = v
	}
	return s
}

// Hunter-style case: a hit gem and an agility gem. Hit is worth more per point than agi, but only
// up to the hit cap; past the cap the optimizer must switch to agi gems.
func TestOptimizeGemsForEP_CapSwitch(t *testing.T) {
	ep := statWith(map[stats.Stat]float64{stats.Agility: 1.0, stats.MeleeHitRating: 2.0})
	// Hard cap on hit at 20: pre-cap EP 2.0, then 0 past the cap.
	curves := map[stats.Stat]statCurve{stats.MeleeHitRating: {thresholds: []float64{20}, segEPs: []float64{2.0, 0}}}
	base := statWith(map[stats.Stat]float64{stats.MeleeHitRating: 6}) // 14 hit room
	pool := []gemOption{
		{id: 1, stats: statWith(map[stats.Stat]float64{stats.MeleeHitRating: 8})}, // hit gem
		{id: 2, stats: statWith(map[stats.Stat]float64{stats.Agility: 8})},  // agi gem
	}

	// Socket1: hit 8*2=16 > agi 8 -> hit (cur hit 14).
	// Socket2: hit room 6 -> 6*2=12 > agi 8 -> hit (cur hit 22, over cap).
	// Socket3: hit room <0 -> 0; agi 8 -> agi.
	// Socket4: agi.
	got := optimizeGemsForEP(4, base, ep, curves, pool)
	want := []int32{1, 1, 2, 2}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// A socket bonus should be chased only when it outweighs the EP given up by color-matching.
func TestShouldMatchSocketBonus(t *testing.T) {
	ep := statWith(map[stats.Stat]float64{stats.Agility: 1.0})
	pool := []gemOption{
		{id: 1, stats: statWith(map[stats.Stat]float64{stats.Agility: 8}), color: proto.GemColor_GemColorRed},
		{id: 2, stats: statWith(map[stats.Stat]float64{stats.MeleeHitRating: 8}), color: proto.GemColor_GemColorYellow}, // 0 agi EP
	}
	// Yellow socket: matching costs 8 EP (yellow gem has no agi). Big bonus beats it; tiny one doesn't.
	item := &Item{GemSockets: []proto.GemColor{proto.GemColor_GemColorYellow}, SocketBonus: statWith(map[stats.Stat]float64{stats.Agility: 100})}
	if !shouldMatchSocketBonus(item, []int{0}, ep, pool) {
		t.Fatal("expected to chase a large socket bonus")
	}
	item.SocketBonus = statWith(map[stats.Stat]float64{stats.Agility: 1})
	if shouldMatchSocketBonus(item, []int{0}, ep, pool) {
		t.Fatal("expected NOT to chase a tiny socket bonus")
	}
}

// With no caps, the optimizer should always pick the highest-EP gem.
func TestOptimizeGemsForEP_NoCaps(t *testing.T) {
	ep := statWith(map[stats.Stat]float64{stats.Agility: 1.0, stats.MeleeHitRating: 2.0})
	var base stats.Stats
	pool := []gemOption{
		{id: 1, stats: statWith(map[stats.Stat]float64{stats.MeleeHitRating: 8})}, // 16 EP
		{id: 2, stats: statWith(map[stats.Stat]float64{stats.Agility: 8})},  // 8 EP
	}
	got := optimizeGemsForEP(3, base, ep, nil, pool) // no caps
	want := []int32{1, 1, 1}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

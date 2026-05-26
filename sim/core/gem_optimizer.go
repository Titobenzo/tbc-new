package core

import (
	"math"

	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/stats"
)

// statCurve is a piecewise-linear EP curve for a (capped) stat. While the stat's cumulative value is
// below thresholds[i] it earns segEPs[i] EP per rating point; past the last threshold it earns the
// last segEP. thresholds is ascending; len(segEPs) == len(thresholds)+1. A hard cap is just a curve
// whose last segEP is 0; a soft cap has positive-but-smaller post-cap segEPs.
type statCurve struct {
	thresholds []float64
	segEPs     []float64
}

// marginal is the EP gained by adding `add` rating starting from cumulative value `start`.
func (c statCurve) marginal(start, add float64) float64 {
	total, pos, remaining := 0.0, start, add
	for i := 0; i < len(c.segEPs) && remaining > 0; i++ {
		segEnd := math.Inf(1)
		if i < len(c.thresholds) {
			segEnd = c.thresholds[i]
		}
		if pos >= segEnd {
			continue
		}
		take := math.Min(remaining, segEnd-pos)
		total += take * c.segEPs[i]
		pos += take
		remaining -= take
	}
	return total
}

// pseudostatRating maps a capped PseudoStat to the gem rating stat that feeds it and its rating-per-1%
// factor. Covers hit, crit, haste, and block (everything gems can affect). Caps configured directly
// on a rating stat need no entry here (their breakpoints are already in rating units).
func pseudostatRating(ps proto.PseudoStat) (stats.Stat, float64, bool) {
	switch ps {
	case proto.PseudoStat_PseudoStatMeleeHitPercent, proto.PseudoStat_PseudoStatRangedHitPercent:
		return stats.MeleeHitRating, PhysicalHitRatingPerHitPercent, true
	case proto.PseudoStat_PseudoStatSpellHitPercent:
		return stats.SpellHitRating, SpellHitRatingPerHitPercent, true
	case proto.PseudoStat_PseudoStatMeleeCritPercent, proto.PseudoStat_PseudoStatRangedCritPercent:
		return stats.MeleeCritRating, PhysicalCritRatingPerCritPercent, true
	case proto.PseudoStat_PseudoStatSpellCritPercent:
		return stats.SpellCritRating, SpellCritRatingPerCritPercent, true
	case proto.PseudoStat_PseudoStatMeleeHastePercent, proto.PseudoStat_PseudoStatRangedHastePercent:
		return stats.MeleeHasteRating, PhysicalHasteRatingPerHastePercent, true
	case proto.PseudoStat_PseudoStatSpellHastePercent:
		return stats.SpellHasteRating, SpellHasteRatingPerHastePercent, true
	case proto.PseudoStat_PseudoStatBlockPercent:
		return stats.BlockRating, BlockRatingPerBlockPercent, true
	}
	return 0, 0, false
}

// buildGemCurves turns the configured caps/soft-cap breakpoints (exactly as set in Suggest Gems) into
// a per-rating-stat piecewise EP curve for THIS combo. Percent breakpoints become rating thresholds
// using the combo's non-gem stats and the rating-per-percent factors; rating-stat caps use their
// breakpoints directly. The pre-cap EP comes from the player's EP weights.
func buildGemCurves(gemCaps []*proto.GemStatCap, nonGemStats stats.Stats, pseudoStats []float64, epWeights stats.Stats) map[stats.Stat]statCurve {
	curves := make(map[stats.Stat]statCurve, len(gemCaps))
	for _, cap := range gemCaps {
		if cap == nil || len(cap.Breakpoints) == 0 {
			continue
		}
		var ratingStat stats.Stat
		factor := 1.0 // rating-per-unit-of-breakpoint (1 for rating-space caps)
		thresholds := make([]float64, 0, len(cap.Breakpoints))
		if cap.IsPseudostat {
			rs, f, ok := pseudostatRating(proto.PseudoStat(cap.UnitStat))
			if !ok {
				continue
			}
			ratingStat, factor = rs, f
			currentPercent := 0.0
			if int(cap.UnitStat) < len(pseudoStats) {
				currentPercent = pseudoStats[cap.UnitStat]
			}
			for _, bp := range cap.Breakpoints {
				thresholds = append(thresholds, nonGemStats[rs]+(bp-currentPercent)*factor)
			}
		} else {
			ratingStat = stats.Stat(cap.UnitStat)
			thresholds = append(thresholds, cap.Breakpoints...)
		}
		// segEPs are per-rating-point; post-cap EPs are configured in the breakpoint's unit (per
		// percent for pseudostat caps), so divide by the rating-per-unit factor to match.
		segEPs := make([]float64, 0, len(thresholds)+1)
		segEPs = append(segEPs, epWeights[ratingStat]) // pre-cap EP (already per-rating)
		for _, pe := range cap.PostCapEps {
			segEPs = append(segEPs, pe/factor)
		}
		for len(segEPs) < len(thresholds)+1 {
			segEPs = append(segEPs, 0) // missing post-cap EPs default to "capped" (0)
		}
		curves[ratingStat] = statCurve{thresholds: thresholds, segEPs: segEPs[:len(thresholds)+1]}
	}
	return curves
}

// firstPlayerStats returns the first player's stats from a ComputeStats result, or nil.
func firstPlayerStats(res *proto.ComputeStatsResult) *proto.PlayerStats {
	if res == nil || res.RaidStats == nil || len(res.RaidStats.Parties) == 0 {
		return nil
	}
	if party := res.RaidStats.Parties[0]; party != nil && len(party.Players) > 0 {
		return party.Players[0]
	}
	return nil
}

// buildGemPool returns the candidate (non-meta) gems the optimizer may use, with stat-dominated gems
// removed so the per-socket search is over only the Pareto front (a handful of gems). When poolIds is
// non-empty the pool is restricted to exactly those gems (the client pre-filters by phase, profession,
// quality, etc. - matching Suggest Gems); empty falls back to every loaded gem.
func buildGemPool(poolIds []int32, disableUnique bool) []gemOption {
	mutex.RLock()
	defer mutex.RUnlock()
	add := func(all []gemOption, id int32, gem Gem) []gemOption {
		// id<=0 is a degenerate "no gem" entry; placing it leaves the socket empty. A statless gem is
		// never worth socketing. Excluding both keeps the optimizer from ever choosing an empty socket.
		if id <= 0 || gem.Color == proto.GemColor_GemColorMeta {
			return all
		}
		if disableUnique && gem.Unique {
			return all
		}
		hasStat := false
		for _, v := range gem.Stats {
			if v != 0 {
				hasStat = true
				break
			}
		}
		if !hasStat {
			return all
		}
		return append(all, gemOption{id: id, stats: gem.Stats, color: gem.Color, unique: gem.Unique})
	}
	if len(poolIds) > 0 {
		all := make([]gemOption, 0, len(poolIds))
		for _, id := range poolIds {
			if gem, ok := GemsByID[id]; ok {
				all = add(all, id, gem)
			}
		}
		return filterDominatedGems(all)
	}
	all := make([]gemOption, 0, len(GemsByID))
	for id, gem := range GemsByID {
		all = add(all, id, gem)
	}
	return filterDominatedGems(all)
}

// statsGE reports whether a has at least as much of every stat as b.
func statsGE(a, b stats.Stats) bool {
	for k := 0; k < len(a); k++ {
		if a[k] < b[k] {
			return false
		}
	}
	return true
}

// filterDominatedGems drops any gem whose stats are matched-or-beaten in every stat by another gem
// OF THE SAME COLOR (color matters for socket bonuses, so a same-stat gem of a different color is not
// redundant). Keeps the per-color Pareto front; duplicates collapse to a single representative.
func filterDominatedGems(gems []gemOption) []gemOption {
	kept := make([]gemOption, 0, len(gems))
	for i := range gems {
		keep := true
		for j := range gems {
			if i == j || gems[i].color != gems[j].color {
				continue
			}
			if statsGE(gems[j].stats, gems[i].stats) {
				if !statsGE(gems[i].stats, gems[j].stats) {
					keep = false // j strictly dominates i
					break
				}
				if j < i {
					keep = false // equal stats: keep only the lowest index
					break
				}
			}
		}
		if keep {
			kept = append(kept, gems[i])
		}
	}
	return kept
}

// optimizeEquipmentGems re-gems the non-meta sockets across all items in equip to maximize EP under
// the caps, accounting for socket-color bonuses. nonGemStats is the character's stats with the gear
// ungemmed. Meta sockets are left as-is (their gem already matches the meta socket).
//
// Per item it decides whether chasing the socket bonus is worth the EP cost of using color-matching
// gems (a cap-ignorant comparison - the cap interaction is a known approximation), then optimizes
// all sockets jointly with the resulting per-socket candidate pools.
func optimizeEquipmentGems(equip *proto.EquipmentSpec, nonGemStats, epWeights stats.Stats, curves map[stats.Stat]statCurve, pool []gemOption) {
	type socketRef struct {
		item   *proto.ItemSpec
		gemIdx int
	}
	var sockets []socketRef
	var socketPools [][]gemOption
	for _, spec := range equip.Items {
		if spec == nil || spec.Id == 0 {
			continue
		}
		item := GetItemByID(spec.Id)
		if item == nil {
			continue
		}
		nonMetaIdx := make([]int, 0, len(item.GemSockets))
		for gi, color := range item.GemSockets {
			if color != proto.GemColor_GemColorMeta {
				nonMetaIdx = append(nonMetaIdx, gi)
			}
		}
		if len(nonMetaIdx) == 0 {
			continue
		}
		matchBonus := shouldMatchSocketBonus(item, nonMetaIdx, epWeights, pool)
		for _, gi := range nonMetaIdx {
			sockets = append(sockets, socketRef{spec, gi})
			if matchBonus {
				socketPools = append(socketPools, gemsMatchingColor(pool, item.GemSockets[gi]))
			} else {
				socketPools = append(socketPools, pool)
			}
		}
	}
	if len(sockets) == 0 {
		return
	}

	chosen := optimizeSockets(socketPools, nonGemStats, epWeights, curves)
	for n, ref := range sockets {
		if n >= len(chosen) {
			continue
		}
		gemId := chosen[n]
		if gemId == 0 {
			// Never ship an empty socket: fall back to the best raw-EP gem in this socket's pool.
			gemId = bestRawEPGemID(socketPools[n], epWeights)
		}
		if gemId == 0 {
			continue
		}
		for len(ref.item.Gems) <= ref.gemIdx {
			ref.item.Gems = append(ref.item.Gems, 0)
		}
		ref.item.Gems[ref.gemIdx] = gemId
	}
}

// bestRawEPGemID returns the id of the highest raw-EP (cap-ignorant) gem in the pool, or 0 if empty.
func bestRawEPGemID(pool []gemOption, epWeights stats.Stats) int32 {
	bestID := int32(0)
	bestEP := math.Inf(-1)
	for i := range pool {
		if pool[i].id <= 0 {
			continue
		}
		v := 0.0
		for k := 0; k < len(pool[i].stats); k++ {
			v += pool[i].stats[k] * epWeights[k]
		}
		if v > bestEP {
			bestEP, bestID = v, pool[i].id
		}
	}
	return bestID
}

// prepareMetaGems fills each meta socket with its existing gem if present, else the fallback
// metaGemId, and strips all non-meta gems (the optimizer fills those). This keeps the meta gem in
// the gear instead of losing it, and ensures the meta's stats are in the non-gem baseline used for
// caps. The optimizer skips meta sockets, so it leaves these untouched.
func prepareMetaGems(equip *proto.EquipmentSpec, metaGemId int32) {
	for _, spec := range equip.Items {
		if spec == nil || spec.Id == 0 {
			continue
		}
		item := GetItemByID(spec.Id)
		if item == nil {
			spec.Gems = nil
			continue
		}
		newGems := make([]int32, len(item.GemSockets))
		for gi, color := range item.GemSockets {
			if color == proto.GemColor_GemColorMeta {
				if gi < len(spec.Gems) && spec.Gems[gi] != 0 {
					newGems[gi] = spec.Gems[gi] // keep the existing meta gem
				} else {
					newGems[gi] = metaGemId // fall back to the configured meta gem
				}
			}
		}
		spec.Gems = newGems
	}
}

// equippedMetaGem returns the gem id sitting in the first meta socket of the equipment, or 0 if none.
// Used as the fallback meta gem so the player's current meta carries forward onto whatever helmet a
// combo lands on, even when no explicit fallback meta gem was configured in the bulk UI.
func equippedMetaGem(equip *proto.EquipmentSpec) int32 {
	if equip == nil {
		return 0
	}
	for _, spec := range equip.Items {
		if spec == nil || spec.Id == 0 {
			continue
		}
		item := GetItemByID(spec.Id)
		if item == nil {
			continue
		}
		for gi, color := range item.GemSockets {
			if color == proto.GemColor_GemColorMeta && gi < len(spec.Gems) && spec.Gems[gi] != 0 {
				return spec.Gems[gi]
			}
		}
	}
	return 0
}

// gemsMatchingColor returns the pool gems whose color satisfies socketColor (for the socket bonus),
// or the whole pool if none match.
func gemsMatchingColor(pool []gemOption, socketColor proto.GemColor) []gemOption {
	matching := make([]gemOption, 0, len(pool))
	for _, g := range pool {
		if ColorIntersects(socketColor, g.color) {
			matching = append(matching, g)
		}
	}
	if len(matching) == 0 {
		return pool
	}
	return matching
}

// bestRawEP is the EP of the highest-EP gem in the pool, ignoring caps.
func bestRawEP(pool []gemOption, epWeights stats.Stats) float64 {
	best := 0.0
	for i := range pool {
		v := 0.0
		for k := 0; k < len(pool[i].stats); k++ {
			v += pool[i].stats[k] * epWeights[k]
		}
		if v > best {
			best = v
		}
	}
	return best
}

// shouldMatchSocketBonus decides (cap-ignorant, by raw EP) whether the item's socket bonus is worth
// the EP given up by using color-matching gems instead of the globally best gems.
func shouldMatchSocketBonus(item *Item, nonMetaIdx []int, epWeights stats.Stats, pool []gemOption) bool {
	bonusEP := 0.0
	for k := 0; k < len(item.SocketBonus); k++ {
		bonusEP += item.SocketBonus[k] * epWeights[k]
	}
	if bonusEP <= 0 {
		return false
	}
	bestAny := bestRawEP(pool, epWeights)
	cost := 0.0
	for _, gi := range nonMetaIdx {
		cost += bestAny - bestRawEP(gemsMatchingColor(pool, item.GemSockets[gi]), epWeights)
	}
	return bonusEP > cost
}

// gemOption is a candidate gem the optimizer may place into a socket.
type gemOption struct {
	id     int32
	stats  stats.Stats
	color  proto.GemColor
	unique bool
}

// optimizeGemsForEP chooses a gem for each of numSockets sockets to maximize total EP, where any
// stat with a cap (caps[k] > 0) contributes no EP beyond that cap. nonGemStats is the gear set's
// stats excluding gems (base + items + enchants).
//
// The objective is separable and concave (each capped stat's value flattens at its cap), so placing
// the highest-marginal-EP gem one socket at a time is optimal for given per-socket pools.
func optimizeGemsForEP(numSockets int, nonGemStats, epWeights stats.Stats, curves map[stats.Stat]statCurve, pool []gemOption) []int32 {
	pools := make([][]gemOption, numSockets)
	for i := range pools {
		pools[i] = pool
	}
	return optimizeSockets(pools, nonGemStats, epWeights, curves)
}

// optimizeSockets chooses one gem per socket from that socket's candidate pool to maximize total EP
// under the caps. Returns the chosen gem id per socket (0 if a socket's pool was empty). At each step
// it fills the (socket, gem) with the highest marginal EP given the running stats - optimal for the
// concave capped objective.
func optimizeSockets(socketPools [][]gemOption, nonGemStats, epWeights stats.Stats, curves map[stats.Stat]statCurve) []int32 {
	n := len(socketPools)
	chosen := make([]int32, n)
	filled := make([]bool, n)
	usedUnique := map[int32]bool{} // unique-equipped gems already placed
	cur := nonGemStats
	for placed := 0; placed < n; placed++ {
		bestSocket := -1
		bestEP := 0.0
		var bestGem gemOption
		for s := 0; s < n; s++ {
			if filled[s] {
				continue
			}
			for gi := range socketPools[s] {
				g := socketPools[s][gi]
				if g.unique && usedUnique[g.id] {
					continue // already placed this unique-equipped gem
				}
				ep := marginalGemEP(cur, g.stats, epWeights, curves)
				if bestSocket == -1 || ep > bestEP {
					bestSocket = s
					bestGem = g
					bestEP = ep
				}
			}
		}
		if bestSocket == -1 {
			// Every remaining socket's pool is exhausted under the max-one-unique rule (the good gems
			// were unique and already placed). Fill them anyway - a duplicate gem beats an empty socket.
			for s := 0; s < n; s++ {
				if filled[s] || len(socketPools[s]) == 0 {
					continue
				}
				best := socketPools[s][0]
				bestSocketEP := marginalGemEP(cur, best.stats, epWeights, curves)
				for gi := 1; gi < len(socketPools[s]); gi++ {
					if ep := marginalGemEP(cur, socketPools[s][gi].stats, epWeights, curves); ep > bestSocketEP {
						best, bestSocketEP = socketPools[s][gi], ep
					}
				}
				chosen[s] = best.id
				filled[s] = true
				cur = cur.Add(best.stats)
			}
			break
		}
		chosen[bestSocket] = bestGem.id
		filled[bestSocket] = true
		cur = cur.Add(bestGem.stats)
		if bestGem.unique {
			usedUnique[bestGem.id] = true
		}
	}
	return chosen
}

// marginalGemEP is the EP gained by adding gemStats on top of cur. A stat with a configured curve
// uses its piecewise EP (so caps and soft-cap breakpoints are honored); others use their flat EP.
func marginalGemEP(cur, gemStats, epWeights stats.Stats, curves map[stats.Stat]statCurve) float64 {
	total := 0.0
	for k := 0; k < len(gemStats); k++ {
		add := gemStats[k]
		if add == 0 {
			continue
		}
		if curve, ok := curves[stats.Stat(k)]; ok {
			total += curve.marginal(cur[k], add)
		} else {
			total += add * epWeights[k]
		}
	}
	return total
}

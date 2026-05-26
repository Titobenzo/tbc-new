package core

import (
	"fmt"
	"time"

	"github.com/wowsims/tbc/sim/core/lp"
	"github.com/wowsims/tbc/sim/core/proto"
	"github.com/wowsims/tbc/sim/core/stats"
)

// metaCondition is the activation requirement of the equipped meta gem (resolved on the client via
// getMetaGemCondition). The LP constrains gem colors so the condition is satisfied and the meta gem
// stays active - matching what Suggest Gems does.
type metaCondition struct {
	minRed         int
	minYellow      int
	minBlue        int
	compareGreater proto.GemColor // 0/Unknown => no "more X than Y" rule
	compareLesser  proto.GemColor
}

func metaConditionFromSettings(s *proto.BulkSettings) metaCondition {
	if s == nil {
		return metaCondition{}
	}
	return metaCondition{
		minRed:         int(s.MetaMinRed),
		minYellow:      int(s.MetaMinYellow),
		minBlue:        int(s.MetaMinBlue),
		compareGreater: proto.GemColor(s.MetaCompareColorGreater),
		compareLesser:  proto.GemColor(s.MetaCompareColorLesser),
	}
}

func (mc metaCondition) active() bool {
	return mc.minRed > 0 || mc.minYellow > 0 || mc.minBlue > 0 || mc.compareGreater != proto.GemColor_GemColorUnknown
}

// hardCapRemaining returns, per rating stat with a HARD cap (all post-cap EPs zero), the rating
// headroom from gems before the cap (cap rating minus the combo's non-gem rating). A non-positive
// value means the stat is already capped without any gems. Soft caps (positive post-cap EP) are not
// returned here - they're left to contribute full EP (an approximation; the user's caps are hard).
func hardCapRemaining(gemCaps []*proto.GemStatCap, nonGemStats stats.Stats, pseudoStats []float64) map[stats.Stat]float64 {
	rem := make(map[stats.Stat]float64)
	for _, c := range gemCaps {
		if c == nil || len(c.Breakpoints) == 0 {
			continue
		}
		hard := true
		for _, pe := range c.PostCapEps {
			if pe != 0 {
				hard = false
				break
			}
		}
		if !hard {
			continue
		}
		var rs stats.Stat
		var headroom float64
		if c.IsPseudostat {
			r, f, ok := pseudostatRating(proto.PseudoStat(c.UnitStat))
			if !ok {
				continue
			}
			rs = r
			cur := 0.0
			if int(c.UnitStat) < len(pseudoStats) {
				cur = pseudoStats[c.UnitStat]
			}
			headroom = (c.Breakpoints[0] - cur) * f
		} else {
			rs = stats.Stat(c.UnitStat)
			headroom = c.Breakpoints[0] - nonGemStats[rs]
		}
		if old, ok := rem[rs]; !ok || headroom < old {
			rem[rs] = headroom
		}
	}
	return rem
}

// optimizeEquipmentGemsLP re-gems the non-meta sockets by solving the same kind of linear program as
// the browser's Suggest Gems: maximize EP, one gem per socket, unique gems used at most once, stat
// hard caps enforced as constraints, the meta gem's color condition satisfied, and socket bonuses
// chased (per item) when worth it. Returns false if the LP found no usable solution (e.g. the meta
// condition is infeasible for this gear) so the caller can fall back to the greedy optimizer.
func optimizeEquipmentGemsLP(equip *proto.EquipmentSpec, nonGemStats, epWeights stats.Stats, pseudoStats []float64, gemCaps []*proto.GemStatCap, pool []gemOption, mc metaCondition) bool {
	type sref struct {
		item   *proto.ItemSpec
		gemIdx int
	}
	var sockets []sref
	var socketCands [][]gemOption
	for _, spec := range equip.Items {
		if spec == nil || spec.Id == 0 {
			continue
		}
		item := GetItemByID(spec.Id)
		if item == nil {
			continue
		}
		nonMeta := make([]int, 0, len(item.GemSockets))
		for gi, color := range item.GemSockets {
			if color != proto.GemColor_GemColorMeta {
				nonMeta = append(nonMeta, gi)
			}
		}
		if len(nonMeta) == 0 {
			continue
		}
		match := shouldMatchSocketBonus(item, nonMeta, epWeights, pool)
		for _, gi := range nonMeta {
			cands := pool
			if match {
				cands = gemsMatchingColor(pool, item.GemSockets[gi])
			}
			sockets = append(sockets, sref{item: spec, gemIdx: gi})
			socketCands = append(socketCands, cands)
		}
	}
	if len(sockets) == 0 {
		return true // nothing to gem
	}

	rem := hardCapRemaining(gemCaps, nonGemStats, pseudoStats)
	effEP := epWeights // stats.Stats is a value array, so this copies
	for st, r := range rem {
		if r <= 0 {
			effEP[st] = 0 // already capped: no EP for further rating in this stat
		}
	}

	m := &lp.Model{Maximize: true, Objective: "score", Binary: make(map[string]bool)}
	addConstraint := func(key string, c lp.Constraint) {
		m.Constraints = append(m.Constraints, lp.ConstraintEntry{Key: key, Constraint: c})
	}
	capKey := func(st stats.Stat) string { return fmt.Sprintf("cap_%d", int(st)) }

	for st, r := range rem {
		if r > 0 {
			addConstraint(capKey(st), lp.LessEq(r))
		}
	}
	if mc.minRed > 0 {
		addConstraint("metaRed", lp.GreaterEq(float64(mc.minRed)))
	}
	if mc.minYellow > 0 {
		addConstraint("metaYellow", lp.GreaterEq(float64(mc.minYellow)))
	}
	if mc.minBlue > 0 {
		addConstraint("metaBlue", lp.GreaterEq(float64(mc.minBlue)))
	}
	if mc.compareGreater != proto.GemColor_GemColorUnknown {
		addConstraint("metaCompare", lp.GreaterEq(1))
	}

	type vinfo struct {
		socketIdx int
		gemId     int32
	}
	varMap := make(map[string]vinfo)
	uniqueAdded := make(map[int32]bool)

	for si := range sockets {
		socketKey := fmt.Sprintf("socket_%d", si)
		addConstraint(socketKey, lp.EqualTo(1)) // exactly one gem per socket
		for _, g := range socketCands[si] {
			vk := fmt.Sprintf("s%d_g%d", si, g.id)
			coeffs := map[string]float64{socketKey: 1}

			score := 0.0
			for k := 0; k < len(g.stats); k++ {
				if g.stats[k] != 0 {
					score += g.stats[k] * effEP[k]
				}
			}
			coeffs["score"] = score

			for st, r := range rem {
				if r > 0 && g.stats[st] != 0 {
					coeffs[capKey(st)] = g.stats[st]
				}
			}
			if mc.minRed > 0 && ColorIntersects(proto.GemColor_GemColorRed, g.color) {
				coeffs["metaRed"] = 1
			}
			if mc.minYellow > 0 && ColorIntersects(proto.GemColor_GemColorYellow, g.color) {
				coeffs["metaYellow"] = 1
			}
			if mc.minBlue > 0 && ColorIntersects(proto.GemColor_GemColorBlue, g.color) {
				coeffs["metaBlue"] = 1
			}
			if mc.compareGreater != proto.GemColor_GemColorUnknown {
				cv := 0.0
				if ColorIntersects(mc.compareGreater, g.color) {
					cv += 1
				}
				if ColorIntersects(mc.compareLesser, g.color) {
					cv -= 1
				}
				if cv != 0 {
					coeffs["metaCompare"] = cv
				}
			}
			if g.unique {
				uk := fmt.Sprintf("unique_%d", g.id)
				coeffs[uk] = 1
				if !uniqueAdded[g.id] {
					uniqueAdded[g.id] = true
					addConstraint(uk, lp.LessEq(1))
				}
			}

			m.Variables = append(m.Variables, lp.Variable{Key: vk, Coeffs: coeffs})
			m.Binary[vk] = true
			varMap[vk] = vinfo{socketIdx: si, gemId: g.id}
		}
	}

	sol := lp.Solve(m, lp.Options{Timeout: 250 * time.Millisecond})
	if (sol.Status != "optimal" && sol.Status != "timedout") || len(sol.Values) == 0 {
		return false
	}

	assigned := make([]bool, len(sockets))
	for vk, val := range sol.Values {
		if val < 0.5 {
			continue
		}
		vi := varMap[vk]
		ref := sockets[vi.socketIdx]
		for len(ref.item.Gems) <= ref.gemIdx {
			ref.item.Gems = append(ref.item.Gems, 0)
		}
		ref.item.Gems[ref.gemIdx] = vi.gemId
		assigned[vi.socketIdx] = true
	}
	for si := range sockets {
		if !assigned[si] {
			return false // an unfilled socket means the solution was incomplete; let the greedy handle it
		}
	}
	return true
}

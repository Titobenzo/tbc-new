package core

import (
	"github.com/wowsims/tbc/sim/core/proto"
	googleProto "google.golang.org/protobuf/proto"
)

// This is the Go port of the frontend bulk combination logic (getItemsForCombo /
// getAllWeaponCombos / calculateBulkCombinations in ui/.../bulk_tab.tsx). The combination space
// is a cartesian product over "dimensions" - one dimension per varied slot (a normal slot, a
// ring/trinket pair, or a weapon configuration). Each dimension is a list of comboChoices, and a
// combination index decomposes across the dimensions exactly like the frontend (each dimension
// varies in turn). Only the SET of combinations needs to match the frontend, not the index order.

// comboChoice is a partial gear assignment for one dimension: which item goes in which slot.
type comboChoice map[proto.ItemSlot]*proto.ItemSpec

// itemSpecsEqual reports whether two item specs are the same pick (item + suffix + enchant + gems),
// matching EquippedItem.equals on the frontend closely enough for the frozen-item exclusion.
func itemSpecsEqual(a, b *proto.ItemSpec) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Id != b.Id || a.RandomSuffix != b.RandomSuffix || a.Enchant != b.Enchant {
		return false
	}
	if len(a.Gems) != len(b.Gems) {
		return false
	}
	for i := range a.Gems {
		if a.Gems[i] != b.Gems[i] {
			return false
		}
	}
	return true
}

// singleChoices builds the options for a normal (single-item) bulk slot.
func singleChoices(slot proto.ItemSlot, options []*proto.ItemSpec) []comboChoice {
	choices := make([]comboChoice, len(options))
	for i, opt := range options {
		choices[i] = comboChoice{slot: opt}
	}
	return choices
}

// pairChoices builds the ring/trinket pair options for a paired bulk slot, mirroring the frontend:
// with no frozen item, every unordered pair of options (binomial(n, 2)); with a frozen item, the
// frozen item paired with each of the other options (n-1, since frozen is one of the options).
func pairChoices(slots [2]proto.ItemSlot, options []*proto.ItemSpec, frozen *proto.ItemSpec) []comboChoice {
	choices := []comboChoice{}
	if frozen != nil {
		for _, opt := range options {
			if itemSpecsEqual(opt, frozen) {
				continue
			}
			choices = append(choices, comboChoice{slots[0]: frozen, slots[1]: opt})
		}
		return choices
	}
	for i := 0; i < len(options); i++ {
		for j := i + 1; j < len(options); j++ {
			choices = append(choices, comboChoice{slots[0]: options[i], slots[1]: options[j]})
		}
	}
	return choices
}

// comboCount is the total number of combinations: the product of the dimension sizes.
func comboCount(dims [][]comboChoice) int {
	total := 1
	for _, dim := range dims {
		total *= len(dim)
	}
	return total
}

// comboAt returns the merged gear assignment for combination index i, decomposing i across the
// dimensions like the frontend's getItemsForCombo (the first dimension varies fastest).
func comboAt(dims [][]comboChoice, i int) comboChoice {
	result := comboChoice{}
	for _, dim := range dims {
		n := len(dim)
		for slot, item := range dim[i%n] {
			result[slot] = item
		}
		i /= n
	}
	return result
}

// dimensionsToChoices converts the proto combination dimensions sent by the frontend into the
// internal comboChoice form used by comboCount / comboAt.
func dimensionsToChoices(dims []*proto.BulkComboDimension) [][]comboChoice {
	result := make([][]comboChoice, len(dims))
	for i, dim := range dims {
		choices := make([]comboChoice, len(dim.Choices))
		for j, ch := range dim.Choices {
			choice := comboChoice{}
			for _, si := range ch.Items {
				choice[si.Slot] = si.Item
			}
			choices[j] = choice
		}
		result[i] = choices
	}
	return result
}

// BulkComboCount returns the total number of combinations described by the dimensions (the
// cartesian product across them). This must match the browser's own combination count.
func BulkComboCount(dims []*proto.BulkComboDimension) int {
	return comboCount(dimensionsToChoices(dims))
}

// applyComboToEquipment writes the combination's items into the equipment (indexed by item slot),
// leaving every slot the combination doesn't touch at its base value. The runner reuses one base
// equipment per worker and rewrites only the varied slots before each sim.
//
// The combo's ItemSpecs are shared across every worker and every combination (they come straight from
// the request dimensions). The gem optimizer mutates ItemSpec.Gems in place, so each slot must get a
// CLONE - otherwise concurrent workers re-gemming the same shared spec race and blank out its sockets.
func applyComboToEquipment(equip *proto.EquipmentSpec, combo comboChoice) {
	for slot, item := range combo {
		idx := int(slot)
		for len(equip.Items) <= idx {
			equip.Items = append(equip.Items, &proto.ItemSpec{})
		}
		equip.Items[idx] = googleProto.Clone(item).(*proto.ItemSpec)
	}
}

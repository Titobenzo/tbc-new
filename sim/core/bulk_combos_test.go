package core

import (
	"fmt"
	"testing"

	"github.com/wowsims/tbc/sim/core/proto"
)

func bulkSpec(id int32) *proto.ItemSpec { return &proto.ItemSpec{Id: id} }

// Enumerate every combination and assert the count matches and every combination is unique.
func assertCombos(t *testing.T, dims [][]comboChoice, wantCount int) []comboChoice {
	t.Helper()
	if got := comboCount(dims); got != wantCount {
		t.Fatalf("comboCount = %d, want %d", got, wantCount)
	}
	seen := map[string]bool{}
	combos := make([]comboChoice, 0, wantCount)
	for i := 0; i < wantCount; i++ {
		c := comboAt(dims, i)
		key := ""
		for slot := proto.ItemSlot(0); int(slot) < 20; slot++ {
			if item, ok := c[slot]; ok {
				key += fmt.Sprintf("|%d:%d", slot, item.Id)
			}
		}
		if seen[key] {
			t.Fatalf("duplicate combination at index %d: %s", i, key)
		}
		seen[key] = true
		combos = append(combos, c)
	}
	if len(seen) != wantCount {
		t.Fatalf("unique combinations = %d, want %d", len(seen), wantCount)
	}
	return combos
}

func TestBulkComboSingleSlots(t *testing.T) {
	// 3 heads x 2 chests = 6 unique combos.
	dims := [][]comboChoice{
		singleChoices(proto.ItemSlot_ItemSlotHead, []*proto.ItemSpec{bulkSpec(1), bulkSpec(2), bulkSpec(3)}),
		singleChoices(proto.ItemSlot_ItemSlotChest, []*proto.ItemSpec{bulkSpec(4), bulkSpec(5)}),
	}
	assertCombos(t, dims, 6)
}

func TestBulkComboPairsNoFrozen(t *testing.T) {
	// 4 ring options, no frozen -> binomial(4,2) = 6 pairs, each filling both finger slots.
	pairs := pairChoices(
		[2]proto.ItemSlot{proto.ItemSlot_ItemSlotFinger1, proto.ItemSlot_ItemSlotFinger2},
		[]*proto.ItemSpec{bulkSpec(1), bulkSpec(2), bulkSpec(3), bulkSpec(4)},
		nil,
	)
	if len(pairs) != 6 {
		t.Fatalf("pairChoices = %d, want 6", len(pairs))
	}
	for _, p := range pairs {
		if p[proto.ItemSlot_ItemSlotFinger1] == nil || p[proto.ItemSlot_ItemSlotFinger2] == nil {
			t.Fatalf("pair did not fill both finger slots: %v", p)
		}
	}
}

func TestBulkComboPairsFrozen(t *testing.T) {
	// Frozen is one of the 4 options -> frozen paired with each of the other 3 = 3 pairs.
	options := []*proto.ItemSpec{bulkSpec(1), bulkSpec(2), bulkSpec(3), bulkSpec(4)}
	frozen := bulkSpec(1)
	pairs := pairChoices([2]proto.ItemSlot{proto.ItemSlot_ItemSlotTrinket1, proto.ItemSlot_ItemSlotTrinket2}, options, frozen)
	if len(pairs) != 3 {
		t.Fatalf("frozen pairChoices = %d, want 3", len(pairs))
	}
	for _, p := range pairs {
		if !itemSpecsEqual(p[proto.ItemSlot_ItemSlotTrinket1], frozen) {
			t.Fatalf("frozen item not in trinket1 slot: %v", p)
		}
	}
}

func TestBulkComboFromProtoDimensions(t *testing.T) {
	// Build proto dimensions like the frontend would: 2 weapon configs x 3 helms x 6 ring pairs = 36.
	mkChoice := func(items ...*proto.BulkComboSlotItem) *proto.BulkComboChoice {
		return &proto.BulkComboChoice{Items: items}
	}
	slotItem := func(slot proto.ItemSlot, id int32) *proto.BulkComboSlotItem {
		return &proto.BulkComboSlotItem{Slot: slot, Item: bulkSpec(id)}
	}

	ringPairs := pairChoices(
		[2]proto.ItemSlot{proto.ItemSlot_ItemSlotFinger1, proto.ItemSlot_ItemSlotFinger2},
		[]*proto.ItemSpec{bulkSpec(10), bulkSpec(11), bulkSpec(12), bulkSpec(13)}, nil)
	ringChoices := make([]*proto.BulkComboChoice, len(ringPairs))
	for i, p := range ringPairs {
		ringChoices[i] = mkChoice(
			&proto.BulkComboSlotItem{Slot: proto.ItemSlot_ItemSlotFinger1, Item: p[proto.ItemSlot_ItemSlotFinger1]},
			&proto.BulkComboSlotItem{Slot: proto.ItemSlot_ItemSlotFinger2, Item: p[proto.ItemSlot_ItemSlotFinger2]},
		)
	}

	dims := []*proto.BulkComboDimension{
		{Choices: []*proto.BulkComboChoice{
			mkChoice(slotItem(proto.ItemSlot_ItemSlotMainHand, 100)),
			mkChoice(slotItem(proto.ItemSlot_ItemSlotMainHand, 101)),
		}},
		{Choices: []*proto.BulkComboChoice{
			mkChoice(slotItem(proto.ItemSlot_ItemSlotHead, 1)),
			mkChoice(slotItem(proto.ItemSlot_ItemSlotHead, 2)),
			mkChoice(slotItem(proto.ItemSlot_ItemSlotHead, 3)),
		}},
		{Choices: ringChoices},
	}

	if got := BulkComboCount(dims); got != 36 {
		t.Fatalf("BulkComboCount = %d, want 36", got)
	}

	// Enumerate via the converted form and confirm every combo fills the expected slots uniquely.
	assertCombos(t, dimensionsToChoices(dims), 36)
}

func TestApplyComboToEquipment(t *testing.T) {
	// Base equipment: distinct item in every slot 0..18.
	base := &proto.EquipmentSpec{}
	for s := 0; s < 19; s++ {
		base.Items = append(base.Items, bulkSpec(int32(1000+s)))
	}

	// Apply a combo that changes head, finger1, finger2.
	combo := comboChoice{
		proto.ItemSlot_ItemSlotHead:    bulkSpec(50),
		proto.ItemSlot_ItemSlotFinger1: bulkSpec(60),
		proto.ItemSlot_ItemSlotFinger2: bulkSpec(61),
	}
	applyComboToEquipment(base, combo)

	if base.Items[proto.ItemSlot_ItemSlotHead].Id != 50 {
		t.Fatalf("head = %d, want 50", base.Items[proto.ItemSlot_ItemSlotHead].Id)
	}
	if base.Items[proto.ItemSlot_ItemSlotFinger1].Id != 60 || base.Items[proto.ItemSlot_ItemSlotFinger2].Id != 61 {
		t.Fatalf("fingers = %d,%d want 60,61", base.Items[proto.ItemSlot_ItemSlotFinger1].Id, base.Items[proto.ItemSlot_ItemSlotFinger2].Id)
	}
	// Chest (untouched) must keep its base item.
	if base.Items[proto.ItemSlot_ItemSlotChest].Id != 1000+int32(proto.ItemSlot_ItemSlotChest) {
		t.Fatalf("chest was modified: %d", base.Items[proto.ItemSlot_ItemSlotChest].Id)
	}
}

func TestBulkComboMixedProduct(t *testing.T) {
	// 2 weapon configs x 3 heads x binomial(4,2)=6 ring pairs = 36 unique combos.
	weaponDim := []comboChoice{
		{proto.ItemSlot_ItemSlotMainHand: bulkSpec(100)},
		{proto.ItemSlot_ItemSlotMainHand: bulkSpec(101)},
	}
	dims := [][]comboChoice{
		weaponDim,
		singleChoices(proto.ItemSlot_ItemSlotHead, []*proto.ItemSpec{bulkSpec(1), bulkSpec(2), bulkSpec(3)}),
		pairChoices([2]proto.ItemSlot{proto.ItemSlot_ItemSlotFinger1, proto.ItemSlot_ItemSlotFinger2},
			[]*proto.ItemSpec{bulkSpec(10), bulkSpec(11), bulkSpec(12), bulkSpec(13)}, nil),
	}
	combos := assertCombos(t, dims, 36)
	// Every combo should assign main hand, head, and both fingers.
	for _, c := range combos {
		for _, slot := range []proto.ItemSlot{
			proto.ItemSlot_ItemSlotMainHand, proto.ItemSlot_ItemSlotHead,
			proto.ItemSlot_ItemSlotFinger1, proto.ItemSlot_ItemSlotFinger2,
		} {
			if c[slot] == nil {
				t.Fatalf("combo missing slot %d: %v", slot, c)
			}
		}
	}
}

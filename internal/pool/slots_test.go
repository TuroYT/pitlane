package pool

import "testing"

// An adopted runner can sit in a slot far above the configured count (the
// slot count was lowered, or the label is odd); the dashboard must still
// show it, and in index order.
func TestSlotsListsHighIndexes(t *testing.T) {
	p := newTestPool("owner",
		SlotState{Index: 0, Status: "ready"},
		SlotState{Index: 1, Status: "ready"},
		SlotState{Index: 57, Status: "busy", Owner: "acme"},
		SlotState{Index: 20, Status: "busy", Owner: "acme"},
	)
	var got []int
	for _, s := range p.Slots() {
		got = append(got, s.Index)
	}
	want := []int{0, 1, 20, 57}
	if len(got) != len(want) {
		t.Fatalf("Slots lists %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Slots lists %v, want %v", got, want)
		}
	}
}

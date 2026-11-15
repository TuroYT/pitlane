package server

import "testing"

func TestMemorySizing(t *testing.T) {
	const gb = 1 << 30
	// 15 GB usable; p90 = 3.5 GB -> 4 slots.
	peaks := []float64{1.8 * gb, 2 * gb, 2.2 * gb, 2.5 * gb, 2.6 * gb, 2.8 * gb, 3 * gb, 3.2 * gb, 3.5 * gb, 3.7 * gb}
	m := newMemorySizing(peaks, 16*gb, 15*gb, 8, 0)
	if m.Count != 10 || *m.P50 != 2.6*gb || *m.P90 != 3.5*gb || *m.Max != 3.7*gb {
		t.Fatalf("quantiles: %+v", m)
	}
	if m.Usable != 15*gb || m.SuggestedSlots == nil || *m.SuggestedSlots != 4 || m.Slots != 8 || m.WindowDays != 7 {
		t.Fatalf("sizing: %+v", m)
	}
	// Small host.
	if m := newMemorySizing(peaks, 2*gb, gb, 2, 1024); m.Usable != gb || *m.SuggestedSlots != 0 || m.MemoryLimitMB != 1024 {
		t.Fatalf("small host: %+v", m)
	}
	// No measured job: no suggestion.
	if m := newMemorySizing(nil, 16*gb, 15*gb, 2, 0); m.P90 != nil || m.SuggestedSlots != nil || m.Count != 0 {
		t.Fatalf("empty: %+v", m)
	}
	// Unknown host memory: quantiles, no suggestion.
	if m := newMemorySizing(peaks, 0, 0, 2, 0); m.P90 == nil || m.SuggestedSlots != nil {
		t.Fatalf("no host memory: %+v", m)
	}
}

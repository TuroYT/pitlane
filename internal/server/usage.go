package server

import (
	"math"
	"net/http"
	"sort"
	"time"
)

// memoryWindow is the job history the memory sizing looks at.
const memoryWindow = 7 * 24 * time.Hour

// memorySizing summarizes the memory peaks of recent jobs and turns them
// into a slot count suggestion.
type memorySizing struct {
	WindowDays int `json:"windowDays"`
	Count      int `json:"count"` // measured jobs in the window
	// Peak memory per job (runner + slot, bytes); nil without measured jobs.
	P50 *float64 `json:"p50"`
	P90 *float64 `json:"p90"`
	Max *float64 `json:"max"`
	// MemTotal is the memory Pitlane's host (or LXC container) has, Usable
	// what is left for jobs once the memory reserve is set aside (the
	// admission's budget, see pool.Admit).
	MemTotal int64 `json:"memTotal"`
	Usable   int64 `json:"usable"`
	// SuggestedSlots = floor(Usable / P90); nil without measured jobs.
	SuggestedSlots *int  `json:"suggestedSlots"`
	Slots          int   `json:"slots"`         // configured
	MemoryLimitMB  int64 `json:"memoryLimitMb"` // per runner and per slot daemon, 0 = none
}

func newMemorySizing(peaks []float64, memTotal, usable int64, slots int, limitMB int64) memorySizing {
	m := memorySizing{WindowDays: int(memoryWindow / (24 * time.Hour)), Count: len(peaks), MemTotal: memTotal, Slots: slots, MemoryLimitMB: limitMB}
	if memTotal > 0 {
		m.Usable = usable
	}
	if len(peaks) == 0 {
		return m
	}
	v := append([]float64(nil), peaks...)
	sort.Float64s(v)
	p50, p90, top := quantile(v, 0.5), quantile(v, 0.9), v[len(v)-1]
	m.P50, m.P90, m.Max = &p50, &p90, &top
	if p90 > 0 && m.Usable > 0 {
		n := int(math.Floor(float64(m.Usable) / p90))
		m.SuggestedSlots = &n
	}
	return m
}

func (s *Server) handleJobMemory(w http.ResponseWriter, r *http.Request) {
	peaks, err := s.store.JobMemoryPeaks(r.Context(), time.Now().Add(-memoryWindow).Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cfg, mem := s.pool.Config(), s.pool.Memory()
	writeJSON(w, newMemorySizing(peaks, mem.Total, mem.Budget, cfg.Slots, cfg.MemoryMB))
}

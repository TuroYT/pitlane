package server

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
	"github.com/TuroYT/pitlane/internal/store"
)

// Settings editable from the UI.
type Settings struct {
	Slots         int    `json:"slots"`
	RunnerImage   string `json:"runnerImage"`
	DindImage     string `json:"dindImage"`
	CPUShares     int64  `json:"cpuShares"`
	MemoryMB      int64  `json:"memoryMb"`
	CacheScope    string `json:"cacheScope"`
	Mirror        bool   `json:"mirror"`
	QEMU          bool   `json:"qemu"`
	KeepBuildGB   int    `json:"keepBuildGb"`
	MinFreeDiskGB int    `json:"minFreeDiskGb"`
	HistoryDays   int    `json:"historyDays"`
	BaseURL       string `json:"baseUrl"`
	// Bearer token required by /metrics. Never returned: GET only says
	// whether one is set; PUT changes it only when the field is present
	// ("" removes it).
	MetricsToken    *string `json:"metricsToken,omitempty"`
	MetricsTokenSet bool    `json:"metricsTokenSet"`
	// Actions cache served from the host (internal/cache). Its stats are
	// read-only, nil when the cache listener is not running.
	ActionsCache   bool `json:"actionsCache"`
	ActionsCacheGB int  `json:"actionsCacheGb"`
	// Share of the actions cache size one owner may use, in percent.
	ActionsCacheOwnerPct int          `json:"actionsCacheOwnerPct"`
	ActionsCacheStats    *cache.Stats `json:"actionsCacheStats,omitempty"`
	// Memory admission (pool.Admit) and its reserve; slots reserved for
	// strict jobs (pool.AutoStrictSlots = -1: one while a strict target
	// is enabled). StrictSlotsActive is what applies now (read-only).
	MemoryAdmission   bool  `json:"memoryAdmission"`
	MemoryReserveMB   int64 `json:"memoryReserveMb"`
	StrictSlots       int   `json:"strictSlots"`
	StrictSlotsActive int   `json:"strictSlotsActive"`
}

// LoadPoolConfig reads the pool configuration from the store.
func LoadPoolConfig(st *store.Store, dataDir string) pool.Config {
	return pool.Config{
		StateFile:            filepath.Join(dataDir, "slots.json"),
		Slots:                st.GetInt("slots", DefaultSlots()),
		RunnerImage:          st.Get("runner_image", ""),
		DindImage:            st.Get("dind_image", ""),
		CPUShares:            int64(st.GetInt("cpu_shares", 0)),
		MemoryMB:             int64(st.GetInt("memory_mb", 0)),
		CacheScope:           st.Get("cache_scope", "owner"),
		Mirror:               st.Get("mirror", "true") == "true",
		NoQEMU:               st.Get("qemu", "true") != "true",
		KeepBuildGB:          st.GetInt("keep_build_gb", 20),
		MinFreeDiskGB:        st.GetInt("min_free_disk_gb", 20),
		ActionsCache:         st.Get("actions_cache", "true") == "true",
		ActionsCacheGB:       st.GetInt("actions_cache_gb", 20),
		ActionsCacheOwnerPct: st.GetInt("actions_cache_owner_pct", cache.DefaultOwnerPercent),
		MemoryAdmission:      st.Get("memory_admission", "true") == "true",
		MemoryReserveMB:      int64(st.GetInt("memory_reserve_mb", pool.DefaultMemoryReserveMB)),
		StrictSlots:          st.GetInt("strict_slots", pool.AutoStrictSlots),
	}
}

// DefaultSlots is half the CPUs, between 1 and 8.
func DefaultSlots() int { return min(8, max(1, runtime.NumCPU()/2)) }

// settings returns the current settings, as GET /api/settings shows them.
func (s *Server) settings(r *http.Request) Settings {
	c := s.pool.Config()
	return Settings{
		Slots: c.Slots, RunnerImage: c.RunnerImage, DindImage: c.DindImage, CPUShares: c.CPUShares, MemoryMB: c.MemoryMB,
		CacheScope: c.CacheScope, Mirror: c.Mirror, QEMU: !c.NoQEMU, KeepBuildGB: c.KeepBuildGB, MinFreeDiskGB: c.MinFreeDiskGB,
		HistoryDays: s.store.GetInt("history_days", 30), BaseURL: s.knownBaseURL(r),
		MetricsTokenSet: s.store.Get(metricsTokenKey, "") != "",
		ActionsCache:    c.ActionsCache, ActionsCacheGB: c.ActionsCacheGB, ActionsCacheOwnerPct: c.ActionsCacheOwnerPct,
		ActionsCacheStats: s.actionsCacheStats(),
		MemoryAdmission:   c.MemoryAdmission, MemoryReserveMB: c.MemoryReserveMB, StrictSlots: c.StrictSlots, StrictSlotsActive: s.pool.StrictSlots(),
	}
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.settings(r))
}

// settingKeys are the settings PUT /api/settings saves, in the order the
// audit log lists their changes.
var settingKeys = []string{"slots", "runner_image", "dind_image", "cpu_shares", "memory_mb", "cache_scope", "mirror", "qemu",
	"keep_build_gb", "min_free_disk_gb", "actions_cache", "actions_cache_gb", "actions_cache_owner_pct", "memory_admission", "memory_reserve_mb",
	"strict_slots", "history_days", "base_url"}

// settingValues are the stored values of the settings in.
func settingValues(in Settings) map[string]string {
	if in.CacheScope != "owner" && in.CacheScope != "shared" {
		in.CacheScope = "owner"
	}
	kv := map[string]string{
		"slots": strconv.Itoa(in.Slots), "runner_image": strings.TrimSpace(in.RunnerImage), "dind_image": strings.TrimSpace(in.DindImage),
		"cpu_shares": strconv.FormatInt(max(0, in.CPUShares), 10), "memory_mb": strconv.FormatInt(max(0, in.MemoryMB), 10),
		"cache_scope": in.CacheScope, "mirror": strconv.FormatBool(in.Mirror), "qemu": strconv.FormatBool(in.QEMU), "keep_build_gb": strconv.Itoa(max(1, in.KeepBuildGB)),
		"history_days":     strconv.Itoa(max(1, in.HistoryDays)),
		"min_free_disk_gb": strconv.Itoa(max(0, in.MinFreeDiskGB)),
		"actions_cache":    strconv.FormatBool(in.ActionsCache), "actions_cache_gb": strconv.Itoa(max(1, in.ActionsCacheGB)),
		"actions_cache_owner_pct": strconv.Itoa(min(100, max(1, in.ActionsCacheOwnerPct))),
		"memory_admission":        strconv.FormatBool(in.MemoryAdmission), "memory_reserve_mb": strconv.FormatInt(max(0, in.MemoryReserveMB), 10),
		"strict_slots": strconv.Itoa(max(pool.AutoStrictSlots, in.StrictSlots)),
	}
	if b := strings.TrimRight(strings.TrimSpace(in.BaseURL), "/"); b != "" {
		kv["base_url"] = b
	}
	return kv
}

// actionsCacheStats returns the actions cache statistics, nil when the cache
// is not serving.
func (s *Server) actionsCacheStats() *cache.Stats {
	if s.cache == nil {
		return nil
	}
	st := s.cache.Stats()
	return &st
}

const (
	// maxSlots is the most parallel jobs the settings accept.
	maxSlots = 64
	// minMetricsToken is the shortest metrics token accepted.
	minMetricsToken = 16
)

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	in := s.settingsBase()
	if !readJSON(w, r.Body, &in) {
		return
	}
	if msg := validateSettings(in); msg != "" {
		writeErr(w, http.StatusBadRequest, msg)
		return
	}
	before := settingValues(s.settings(r))
	metricsAudit, err := s.saveMetricsToken(in.MetricsToken)
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "settings_save", "cannot save the settings", err)
		return
	}
	kv := settingValues(in)
	if err := s.saveSettings(kv); err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "settings_save", "cannot save the settings", err)
		return
	}
	if detail := joinNonEmpty("; ", changes(settingKeys, before, kv), metricsAudit); detail != "" {
		s.audit(r, "settings.update", "", detail)
	}
	// Applying may rebuild slots: it must not be cut short with the request.
	go s.applyPoolConfig(context.WithoutCancel(r.Context()))
	s.handleGetSettings(w, r)
}

// settingsBase is what a PUT /api/settings body is decoded into: the fields
// added after the first release keep their current value when left out, so
// an older UI does not reset them.
func (s *Server) settingsBase() Settings {
	cur := s.pool.Config()
	return Settings{MinFreeDiskGB: cur.MinFreeDiskGB, ActionsCache: cur.ActionsCache, ActionsCacheGB: cur.ActionsCacheGB,
		ActionsCacheOwnerPct: cur.ActionsCacheOwnerPct, MemoryAdmission: cur.MemoryAdmission, MemoryReserveMB: cur.MemoryReserveMB,
		StrictSlots: cur.StrictSlots}
}

// validateSettings returns why in cannot be saved, or "". Other values out
// of range are clamped by settingValues.
func validateSettings(in Settings) string {
	switch {
	case in.Slots < 1 || in.Slots > maxSlots:
		return fmt.Sprintf("parallel jobs must be between 1 and %d", maxSlots)
	case in.StrictSlots < pool.AutoStrictSlots || in.StrictSlots >= in.Slots:
		return "slots reserved for strict jobs must be -1 (auto) or between 0 and the number of parallel jobs minus one"
	case in.MemoryReserveMB < 0:
		return "the memory reserve cannot be negative"
	}
	if in.MetricsToken != nil {
		if tok := strings.TrimSpace(*in.MetricsToken); tok != "" && len(tok) < minMetricsToken {
			return fmt.Sprintf("the metrics token must be at least %d characters", minMetricsToken)
		}
	}
	return ""
}

// saveMetricsToken saves the metrics token of a settings request: nil keeps
// it, "" removes it. It returns what the audit log should say ("" for
// nothing).
func (s *Server) saveMetricsToken(tok *string) (string, error) {
	if tok == nil {
		return "", nil
	}
	if t := strings.TrimSpace(*tok); t != "" {
		if err := s.store.SetSecret(metricsTokenKey, t); err != nil {
			return "", fmt.Errorf("save the metrics token: %w", err)
		}
		return "metrics_token changed", nil
	}
	audit := ""
	if s.store.Get(metricsTokenKey, "") != "" {
		audit = "metrics_token removed"
	}
	if err := s.store.Set(metricsTokenKey, ""); err != nil {
		return "", fmt.Errorf("remove the metrics token: %w", err)
	}
	return audit, nil
}

// saveSettings stores the settings values kv, in settingKeys order.
func (s *Server) saveSettings(kv map[string]string) error {
	for _, k := range settingKeys {
		v, ok := kv[k]
		if !ok {
			continue
		}
		if err := s.store.Set(k, v); err != nil {
			return fmt.Errorf("save %s: %w", k, err)
		}
	}
	return nil
}

// applyPoolConfig applies the stored settings to the pool and tells the UI.
func (s *Server) applyPoolConfig(ctx context.Context) {
	if err := s.pool.Configure(ctx, LoadPoolConfig(s.store, s.cfg.DataDir)); err != nil {
		s.log.Error("apply settings", "error", err)
	}
	s.Notify()
}

// joinNonEmpty joins the non-empty parts with sep.
func joinNonEmpty(sep string, parts ...string) string {
	return strings.Join(slices.DeleteFunc(parts, func(x string) bool { return x == "" }), sep)
}

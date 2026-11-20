package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/cache"
	"github.com/TuroYT/pitlane/internal/pool"
)

// currentBody is a PUT body that changes nothing of a fresh apiServer (the
// first fields of Settings must be sent, see settingsBase).
const currentBody = `"slots":4,"cacheScope":"owner","mirror":true,"qemu":true,"keepBuildGb":20,"historyDays":30`

func decodeSettings(t *testing.T, body string) Settings {
	t.Helper()
	var s Settings
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		t.Fatalf("%q: %v", body, err)
	}
	return s
}

func TestGetSettings(t *testing.T) {
	t.Parallel()
	s, st, fp, h := apiServer(t)
	fp.cfg = pool.Config{Slots: 6, RunnerImage: "runner:1", CacheScope: "shared", Mirror: true, ActionsCache: true, ActionsCacheGB: 30,
		ActionsCacheOwnerPct: 40, MemoryAdmission: true, MemoryReserveMB: 2048, StrictSlots: 1, MinFreeDiskGB: 10, NoQEMU: true}
	fp.strictSlots = 2
	_ = st.Set("history_days", "14")
	s.SetActionsCache(&cache.Server{})

	for _, who := range []string{"", "vera", "oscar"} {
		w := get(h, "/api/settings", who)
		want := http.StatusUnauthorized
		if who != "" {
			want = http.StatusForbidden
		}
		if w.Code != want {
			t.Errorf("as %q: %d, want %d", who, w.Code, want)
		}
	}
	w := get(h, "/api/settings", "alice")
	got := decodeSettings(t, w.Body.String())
	if w.Code != http.StatusOK || got.Slots != 6 || got.RunnerImage != "runner:1" || got.CacheScope != "shared" || got.QEMU || got.HistoryDays != 14 ||
		got.BaseURL != "http://pitlane.test" || got.ActionsCacheGB != 30 || got.ActionsCacheOwnerPct != 40 || got.MemoryReserveMB != 2048 ||
		got.StrictSlots != 1 || got.StrictSlotsActive != 2 || got.MetricsTokenSet || got.ActionsCacheStats == nil {
		t.Fatalf("%d %+v", w.Code, got)
	}
	// The token itself is never returned, only whether one is set.
	const token = "0123456789abcdef-secret"
	if err := st.SetSecret(metricsTokenKey, token); err != nil {
		t.Fatal(err)
	}
	w = get(h, "/api/settings", "alice")
	if got := decodeSettings(t, w.Body.String()); !got.MetricsTokenSet || got.MetricsToken != nil || bodyHas(w, token) || bodyHas(w, "metricsToken\"") {
		t.Fatalf("token leaked or not flagged: %s", w.Body)
	}
}

// putSettings sends a PUT as the admin and waits for the pool to be
// configured when the save succeeded.
func putSettings(t *testing.T, h http.Handler, fp *fakePool, body string) (int, string) {
	t.Helper()
	w := send(h, "PUT", "/api/settings", body, session("alice"))
	if w.Code == http.StatusOK {
		select {
		case <-fp.configured:
		case <-time.After(10 * time.Second):
			t.Fatal("pool not reconfigured after the settings were saved")
		}
	}
	return w.Code, w.Body.String()
}

func TestPutSettingsSavesAndApplies(t *testing.T) {
	t.Parallel()
	s, st, fp, h := apiServer(t)
	ch := s.hub.subscribe()
	code, body := putSettings(t, h, fp, `{"slots":6,"runnerImage":" runner:2 ","cacheScope":"bogus","mirror":true,"qemu":true,"keepBuildGb":0,
		"historyDays":14,"baseUrl":"https://p.example/","memoryAdmission":true,"memoryReserveMb":1024,"strictSlots":2,"minFreeDiskGb":5,
		"actionsCache":true,"actionsCacheGb":50,"actionsCacheOwnerPct":500}`)
	if code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	for k, want := range map[string]string{
		"slots": "6", "runner_image": "runner:2", "cache_scope": "owner", "mirror": "true", "qemu": "true", "keep_build_gb": "1",
		"history_days": "14", "base_url": "https://p.example", "memory_admission": "true", "memory_reserve_mb": "1024",
		"strict_slots": "2", "min_free_disk_gb": "5", "actions_cache": "true", "actions_cache_gb": "50", "actions_cache_owner_pct": "100",
	} {
		if got := st.Get(k, "<unset>"); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	// The pool is rebuilt from the stored values, and the UI is told.
	if c := LoadPoolConfig(st, s.cfg.DataDir); c.Slots != 6 || c.RunnerImage != "runner:2" || !c.ActionsCache {
		t.Fatalf("pool config %+v", c)
	}
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the UI was not notified")
	}
	// The change reaches the audit log.
	entries, _, _ := st.AuditPage(t.Context(), 5, 0)
	if len(entries) != 1 || entries[0].Action != "settings.update" || entries[0].Actor != "alice" || !strings.Contains(entries[0].Detail, "slots: 4 → 6") {
		t.Fatalf("audit %+v", entries)
	}
	// Saving the same settings again changes nothing: no audit entry.
	if code, body := putSettings(t, h, fp, `{"slots":6,"runnerImage":"runner:2","mirror":true,"qemu":true,"keepBuildGb":1,"historyDays":14,
		"memoryAdmission":true,"memoryReserveMb":1024,"strictSlots":2,"minFreeDiskGb":5,"actionsCache":true,
		"actionsCacheGb":50,"actionsCacheOwnerPct":100,"cacheScope":"owner","cpuShares":0,"memoryMb":0}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	if entries, total, _ := st.AuditPage(t.Context(), 5, 0); total != 1 {
		t.Fatalf("unchanged settings audited: %+v", entries)
	}
}

// Fields added after the first release keep their value when a PUT leaves
// them out (an older UI must not reset them).
func TestPutSettingsKeepsFieldsLeftOut(t *testing.T) {
	t.Parallel()
	_, st, fp, h := apiServer(t)
	fp.cfg.MinFreeDiskGB, fp.cfg.ActionsCache, fp.cfg.ActionsCacheGB, fp.cfg.ActionsCacheOwnerPct = 7, true, 33, 25
	fp.cfg.MemoryAdmission, fp.cfg.MemoryReserveMB, fp.cfg.StrictSlots = true, 512, 1
	if code, body := putSettings(t, h, fp, `{`+currentBody+`}`); code != http.StatusOK {
		t.Fatalf("%d %s", code, body)
	}
	for k, want := range map[string]string{
		"min_free_disk_gb": "7", "actions_cache": "true", "actions_cache_gb": "33", "actions_cache_owner_pct": "25",
		"memory_admission": "true", "memory_reserve_mb": "512", "strict_slots": "1",
	} {
		if got := st.Get(k, ""); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestPutSettingsValidation(t *testing.T) {
	t.Parallel()
	short := `"short"`
	for _, tc := range []struct {
		name, body, want string
	}{
		{"not JSON", `slots=4`, "invalid JSON body"},
		{"wrong type", `{"slots":"four"}`, "invalid JSON body"},
		{"no slot", `{"slots":0}`, "parallel jobs"},
		{"too many slots", `{"slots":65}`, "parallel jobs"},
		{"strict slots as many as slots", `{"slots":4,"strictSlots":4}`, "strict jobs"},
		{"strict slots below auto", `{"slots":4,"strictSlots":-2}`, "strict jobs"},
		{"negative reserve", `{"slots":4,"memoryReserveMb":-1}`, "memory reserve"},
		{"short metrics token", `{"slots":4,"metricsToken":` + short + `}`, "metrics token must be at least 16"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, st, _, h := apiServer(t)
			w := send(h, "PUT", "/api/settings", tc.body, session("alice"))
			if w.Code != http.StatusBadRequest || !bodyHas(w, tc.want) {
				t.Fatalf("%d %s, want 400 %q", w.Code, w.Body, tc.want)
			}
			if got := st.Get("slots", "<unset>"); got != "<unset>" {
				t.Errorf("a refused request saved slots = %q", got)
			}
			if entries, total, _ := st.AuditPage(t.Context(), 5, 0); total != 0 {
				t.Errorf("a refused request was audited: %+v", entries)
			}
		})
	}
}

func TestPutSettingsMetricsToken(t *testing.T) {
	t.Parallel()
	_, st, fp, h := apiServer(t)
	const token = "0123456789abcdef-secret"
	secret := func() string { v, _ := st.GetSecret(metricsTokenKey); return v }
	audit := func() string {
		entries, _, _ := st.AuditPage(t.Context(), 1, 0)
		if len(entries) == 0 {
			return ""
		}
		return entries[0].Detail
	}

	// Left out: nothing changes.
	if code, _ := putSettings(t, h, fp, `{`+currentBody+`}`); code != http.StatusOK || secret() != "" || audit() != "" {
		t.Fatalf("token left out: %q audit %q", secret(), audit())
	}
	// Set (spaces are trimmed). The audit log says it changed, not to what.
	code, body := putSettings(t, h, fp, `{`+currentBody+`,"metricsToken":"  `+token+`  "}`)
	if code != http.StatusOK || secret() != token || audit() != "metrics_token changed" || strings.Contains(body, token) {
		t.Fatalf("set: %d %q audit %q body %s", code, secret(), audit(), body)
	}
	if raw := st.Get(metricsTokenKey, ""); strings.Contains(raw, token) {
		t.Fatal("the token is stored in clear")
	}
	if got := decodeSettings(t, body); !got.MetricsTokenSet {
		t.Fatalf("metricsTokenSet false after setting a token: %s", body)
	}
	// Left out again: kept.
	if code, _ := putSettings(t, h, fp, `{`+currentBody+`}`); code != http.StatusOK || secret() != token {
		t.Fatalf("token lost by a PUT that left it out: %q", secret())
	}
	// Blank removes it.
	if code, _ := putSettings(t, h, fp, `{`+currentBody+`,"metricsToken":" "}`); code != http.StatusOK || secret() != "" || audit() != "metrics_token removed" {
		t.Fatalf("remove: %q audit %q", secret(), audit())
	}
	// Removing a token that is not set is not an event.
	_, total0, _ := st.AuditPage(t.Context(), 1, 0)
	if code, _ := putSettings(t, h, fp, `{`+currentBody+`,"metricsToken":""}`); code != http.StatusOK {
		t.Fatal("blank token refused")
	}
	if _, total, _ := st.AuditPage(t.Context(), 1, 0); total != total0 {
		t.Fatalf("removing no token was audited (%d -> %d entries)", total0, total)
	}
}

// A failure while saving is an internal error (the cause stays in the log),
// and nothing is applied to the pool.
func TestPutSettingsSaveFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, trigger, body string
		wantToken           bool // the token was saved before the failure
	}{
		{"token cannot be saved", metricsTokenKey, `{"slots":4,"metricsToken":"0123456789abcdef-secret"}`, false},
		{"a setting cannot be saved", "slots", `{"slots":4,"metricsToken":"0123456789abcdef-secret"}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, st, dir, logs := serverAt(t)
			execSQL(t, dir, `CREATE TRIGGER no_save BEFORE INSERT ON settings WHEN NEW.key = '`+tc.trigger+`' BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
			r := send(s.Handler(), "PUT", "/api/settings", tc.body)
			checkInternalErr(t, r, logs, http.StatusInternalServerError, "settings_save", "disk full")
			if saved := st.Get(metricsTokenKey, "") != ""; saved != tc.wantToken {
				t.Errorf("token saved = %v, want %v", saved, tc.wantToken)
			}
			select {
			case c := <-s.pool.(*fakePool).configured:
				t.Fatalf("pool configured after a failed save: %+v", c)
			default:
			}
			if entries, total, _ := st.AuditPage(t.Context(), 5, 0); total != 0 {
				t.Errorf("a failed save was audited: %+v", entries)
			}
		})
	}
}

func TestApplyPoolConfigFailureIsLogged(t *testing.T) {
	t.Parallel()
	s, _, logs := newErrServer(t)
	fp := newFakePool()
	fp.configErr = errors.New("docker exploded")
	s.pool = fp
	ch := s.hub.subscribe()
	s.applyPoolConfig(t.Context())
	if !strings.Contains(logs.String(), "docker exploded") {
		t.Errorf("failure not logged:\n%s", logs)
	}
	select { // the UI is told anyway, so it shows what the pool really is
	case <-ch:
	default:
		t.Error("UI not notified after a failed apply")
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/store"
)

// createTarget creates a target through the API as the admin.
func createTarget(t *testing.T, h http.Handler, body string) store.Target {
	t.Helper()
	w := send(h, "POST", "/api/targets", body, session("alice"))
	var tg store.Target
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &tg) != nil || tg.ID == 0 {
		t.Fatalf("create %s: %d %s", body, w.Code, w.Body)
	}
	return tg
}

func targetPath(tg store.Target) string { return "/api/targets/" + strconv.FormatInt(tg.ID, 10) }

func TestCreateTargetAPI(t *testing.T) {
	_, st, _, h := apiServer(t)
	tg := createTarget(t, h, `{"kind":"repo","owner":"acme","repo":"api","scaleSetName":"fast","maxRunners":3,"minRunners":1,"maxJobMinutes":30}`)
	if tg.Owner != "acme" || tg.Repo != "api" || tg.ScaleSetName != "fast" || tg.MaxRunners != 3 || tg.MinRunners != 1 || tg.MaxJobMinutes != 30 || !tg.Enabled {
		t.Fatalf("created %+v", tg)
	}
	if got, err := st.Target(tg.ID); err != nil || got.FullName() != "acme/api" {
		t.Fatalf("stored %+v %v", got, err)
	}
	entries, _, _ := st.AuditPage(t.Context(), 5, 0)
	if len(entries) != 1 || entries[0].Action != "target.create" || entries[0].Actor != "alice" || entries[0].Subject != "acme/api" ||
		!strings.Contains(entries[0].Detail, "label: (none) → fast") {
		t.Fatalf("audit %+v", entries)
	}

	for _, tc := range []struct {
		name, body string
		code       int
		msg        string
	}{
		{"not JSON", `nope`, 400, "invalid JSON body"},
		{"unknown kind", `{"kind":"user","owner":"acme"}`, 400, "invalid target"},
		{"owner with a slash", `{"kind":"org","owner":"a/b"}`, 400, "invalid target"},
		{"repo without name", `{"kind":"repo","owner":"acme"}`, 400, "invalid target"},
		{"bad label", `{"kind":"repo","owner":"acme","repo":"x","scaleSetName":"a b"}`, 400, "invalid runner label"},
		{"same repository twice", `{"kind":"repo","owner":"acme","repo":"api","scaleSetName":"fast"}`, 409, "target_create"},
	} {
		w := send(h, "POST", "/api/targets", tc.body, session("alice"))
		if w.Code != tc.code || !bodyHas(w, tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, w.Code, w.Body, tc.code, tc.msg)
		}
	}
	if targets, _ := st.Targets(); len(targets) != 1 {
		t.Errorf("%d targets stored after refused requests, want 1", len(targets))
	}
	// Only an admin creates targets.
	for _, who := range []string{"oscar", "vera", "olga"} {
		if w := send(h, "POST", "/api/targets", `{"kind":"org","owner":"acme"}`, session(who)); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", who, w.Code)
		}
	}
}

func TestDeleteTargetAPI(t *testing.T) {
	_, st, _, h := apiServer(t)
	tg := createTarget(t, h, `{"kind":"repo","owner":"acme","repo":"api"}`)

	for _, who := range []string{"", "vera", "oscar"} {
		if w := send(h, "DELETE", targetPath(tg), "", cookiesFor(who)...); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%q deleted a target: %d", who, w.Code)
		}
	}
	if _, err := st.Target(tg.ID); err != nil {
		t.Fatal("target deleted by a non-admin")
	}
	for _, id := range []string{"999", "abc", "0"} {
		if w := send(h, "DELETE", "/api/targets/"+id, "", session("alice")); w.Code != http.StatusNotFound {
			t.Errorf("id %q: %d, want 404", id, w.Code)
		}
	}
	w := send(h, "DELETE", targetPath(tg), "", session("alice"))
	if w.Code != http.StatusOK || !bodyHas(w, `"ok":true`) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if _, err := st.Target(tg.ID); err == nil {
		t.Fatal("target still stored")
	}
	entries, _, _ := st.AuditPage(t.Context(), 1, 0)
	if len(entries) != 1 || entries[0].Action != "target.delete" || entries[0].Subject != "acme/api" {
		t.Fatalf("audit %+v", entries)
	}
	if w := send(h, "DELETE", targetPath(tg), "", session("alice")); w.Code != http.StatusNotFound {
		t.Fatalf("second delete: %d", w.Code)
	}
}

// cookiesFor is the session of who, nothing for "".
func cookiesFor(who string) []*http.Cookie {
	if who == "" {
		return nil
	}
	return []*http.Cookie{session(who)}
}

func TestDeleteTargetStoreFailure(t *testing.T) {
	s, st, dir, logs := serverAt(t)
	tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `CREATE TRIGGER keep BEFORE DELETE ON targets BEGIN SELECT RAISE(ABORT, 'read only'); END`)
	w := send(s.Handler(), "DELETE", targetPath(tg), "")
	checkInternalErr(t, w, logs, http.StatusInternalServerError, "target_delete", "read only")
	if _, err := st.Target(tg.ID); err != nil {
		t.Fatal("target gone")
	}
	if _, total, _ := st.AuditPage(t.Context(), 1, 0); total != 0 {
		t.Fatal("a failed delete was audited")
	}
}

func TestUpdateTargetAPI(t *testing.T) {
	_, st, _, h := apiServer(t)
	tg := createTarget(t, h, `{"kind":"repo","owner":"acme","repo":"api","maxRunners":4,"minRunners":2,"scaleSetName":"fast"}`)
	path := targetPath(tg)

	for _, tc := range []struct {
		name, path, body string
		code             int
		msg              string
	}{
		{"unknown target", "/api/targets/999", `{"enabled":false}`, 404, "target not found"},
		{"not a number", "/api/targets/abc", `{"enabled":false}`, 404, "target not found"},
		{"not JSON", path, `{`, 400, "invalid JSON body"},
		{"bad label", path, `{"scaleSetName":"a b"}`, 400, "invalid runner label"},
	} {
		if w := send(h, "PATCH", tc.path, tc.body, session("alice")); w.Code != tc.code || !bodyHas(w, tc.msg) {
			t.Errorf("%s: %d %s, want %d %q", tc.name, w.Code, w.Body, tc.code, tc.msg)
		}
	}
	if got, _ := st.Target(tg.ID); got != tg {
		t.Fatalf("a refused patch changed the target: %+v", got)
	}

	// Numbers are clamped, warm runners follow the new maximum, a new label
	// resets the scale set.
	w := send(h, "PATCH", path, `{"maxRunners":-3,"minRunners":-1,"maxJobMinutes":-5,"scaleSetName":"slow"}`, session("alice"))
	var got store.Target
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != http.StatusOK {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if got.MaxRunners != 1 || got.MinRunners != 0 || got.MaxJobMinutes != 0 || got.ScaleSetName != "slow" || got.ScaleSetID != 0 {
		t.Fatalf("patched %+v", got)
	}
	if stored, _ := st.Target(tg.ID); stored != got {
		t.Fatalf("stored %+v, answered %+v", stored, got)
	}
	// An empty patch is accepted and not audited.
	_, before, _ := st.AuditPage(t.Context(), 1, 0)
	if w := send(h, "PATCH", path, `{}`, session("alice")); w.Code != http.StatusOK {
		t.Fatalf("%d", w.Code)
	}
	if _, after, _ := st.AuditPage(t.Context(), 1, 0); after != before {
		t.Fatal("a patch that changed nothing was audited")
	}
}

func TestUpdateTargetDeniedToViewers(t *testing.T) {
	_, st, _, h := apiServer(t)
	tg := createTarget(t, h, `{"kind":"repo","owner":"acme","repo":"api"}`)
	for _, who := range []string{"", "vera", "olga"} {
		if w := send(h, "PATCH", targetPath(tg), `{"enabled":false}`, cookiesFor(who)...); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%q: %d", who, w.Code)
		}
	}
	if got, _ := st.Target(tg.ID); !got.Enabled {
		t.Fatal("a viewer disabled a target")
	}
}

func TestUpdateTargetStoreFailure(t *testing.T) {
	s, st, dir, logs := serverAt(t)
	tg, err := st.CreateTarget(store.Target{Kind: "repo", Owner: "acme", Repo: "api", Enabled: true, MaxRunners: 1})
	if err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `CREATE TRIGGER keep BEFORE UPDATE ON targets BEGIN SELECT RAISE(ABORT, 'read only'); END`)
	w := send(s.Handler(), "PATCH", targetPath(tg), `{"enabled":false}`)
	checkInternalErr(t, w, logs, http.StatusInternalServerError, "target_save", "read only")
	if _, total, _ := st.AuditPage(t.Context(), 1, 0); total != 0 {
		t.Fatal("a failed update was audited")
	}
}

func TestDuplicateCheckStoreFailure(t *testing.T) {
	s, _, dir, logs := serverAt(t)
	s.strictCheck = func(_ context.Context, _ store.Target) (string, error) { return "", nil }
	dropTable(t, dir, "targets")
	w := send(s.Handler(), "POST", "/api/targets", `{"kind":"repo","owner":"acme","repo":"api"}`)
	checkInternalErr(t, w, logs, http.StatusInternalServerError, "targets_list", "no such table")
}

func TestInstallationsWithoutApp(t *testing.T) {
	_, st, _, h := apiServer(t)
	_ = st.Set("app_client_id", "") // no GitHub App yet
	var out struct {
		Installations []installation `json:"installations"`
		InstallURL    string         `json:"installUrl"`
	}
	w := get(h, "/api/installations", "alice")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || w.Code != http.StatusOK || out.InstallURL != "" || out.Installations == nil || len(out.Installations) != 0 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	_ = st.Set("app_slug", "pitlane-test")
	w = get(h, "/api/installations", "alice")
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out.InstallURL != "https://github.com/apps/pitlane-test/installations/new" {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	for _, who := range []string{"", "oscar"} {
		if w := get(h, "/api/installations", who); w.Code != http.StatusUnauthorized && w.Code != http.StatusForbidden {
			t.Errorf("%q: %d", who, w.Code)
		}
	}
}

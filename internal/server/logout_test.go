package server

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/TuroYT/pitlane/internal/store"
)

// A session that cannot be deleted stays valid: the user is told they are
// signed out, so the failure must at least be in the log.
func TestLogoutFailureIsLogged(t *testing.T) {
	s, st, dir, logs := serverAt(t)
	s.cfg.InsecureNoAuth = false
	addUser(t, st, "alice", store.RoleAdmin)
	if err := st.CreateSession("tok", "alice", time.Hour); err != nil {
		t.Fatal(err)
	}
	execSQL(t, dir, `CREATE TRIGGER keep BEFORE DELETE ON sessions BEGIN SELECT RAISE(ABORT, 'read only'); END`)
	w := do(s.Handler(), "POST", "/api/logout", uiHeader, session("tok"))
	if w.Code != http.StatusOK || cookie(w, sessionCookie).MaxAge >= 0 {
		t.Fatalf("%d", w.Code)
	}
	if !strings.Contains(logs.String(), "read only") {
		t.Errorf("the failed sign-out is not in the log:\n%s", logs)
	}
}

package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TuroYT/pitlane/internal/pool"
)

// stalePool applies a new configuration only later: Config keeps answering
// the old one, as *pool.Pool does until the Configure it was given has run.
type stalePool struct {
	runnerPool
	cfg pool.Config
}

func (p stalePool) Config() pool.Config                          { return p.cfg }
func (p stalePool) Configure(context.Context, pool.Config) error { return nil }
func (p stalePool) StrictSlots() int                             { return 0 }

func TestPutSettingsAnswersWithWhatItSaved(t *testing.T) {
	s, _, _ := newErrServer(t)
	s.pool = stalePool{cfg: pool.Config{Slots: 2, KeepBuildGB: 20}.WithDefaults()}

	body := `{"slots":3,"runnerImage":"","dindImage":"","cacheScope":"shared","keepBuildGb":50,"historyDays":10,"mirror":true}`
	w := httptest.NewRecorder()
	s.handlePutSettings(w, httptest.NewRequest(http.MethodPut, "/api/settings", strings.NewReader(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var got Settings
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("body %q: %v", w.Body, err)
	}
	if got.Slots != 3 || got.KeepBuildGB != 50 || got.CacheScope != "shared" || got.HistoryDays != 10 || !got.Mirror {
		t.Errorf("the response shows %+v, want the saved slots 3, keepBuildGb 50, cacheScope shared, historyDays 10, mirror", got)
	}
	if got.RunnerImage != pool.DefaultRunnerImage {
		t.Errorf("runner image %q, want the default %q for an empty setting", got.RunnerImage, pool.DefaultRunnerImage)
	}
}

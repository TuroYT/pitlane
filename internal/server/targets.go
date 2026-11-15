package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// ------------------------------------------------------------- installations

func (s *Server) handleInstallations(w http.ResponseWriter, r *http.Request) {
	type repo struct {
		Name     string `json:"name"`
		FullName string `json:"fullName"`
		Private  bool   `json:"private"`
	}
	type inst struct {
		ID      int64  `json:"id"`
		Account string `json:"account"`
		Type    string `json:"type"`
		Repos   []repo `json:"repos"`
		Error   string `json:"error,omitempty"`
	}
	out := map[string]any{"installations": []inst{}, "installUrl": ""}
	if slug := s.store.Get("app_slug", ""); slug != "" {
		out["installUrl"] = "https://github.com/apps/" + slug + "/installations/new"
	}
	if !s.appConfigured() {
		writeJSON(w, out)
		return
	}
	pem, _ := s.store.GetSecret("app_pem")
	appID, _ := strconv.ParseInt(s.store.Get("app_id", "0"), 10, 64)
	client, err := gh.NewAppClient(appID, pem)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	insts, err := client.Installations(ctx)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	list := make([]inst, len(insts))
	var wg sync.WaitGroup
	for i, in := range insts {
		list[i] = inst{ID: in.ID, Account: in.Account.Login, Type: in.Account.Type, Repos: []repo{}}
		wg.Add(1)
		go func(i int, id int64) {
			defer wg.Done()
			repos, err := client.Repositories(ctx, id)
			if err != nil {
				list[i].Error = err.Error()
				return
			}
			for _, rp := range repos {
				list[i].Repos = append(list[i].Repos, repo{Name: rp.Name, FullName: rp.FullName, Private: rp.Private})
			}
		}(i, in.ID)
	}
	wg.Wait()
	private := map[string]bool{}
	for _, in := range list {
		for _, rp := range in.Repos {
			private[strings.ToLower(rp.FullName)] = rp.Private
		}
	}
	s.enforcePublicRepos(private)
	out["installations"] = list
	writeJSON(w, out)
}

// enforcePublicRepos applies the visibility of the repositories just listed
// (full name, lower case -> private) to their targets: a public one must run
// strict.
func (s *Server) enforcePublicRepos(private map[string]bool) {
	targets, err := s.store.Targets()
	if err != nil {
		s.log.Error("load targets", "error", err)
		return
	}
	changed := false
	for _, t := range targets {
		p, ok := private[strings.ToLower(t.FullName())]
		if t.Kind != "repo" || !ok {
			continue
		}
		reason := ""
		if !p {
			reason = store.StrictPublicRepo
		}
		if s.mgr.ApplyStrictRequirement(t, reason) {
			changed = true
		}
	}
	if changed {
		s.mgr.Sync()
	}
}

// ------------------------------------------------------------------ targets

var validName = func(s string) bool {
	if s == "" || len(s) > 100 {
		return false
	}
	for _, c := range s {
		if c != '-' && c != '_' && c != '.' && (c < '0' || c > '9') && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// targetOptions are the fields of a creation request whose absence matters.
type targetOptions struct {
	Strict     *bool // nil: decided by the server
	MinRunners *int  // nil: depends on strict mode
}

// newTarget decodes a target creation request. Fields left out get their
// defaults (one warm runner, GitHub's job time limit, one runner per slot);
// applyStrictPolicy then settles strict mode and the warm runners.
func newTarget(body io.Reader, slots int) (store.Target, targetOptions, error) {
	var req struct {
		store.Target
		Strict     *bool `json:"strict"`
		MinRunners *int  `json:"minRunners"`
	}
	req.MaxJobMinutes = store.DefaultMaxJobMinutes
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		return req.Target, targetOptions{}, err
	}
	t, opts := req.Target, targetOptions{Strict: req.Strict, MinRunners: req.MinRunners}
	t.MinRunners = store.DefaultMinRunners
	if req.MinRunners != nil {
		t.MinRunners = *req.MinRunners
	}
	t.Strict, t.StrictRequired, t.StrictForcedAt = false, "", 0 // set by applyStrictPolicy
	if (t.Kind != "repo" && t.Kind != "org") || !validName(t.Owner) || (t.Kind == "repo" && !validName(t.Repo)) {
		return t, opts, errors.New("invalid target")
	}
	if t.Kind == "org" {
		t.Repo = ""
	}
	if t.ScaleSetName != "" && !validName(t.ScaleSetName) {
		return t, opts, errors.New("invalid runner label")
	}
	if t.MaxRunners <= 0 {
		t.MaxRunners = max(1, slots)
	}
	t.MinRunners = min(max(0, t.MinRunners), t.MaxRunners)
	t.Enabled = true
	return t, opts, nil
}

// duplicateTarget returns an enabled target that would compete with t for
// the same jobs: a repository and its organisation both serving the same
// label. GitHub then routes the jobs to one scale set only; the other holds
// slots for nothing.
func duplicateTarget(t store.Target, all []store.Target) (store.Target, bool) {
	if !t.Enabled {
		return store.Target{}, false
	}
	for _, o := range all {
		if o.ID != t.ID && o.Enabled && o.Kind != t.Kind && strings.EqualFold(o.Owner, t.Owner) &&
			strings.EqualFold(scaleSetName(o), scaleSetName(t)) {
			return o, true
		}
	}
	return store.Target{}, false
}

func scaleSetName(t store.Target) string {
	if t.ScaleSetName == "" {
		return "pitlane" // store default
	}
	return t.ScaleSetName
}

// checkDuplicate writes a 409 and returns false when t would duplicate an
// enabled target.
func (s *Server) checkDuplicate(w http.ResponseWriter, t store.Target) bool {
	all, err := s.store.Targets()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return false
	}
	if o, dup := duplicateTarget(t, all); dup {
		repo, org := t, o
		if t.Kind == "org" {
			repo, org = o, t
		}
		writeErr(w, http.StatusConflict, fmt.Sprintf("the repository %s and the organization %s would both serve the label %q: "+
			"GitHub sends each job to only one of them, the other just holds slots. Give one of them another label, or disable one",
			repo.FullName(), org.FullName(), scaleSetName(t)))
		return false
	}
	return true
}

// strictRequiredError rejects turning strict mode off for a target that
// must run strict.
type strictRequiredError struct {
	target, reason string
}

func (e strictRequiredError) Error() string {
	return fmt.Sprintf("%s must run in strict mode (%s): anyone can run a job on its runners, so strict mode cannot be turned off", e.target, e.reason)
}

// applyStrictPolicy decides the strict mode of a new target from what the
// request asked (nil: nothing) and what GitHub said. A target that must run
// strict cannot be created without it; when GitHub cannot be asked, the
// target is strict (fail closed) and the periodic check settles it later.
// Unless the request set them, warm runners follow: none for a strict
// target, one otherwise.
func applyStrictPolicy(t *store.Target, opts targetOptions, reason string, checkErr error) error {
	asked := opts.Strict
	switch {
	case checkErr != nil:
		t.Strict = true
	case reason != "":
		if asked != nil && !*asked {
			return strictRequiredError{t.FullName(), reason}
		}
		t.Strict, t.StrictRequired = true, reason
	default:
		t.Strict = asked != nil && *asked
	}
	if opts.MinRunners == nil {
		t.MinRunners = min(store.DefaultWarmRunners(t.Strict), t.MaxRunners)
	}
	return nil
}

// checkStrict asks GitHub why a target must run strict.
func (s *Server) checkStrict(ctx context.Context, t store.Target) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if s.strictCheck != nil {
		return s.strictCheck(ctx, t)
	}
	return s.StrictReason(ctx, t)
}

// StrictReason asks GitHub why a target must run strict ("" when it need
// not): a public repository runs jobs of pull requests from forks. An
// organisation target serves every repository its runner group allows: the
// group's "Allow public repositories" setting (off by default) decides
// whether public repositories, hence forks, can use the runners at all.
func (s *Server) StrictReason(ctx context.Context, t store.Target) (string, error) {
	tok, err := s.token(ctx, t)
	if err != nil {
		return "", err
	}
	if t.Kind == "org" {
		public, err := gh.RunnerGroupAllowsPublic(ctx, tok, t.Owner, scaler.RunnerGroupID)
		if err != nil {
			return "", err
		}
		if public {
			return store.StrictPublicRunnerGroup, nil
		}
		return "", nil
	}
	private, err := gh.RepoPrivate(ctx, tok, t.Owner, t.Repo)
	if err != nil {
		return "", err
	}
	if !private {
		return store.StrictPublicRepo, nil
	}
	return "", nil
}

func (s *Server) handleCreateTarget(w http.ResponseWriter, r *http.Request) {
	t, opts, err := newTarget(r.Body, s.pool.Config().Slots)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	reason, checkErr := s.checkStrict(r.Context(), t)
	if checkErr != nil {
		s.log.Warn("cannot check whether the new target must run strict: enabling strict mode", "target", t.FullName(), "error", checkErr)
	}
	if err := applyStrictPolicy(&t, opts, reason, checkErr); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if !s.checkDuplicate(w, t) {
		return
	}
	created, err := s.store.CreateTarget(t)
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	s.audit(r, "target.create", created.FullName(), changes(targetKeys, nil, targetValues(created)))
	s.mgr.Sync()
	writeJSON(w, created)
}

func (s *Server) targetFromPath(w http.ResponseWriter, r *http.Request) (store.Target, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.store.Target(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "target not found")
		return t, false
	}
	return t, true
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.targetFromPath(w, r)
	if !ok {
		return
	}
	var body struct {
		Enabled      *bool   `json:"enabled"`
		MinRunners   *int    `json:"minRunners"`
		MaxRunners   *int    `json:"maxRunners"`
		ScaleSetName *string `json:"scaleSetName"`
		Strict       *bool   `json:"strict"`
		MaxJobMins   *int    `json:"maxJobMinutes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if p, _ := s.currentUser(r); !p.can(store.RoleAdmin) &&
		(body.MinRunners != nil || body.MaxRunners != nil || body.ScaleSetName != nil || body.Strict != nil || body.MaxJobMins != nil) {
		writeErr(w, http.StatusForbidden, "operators can only enable or disable a target")
		return
	}
	before := targetValues(t)
	wasEnabled, oldLabel := t.Enabled, t.ScaleSetName
	if body.Enabled != nil {
		t.Enabled = *body.Enabled
	}
	if body.MinRunners != nil {
		t.MinRunners = max(0, *body.MinRunners)
	}
	if body.Strict != nil && !*body.Strict && t.Strict {
		// Only once GitHub confirms the target need not run strict.
		reason, err := s.checkStrict(r.Context(), t)
		if err != nil {
			writeErr(w, http.StatusBadGateway, "cannot check on GitHub whether "+t.FullName()+" must run strict, strict mode stays on: "+err.Error())
			return
		}
		if s.mgr.ApplyStrictRequirement(t, reason) {
			defer s.mgr.Sync()
		}
		if reason != "" {
			writeErr(w, http.StatusBadRequest, strictRequiredError{t.FullName(), reason}.Error())
			return
		}
	}
	if body.Strict != nil {
		t.Strict = *body.Strict
	}
	if body.MaxJobMins != nil {
		t.MaxJobMinutes = max(0, *body.MaxJobMins)
	}
	if body.MaxRunners != nil {
		t.MaxRunners = max(1, *body.MaxRunners)
	}
	if body.ScaleSetName != nil && *body.ScaleSetName != t.ScaleSetName {
		if !validName(*body.ScaleSetName) {
			writeErr(w, http.StatusBadRequest, "invalid runner label")
			return
		}
		t.ScaleSetName, t.ScaleSetID = *body.ScaleSetName, 0
	}
	t.MinRunners = min(t.MinRunners, t.MaxRunners)
	if t.Enabled && (!wasEnabled || !strings.EqualFold(t.ScaleSetName, oldLabel)) && !s.checkDuplicate(w, t) {
		return
	}
	if err := s.store.UpdateTarget(t); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if detail := changes(targetKeys, before, targetValues(t)); detail != "" {
		s.audit(r, "target.update", t.FullName(), detail)
	}
	s.mgr.Sync()
	writeJSON(w, t)
}

func (s *Server) handleDeleteTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.targetFromPath(w, r)
	if !ok {
		return
	}
	if err := s.store.DeleteTarget(t.ID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "target.delete", t.FullName(), "")
	s.mgr.Sync()
	writeJSON(w, map[string]bool{"ok": true})
}

// targetKeys are the target fields the audit log reports, in order.
var targetKeys = []string{"enabled", "label", "warm runners", "max parallel", "strict", "job limit (min)"}

func targetValues(t store.Target) map[string]string {
	return map[string]string{
		"enabled": strconv.FormatBool(t.Enabled), "label": scaleSetName(t), "warm runners": strconv.Itoa(t.MinRunners),
		"max parallel": strconv.Itoa(t.MaxRunners), "strict": strconv.FormatBool(t.Strict), "job limit (min)": strconv.Itoa(t.MaxJobMinutes),
	}
}

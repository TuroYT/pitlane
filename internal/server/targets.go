package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TuroYT/pitlane/internal/gh"
	"github.com/TuroYT/pitlane/internal/scaler"
	"github.com/TuroYT/pitlane/internal/store"
)

// installationsTimeout bounds the GitHub calls listing the App's
// installations and their repositories.
const installationsTimeout = 30 * time.Second

// installation is a GitHub App installation and the repositories it grants.
type installation struct {
	ID      int64              `json:"id"`
	Account string             `json:"account"`
	Type    string             `json:"type"`
	Repos   []installationRepo `json:"repos"`
	Error   string             `json:"error,omitempty"`
}

type installationRepo struct {
	Name     string `json:"name"`
	FullName string `json:"fullName"`
	Private  bool   `json:"private"`
}

// installationLister is the part of *gh.AppClient that lists installations.
type installationLister interface {
	Installations(ctx context.Context) ([]gh.Installation, error)
	Repositories(ctx context.Context, installationID int64) ([]gh.Repo, error)
}

// handleInstallations lists the installations of the GitHub App, with the
// link to install it, and switches the targets of public repositories to
// strict mode on the way.
func (s *Server) handleInstallations(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"installations": []installation{}, "installUrl": ""}
	if slug := s.store.Get("app_slug", ""); slug != "" {
		out["installUrl"] = "https://github.com/apps/" + slug + "/installations/new"
	}
	if !s.appConfigured() {
		writeJSON(w, out)
		return
	}
	client, err := s.appClient()
	if err != nil {
		msg := "cannot load the GitHub App"
		if errors.Is(err, errAppKey) {
			msg = errAppKey.Error()
		}
		s.internalErr(w, r, http.StatusInternalServerError, "github_app", msg, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), installationsTimeout)
	defer cancel()
	list, err := listInstallations(ctx, client, s.log)
	if err != nil {
		s.internalErr(w, r, http.StatusBadGateway, "github_installations", "cannot list the GitHub App installations", err)
		return
	}
	s.enforcePublicRepos(repoPrivacy(list))
	out["installations"] = list
	writeJSON(w, out)
}

// listInstallations lists the App's installations and, in parallel, the
// repositories of each. An installation whose repositories GitHub cannot
// list gets an Error instead (the details go to log).
func listInstallations(ctx context.Context, c installationLister, log *slog.Logger) ([]installation, error) {
	insts, err := c.Installations(ctx)
	if err != nil {
		return nil, fmt.Errorf("list installations: %w", err)
	}
	list := make([]installation, len(insts))
	var wg sync.WaitGroup
	for i, in := range insts {
		list[i] = installation{ID: in.ID, Account: in.Account.Login, Type: in.Account.Type, Repos: []installationRepo{}}
		wg.Go(func() {
			repos, err := c.Repositories(ctx, in.ID)
			if err != nil {
				log.Error("list installation repositories", "installation", in.ID, "error", err)
				list[i].Error = "cannot list the repositories of this installation on GitHub (details in the server log)"
				return
			}
			for _, rp := range repos {
				list[i].Repos = append(list[i].Repos, installationRepo{Name: rp.Name, FullName: rp.FullName, Private: rp.Private})
			}
		})
	}
	wg.Wait()
	return list, nil
}

// repoPrivacy maps the full name, in lower case, of every repository listed
// to whether it is private.
func repoPrivacy(list []installation) map[string]bool {
	private := map[string]bool{}
	for _, in := range list {
		for _, rp := range in.Repos {
			private[strings.ToLower(rp.FullName)] = rp.Private
		}
	}
	return private
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

// validName accepts a repository, owner, runner or label name: letters,
// digits, '-', '_' and '.', at most 100 characters.
func validName(s string) bool {
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
		return req.Target, targetOptions{}, errors.New("invalid JSON body")
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

// scaleSetName is the runner label of t, with the store's default.
func scaleSetName(t store.Target) string {
	if t.ScaleSetName == "" {
		return "pitlane" // store default
	}
	return t.ScaleSetName
}

// checkDuplicate writes a 409 and returns false when t would duplicate an
// enabled target.
func (s *Server) checkDuplicate(w http.ResponseWriter, r *http.Request, t store.Target) bool {
	all, err := s.store.Targets()
	if err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "targets_list", "cannot list the targets", err)
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

// strictCheckTimeout bounds the GitHub calls deciding whether a target must
// run strict.
const strictCheckTimeout = 30 * time.Second

// checkStrict asks GitHub why a target must run strict.
func (s *Server) checkStrict(ctx context.Context, t store.Target) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, strictCheckTimeout)
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
		writeErr(w, http.StatusBadRequest, err.Error()) // validation messages only, see newTarget
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
	if !s.checkDuplicate(w, r, t) {
		return
	}
	created, err := s.store.CreateTarget(t)
	if err != nil {
		s.internalErr(w, r, http.StatusConflict, "target_create", "cannot create the target, it may already exist", err)
		return
	}
	s.audit(r, "target.create", created.FullName(), changes(targetKeys, nil, targetValues(created)))
	s.mgr.Sync()
	writeJSON(w, created)
}

// targetFromPath returns the target named by the {id} path value, or answers
// 404 and returns false.
func (s *Server) targetFromPath(w http.ResponseWriter, r *http.Request) (store.Target, bool) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	t, err := s.store.Target(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "target not found")
		return t, false
	}
	return t, true
}

// targetPatch is the body of PATCH /api/targets/{id}: a field left out
// keeps its value.
type targetPatch struct {
	Enabled      *bool   `json:"enabled"`
	MinRunners   *int    `json:"minRunners"`
	MaxRunners   *int    `json:"maxRunners"`
	ScaleSetName *string `json:"scaleSetName"`
	Strict       *bool   `json:"strict"`
	MaxJobMins   *int    `json:"maxJobMinutes"`
}

// adminOnly reports whether the patch changes more than whether the target
// is enabled, which operators may not do.
func (p targetPatch) adminOnly() bool {
	return p.MinRunners != nil || p.MaxRunners != nil || p.ScaleSetName != nil || p.Strict != nil || p.MaxJobMins != nil
}

// turnsStrictOff reports whether the patch turns strict mode off for t.
func (p targetPatch) turnsStrictOff(t store.Target) bool {
	return p.Strict != nil && !*p.Strict && t.Strict
}

// apply changes t as the patch asks, clamping numbers to their valid range.
// It fails, leaving t partly changed, on an invalid runner label.
func (p targetPatch) apply(t *store.Target) error {
	if p.Enabled != nil {
		t.Enabled = *p.Enabled
	}
	if p.MinRunners != nil {
		t.MinRunners = max(0, *p.MinRunners)
	}
	if p.Strict != nil {
		t.Strict = *p.Strict
	}
	if p.MaxJobMins != nil {
		t.MaxJobMinutes = max(0, *p.MaxJobMins)
	}
	if p.MaxRunners != nil {
		t.MaxRunners = max(1, *p.MaxRunners)
	}
	if p.ScaleSetName != nil && *p.ScaleSetName != t.ScaleSetName {
		if !validName(*p.ScaleSetName) {
			return errors.New("invalid runner label")
		}
		t.ScaleSetName, t.ScaleSetID = *p.ScaleSetName, 0
	}
	t.MinRunners = min(t.MinRunners, t.MaxRunners)
	return nil
}

func (s *Server) handleUpdateTarget(w http.ResponseWriter, r *http.Request) {
	t, ok := s.targetFromPath(w, r)
	if !ok {
		return
	}
	var patch targetPatch
	if !readJSON(w, r.Body, &patch) {
		return
	}
	if p, _ := s.currentUser(r); !p.can(store.RoleAdmin) && patch.adminOnly() {
		writeErr(w, http.StatusForbidden, "operators can only enable or disable a target")
		return
	}
	if patch.turnsStrictOff(t) {
		// Only once GitHub confirms the target need not run strict.
		reason, err := s.checkStrict(r.Context(), t)
		if err != nil {
			s.internalErr(w, r, http.StatusBadGateway, "github_strict_check", "cannot check on GitHub whether "+t.FullName()+" must run strict, strict mode stays on", err)
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
	before := targetValues(t)
	wasEnabled, oldLabel := t.Enabled, t.ScaleSetName
	if err := patch.apply(&t); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if t.Enabled && (!wasEnabled || !strings.EqualFold(t.ScaleSetName, oldLabel)) && !s.checkDuplicate(w, r, t) {
		return
	}
	if err := s.store.UpdateTarget(t); err != nil {
		s.internalErr(w, r, http.StatusInternalServerError, "target_save", "cannot save the target", err)
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
		s.internalErr(w, r, http.StatusInternalServerError, "target_delete", "cannot delete the target", err)
		return
	}
	s.audit(r, "target.delete", t.FullName(), "")
	s.mgr.Sync()
	writeJSON(w, map[string]bool{"ok": true})
}

// targetKeys are the target fields the audit log reports, in order.
var targetKeys = []string{"enabled", "label", "warm runners", "max parallel", "strict", "job limit (min)"}

// targetValues are the audited fields of t, keyed by targetKeys.
func targetValues(t store.Target) map[string]string {
	return map[string]string{
		"enabled": strconv.FormatBool(t.Enabled), "label": scaleSetName(t), "warm runners": strconv.Itoa(t.MinRunners),
		"max parallel": strconv.Itoa(t.MaxRunners), "strict": strconv.FormatBool(t.Strict), "job limit (min)": strconv.Itoa(t.MaxJobMinutes),
	}
}

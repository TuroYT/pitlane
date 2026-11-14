package update

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"time"
)

// Settings keys (in the settings table).
const (
	keyAuto        = "update_auto"
	keyWindowStart = "update_window_start"
	keyWindowEnd   = "update_window_end"
	keyAutoPull    = "runner_autopull"
	keyPulledAt    = "runner_pulled_at"
	keyLastUpdate  = "update_last"
)

// CheckEvery is how often releases are fetched from GitHub.
const CheckEvery = 6 * time.Hour

// PullEvery is how often the runner image is pulled when auto-pull is on.
const PullEvery = 24 * time.Hour

// Store is the settings table.
type Store interface {
	Get(key, def string) string
	Set(key, value string) error
}

// Settings are the update preferences.
type Settings struct {
	AutoUpdate bool   `json:"autoUpdate"`
	Window     Window `json:"window"`
	AutoPull   bool   `json:"autoPull"`
}

// PitlaneState is what the UI shows about Pitlane itself.
type PitlaneState struct {
	Current         string     `json:"current"`
	Latest          string     `json:"latest,omitempty"`
	LatestURL       string     `json:"latestUrl,omitempty"`
	LatestPublished int64      `json:"latestPublished,omitempty"`
	UpdateAvailable bool       `json:"updateAvailable"`
	AutoCandidate   string     `json:"autoCandidate,omitempty"`
	AutoStatus      string     `json:"autoStatus,omitempty"`
	Capability      Capability `json:"capability"`
	ManualCommand   string     `json:"manualCommand,omitempty"`
	CheckedAt       int64      `json:"checkedAt,omitempty"`
	CheckError      string     `json:"checkError,omitempty"`
	Phase           string     `json:"phase,omitempty"` // non-empty while an update runs
	ApplyError      string     `json:"applyError,omitempty"`
	LastUpdate      string     `json:"lastUpdate,omitempty"`
}

// RunnerImageState is what the UI shows about the runner image.
type RunnerImageState struct {
	Image      string `json:"image"`
	ImageID    string `json:"imageId,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Version    string `json:"version,omitempty"`
	VersionErr string `json:"versionError,omitempty"`
	Latest     string `json:"latest,omitempty"`
	LatestURL  string `json:"latestUrl,omitempty"`
	Status     string `json:"status"`
	Deadline   int64  `json:"deadline,omitempty"`
	PulledAt   int64  `json:"pulledAt,omitempty"`
	Pulling    bool   `json:"pulling"`
	PullError  string `json:"pullError,omitempty"`
	CheckError string `json:"checkError,omitempty"`
}

// State is the GET /api/updates response.
type State struct {
	Pitlane  PitlaneState     `json:"pitlane"`
	Runner   RunnerImageState `json:"runner"`
	Settings Settings         `json:"settings"`
}

// Updater checks for updates and applies them.
type Updater struct {
	Version     string
	Store       Store
	GitHub      *GitHub
	Images      Images
	RunnerImage func() string // image new runners use
	Busy        func() bool   // a job is running
	Restart     func()        // graceful restart into the new binary
	Notify      func()        // UI state changed
	Probe       Probe
	Verify      Verifier
	Log         *slog.Logger
	Now         func() time.Time

	mu         sync.Mutex
	cap        Capability
	releases   []Release
	runnerRels []Release
	checkedAt  time.Time
	checkErr   string
	runnerErr  string
	phase      string
	applyErr   string
	autoStatus string
	failed     map[string]time.Time // auto-update failures per version
	img        ImageInfo
	imgVersion map[string]string // image ID → runner version
	versionErr string
	pulling    bool
	pullErr    string
	lastPull   time.Time // last automatic pull attempt
}

func (u *Updater) now() time.Time {
	if u.Now != nil {
		return u.Now()
	}
	return time.Now()
}

func (u *Updater) log() *slog.Logger {
	if u.Log != nil {
		return u.Log
	}
	return slog.Default()
}

func (u *Updater) notify() {
	if u.Notify != nil {
		u.Notify()
	}
}

// LoadSettings reads the preferences (auto-update off, window 03-05, auto-pull on).
func (u *Updater) LoadSettings() Settings {
	start, _ := strconv.Atoi(u.Store.Get(keyWindowStart, "3"))
	end, _ := strconv.Atoi(u.Store.Get(keyWindowEnd, "5"))
	return Settings{
		AutoUpdate: u.Store.Get(keyAuto, "false") == "true",
		Window:     Window{Start: start, End: end},
		AutoPull:   u.Store.Get(keyAutoPull, "true") == "true",
	}
}

// SaveSettings validates and stores the preferences.
func (u *Updater) SaveSettings(s Settings) error {
	if !s.Window.Valid() {
		return errors.New("the update window hours must be between 0 and 23")
	}
	for k, v := range map[string]string{
		keyAuto: strconv.FormatBool(s.AutoUpdate), keyAutoPull: strconv.FormatBool(s.AutoPull),
		keyWindowStart: strconv.Itoa(s.Window.Start), keyWindowEnd: strconv.Itoa(s.Window.End),
	} {
		if err := u.Store.Set(k, v); err != nil {
			return err
		}
	}
	u.log().Info("update settings saved", "autoUpdate", s.AutoUpdate, "window", s.Window, "autoPull", s.AutoPull)
	return nil
}

func (u *Updater) capability() Capability {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.cap.Mode == "" {
		u.cap = Detect(u.Probe)
	}
	return u.cap
}

// State returns a snapshot for the UI.
func (u *Updater) State() State {
	c := u.capability()
	set := u.LoadSettings()
	u.mu.Lock()
	defer u.mu.Unlock()
	ps := PitlaneState{
		Current: u.Version, Capability: c, CheckError: u.checkErr, Phase: u.phase, ApplyError: u.applyErr,
		AutoStatus: u.autoStatus, LastUpdate: u.Store.Get(keyLastUpdate, ""),
	}
	if !u.checkedAt.IsZero() {
		ps.CheckedAt = u.checkedAt.Unix()
	}
	if r, v, ok := LatestStable(u.releases); ok {
		ps.Latest, ps.LatestURL, ps.LatestPublished = r.Tag, r.HTMLURL, r.PublishedAt.Unix()
		if cur, ok := ReleaseVersion(u.Version); ok {
			ps.UpdateAvailable = Compare(v, cur) > 0
		}
	}
	if r, ok := AutoCandidate(u.releases, u.Version); ok {
		ps.AutoCandidate = r.Tag
	}
	if !c.CanSelfUpdate {
		ps.ManualCommand = ManualCommand(c.Mode, ps.Latest)
	}

	rs := RunnerImageState{
		Image: u.runnerImage(), ImageID: u.img.ID, Digest: u.img.Digest, Version: u.imgVersion[u.img.ID],
		VersionErr: u.versionErr, Pulling: u.pulling, PullError: u.pullErr, CheckError: u.runnerErr,
	}
	if at, err := strconv.ParseInt(u.Store.Get(keyPulledAt, "0"), 10, 64); err == nil && at > 0 {
		rs.PulledAt = at
	}
	if r, _, ok := LatestStable(u.runnerRels); ok {
		rs.Latest, rs.LatestURL = r.Tag, r.HTMLURL
	}
	var deadline time.Time
	rs.Status, deadline = RunnerStatus(rs.Version, u.runnerRels, u.now())
	if !deadline.IsZero() {
		rs.Deadline = deadline.Unix()
	}
	return State{Pitlane: ps, Runner: rs, Settings: set}
}

func (u *Updater) runnerImage() string {
	if u.RunnerImage == nil {
		return ""
	}
	return u.RunnerImage()
}

// Check fetches the Pitlane and runner releases and inspects the runner image.
func (u *Updater) Check(ctx context.Context) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rels, err := u.GitHub.Releases(ctx, Repo, 30)
	runnerRels, rerr := u.GitHub.Releases(ctx, RunnerRepo, 30)
	u.mu.Lock()
	u.checkedAt = u.now()
	u.checkErr, u.runnerErr = "", ""
	if err != nil {
		u.checkErr = err.Error()
	} else {
		u.releases = rels
	}
	if rerr != nil {
		u.runnerErr = rerr.Error()
	} else {
		u.runnerRels = runnerRels
	}
	u.mu.Unlock()
	if err != nil {
		u.log().Warn("update check: Pitlane releases", "error", err)
	}
	if rerr != nil {
		u.log().Warn("update check: runner releases", "error", rerr)
	}
	st := u.State()
	u.log().Info("update check done", "current", u.Version, "latest", st.Pitlane.Latest, "runnerLatest", st.Runner.Latest)
	u.InspectImage(ctx)
}

// InspectImage refreshes the runner image ID, digest and runner version.
func (u *Updater) InspectImage(ctx context.Context) {
	if u.Images == nil {
		return
	}
	ref := u.runnerImage()
	info, err := u.Images.Inspect(ctx, ref)
	if err != nil {
		u.mu.Lock()
		u.img, u.versionErr = ImageInfo{}, "inspect "+ref+": "+err.Error()
		u.mu.Unlock()
		u.notify()
		return
	}
	u.mu.Lock()
	u.img = info
	_, known := u.imgVersion[info.ID]
	u.mu.Unlock()
	if !known {
		v, err := u.Images.RunnerVersion(ctx, ref)
		u.mu.Lock()
		u.versionErr = ""
		if err != nil {
			u.versionErr = err.Error()
			u.log().Warn("cannot read the runner version of the image", "image", ref, "error", err)
		} else {
			if u.imgVersion == nil {
				u.imgVersion = map[string]string{}
			}
			u.imgVersion[info.ID] = v
		}
		u.mu.Unlock()
	}
	u.notify()
}

// PullRunner pulls the configured runner image tag. Runners already started
// keep their image; new runners use the pulled one.
func (u *Updater) PullRunner(ctx context.Context) error {
	if u.Images == nil {
		return errors.New("docker is not available")
	}
	u.mu.Lock()
	if u.pulling {
		u.mu.Unlock()
		return errors.New("a pull is already running")
	}
	u.pulling, u.pullErr = true, ""
	u.mu.Unlock()
	u.notify()
	ref := u.runnerImage()
	before, _ := u.Images.Inspect(ctx, ref)
	u.log().Info("pulling runner image", "image", ref)
	err := u.Images.Pull(ctx, ref)
	u.mu.Lock()
	u.pulling = false
	if err != nil {
		u.pullErr = err.Error()
	}
	u.mu.Unlock()
	if err != nil {
		u.log().Error("runner image pull failed", "image", ref, "error", err)
		u.notify()
		return err
	}
	_ = u.Store.Set(keyPulledAt, strconv.FormatInt(u.now().Unix(), 10))
	u.InspectImage(ctx)
	after := u.State().Runner
	if after.ImageID != before.ID {
		u.log().Info("runner image updated: new runners use it, running ones keep theirs", "image", ref, "digest", after.Digest, "runner", after.Version)
	} else {
		u.log().Info("runner image already up to date", "image", ref, "digest", after.Digest)
	}
	return nil
}

// Apply installs the latest stable release and restarts. It runs in the
// background; progress and errors show in State.
func (u *Updater) Apply(auto bool) error {
	c := u.capability()
	if !c.CanSelfUpdate {
		return errors.New(c.Reason + ": run " + ManualCommand(c.Mode, u.State().Pitlane.Latest))
	}
	u.mu.Lock()
	if u.phase != "" {
		u.mu.Unlock()
		return errors.New("an update is already running")
	}
	var rel Release
	var want Version
	var ok bool
	if auto {
		rel, ok = AutoCandidate(u.releases, u.Version)
		if ok {
			want, _ = ParseVersion(rel.Tag)
		}
	} else {
		rel, want, ok = LatestStable(u.releases)
	}
	if !ok {
		u.mu.Unlock()
		return errors.New("no release to install: check for updates first")
	}
	if cur, ok := ReleaseVersion(u.Version); ok && Compare(want, cur) <= 0 {
		u.mu.Unlock()
		return errors.New("already up to date")
	}
	u.phase, u.applyErr = "starting", ""
	u.mu.Unlock()
	u.log().Info("update: installing", "from", u.Version, "to", rel.Tag, "automatic", auto)
	u.notify()
	go u.install(rel, want, c, auto)
	return nil
}

func (u *Updater) install(rel Release, want Version, c Capability, auto bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	in := &Installer{GitHub: u.GitHub, Executable: c.Executable, Arch: u.Probe.GOARCH, Verify: u.Verify, Log: u.log()}
	if in.Arch == "" {
		in.Arch = runtime.GOARCH
	}
	err := in.Install(ctx, rel, want, func(p string) {
		u.mu.Lock()
		u.phase = p
		u.mu.Unlock()
		u.notify()
	})
	if err != nil {
		u.log().Error("update failed: nothing was replaced", "to", rel.Tag, "error", err)
		u.mu.Lock()
		u.phase, u.applyErr = "", err.Error()
		if auto {
			if u.failed == nil {
				u.failed = map[string]time.Time{}
			}
			u.failed[rel.Tag] = u.now()
		}
		u.mu.Unlock()
		u.notify()
		return
	}
	_ = u.Store.Set(keyLastUpdate, u.Version+" → "+want.String()+" on "+u.now().Format(time.RFC3339))
	u.mu.Lock()
	u.phase = "restarting"
	u.mu.Unlock()
	u.notify()
	u.log().Info("update: restarting into the new version (busy runners keep running and are adopted)", "version", want.String())
	time.Sleep(time.Second) // let the UI see "restarting"
	if u.Restart != nil {
		u.Restart()
	}
}

// tick runs the periodic work: release checks, auto-pull, auto-update.
func (u *Updater) tick(ctx context.Context) {
	u.mu.Lock()
	due := u.checkedAt.IsZero() || u.now().Sub(u.checkedAt) >= CheckEvery
	u.mu.Unlock()
	if due {
		u.Check(ctx)
	}
	set := u.LoadSettings()
	if set.AutoPull {
		at, _ := strconv.ParseInt(u.Store.Get(keyPulledAt, "0"), 10, 64)
		u.mu.Lock()
		retry := u.lastPull.IsZero() || u.now().Sub(u.lastPull) >= time.Hour
		u.mu.Unlock()
		if u.now().Sub(time.Unix(at, 0)) >= PullEvery && retry {
			u.mu.Lock()
			u.lastPull = u.now()
			u.mu.Unlock()
			_ = u.PullRunner(ctx)
		}
	}
	u.autoUpdate(set)
}

func (u *Updater) autoUpdate(set Settings) {
	c := u.capability()
	u.mu.Lock()
	cand, _ := AutoCandidate(u.releases, u.Version)
	failedAt, failed := u.failed[cand.Tag]
	busy := u.phase != ""
	u.mu.Unlock()
	in := AutoInput{Enabled: set.AutoUpdate, Window: set.Window, Now: u.now(), Current: u.Version, Candidate: cand, CanSelf: c.CanSelfUpdate}
	if in.Enabled && in.Candidate.Tag != "" && u.Busy != nil {
		in.Busy = u.Busy()
	}
	ok, why := AutoDecision(in)
	if ok && failed && u.now().Sub(failedAt) < 24*time.Hour {
		ok, why = false, "the last automatic attempt failed, retrying tomorrow"
	}
	u.mu.Lock()
	u.autoStatus = why
	u.mu.Unlock()
	if ok && !busy {
		if err := u.Apply(true); err != nil {
			u.log().Warn("automatic update not started", "error", err)
		}
	}
}

// Run does the periodic work until ctx ends.
func (u *Updater) Run(ctx context.Context) {
	u.log().Info("update checks enabled", "version", u.Version, "mode", u.capability().Mode, "selfUpdate", u.capability().CanSelfUpdate)
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second): // let the slots start first
	}
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		u.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---------------------------------------------------------------- HTTP

// Handler serves /api/updates (the caller enforces authentication; the
// server's CSRF check covers the mutations).
func (u *Updater) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/updates", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, http.StatusOK, u.State()) })
	mux.HandleFunc("POST /api/updates/check", func(w http.ResponseWriter, r *http.Request) {
		u.Check(r.Context())
		writeJSON(w, http.StatusOK, u.State())
	})
	mux.HandleFunc("POST /api/updates/apply", func(w http.ResponseWriter, r *http.Request) {
		if err := u.Apply(false); err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, u.State())
	})
	mux.HandleFunc("POST /api/updates/pull-runner", func(w http.ResponseWriter, r *http.Request) {
		u.mu.Lock()
		running := u.pulling
		u.mu.Unlock()
		if running {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "a pull is already running"})
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
			defer cancel()
			_ = u.PullRunner(ctx)
		}()
		writeJSON(w, http.StatusAccepted, u.State())
	})
	mux.HandleFunc("PUT /api/updates/settings", func(w http.ResponseWriter, r *http.Request) {
		var s Settings
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if err := u.SaveSettings(s); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, u.State())
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

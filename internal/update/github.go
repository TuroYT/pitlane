package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"time"
)

// Release is the subset of a GitHub release Pitlane uses.
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// Asset is a file attached to a release.
type Asset struct {
	Name string `json:"name"`
	URL  string `json:"browser_download_url"`
	Size int64  `json:"size"`
}

// Asset returns the asset with that name.
func (r Release) Asset(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// ErrNoRelease means GitHub returned 404: the repository is private, does not
// exist, or has no published release yet.
var ErrNoRelease = errors.New("no release found (the repository is private, unreachable or has no published release yet)")

// DefaultHosts are the only hosts Pitlane talks to for updates: the API, and
// the release download host with the storage hosts it redirects to.
var DefaultHosts = []string{"api.github.com", "github.com", "objects.githubusercontent.com", "release-assets.githubusercontent.com"}

// GitHub is a minimal, unauthenticated client for public release data.
// Every request and every redirect hop must be HTTPS to an allowed host.
type GitHub struct {
	HTTP    *http.Client // nil: a client with a 60 s timeout
	APIBase string       // "" = https://api.github.com
	Hosts   []string     // nil = DefaultHosts
}

func (g *GitHub) hosts() []string {
	if g.Hosts != nil {
		return g.Hosts
	}
	return DefaultHosts
}

func (g *GitHub) checkURL(u *url.URL) error {
	if u.Scheme != "https" {
		return fmt.Errorf("refusing non-HTTPS URL %s", u.Redacted())
	}
	if !slices.Contains(g.hosts(), u.Host) {
		return fmt.Errorf("refusing download from unexpected host %s", u.Host)
	}
	return nil
}

func (g *GitHub) client() *http.Client {
	base := g.HTTP
	if base == nil {
		base = &http.Client{Timeout: 60 * time.Second}
	}
	c := *base
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return g.checkURL(req.URL)
	}
	return &c
}

func (g *GitHub) get(ctx context.Context, rawURL string) (*http.Response, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	if err := g.checkURL(u); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "pitlane-updater")
	req.Header.Set("Accept", "application/vnd.github+json")
	return g.client().Do(req)
}

// Releases lists the most recent releases of owner/repo (drafts excluded).
func (g *GitHub) Releases(ctx context.Context, repo string, n int) ([]Release, error) {
	base := g.APIBase
	if base == "" {
		base = "https://api.github.com"
	}
	resp, err := g.get(ctx, fmt.Sprintf("%s/repos/%s/releases?per_page=%d", base, repo, n))
	if err != nil {
		return nil, fmt.Errorf("GitHub releases of %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, fmt.Errorf("%s: %w", repo, ErrNoRelease)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests:
		return nil, fmt.Errorf("GitHub releases of %s: rate limited or forbidden (HTTP %d), retry later", repo, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub releases of %s: HTTP %d", repo, resp.StatusCode)
	}
	var all []Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&all); err != nil {
		return nil, fmt.Errorf("GitHub releases of %s: %w", repo, err)
	}
	out := all[:0]
	for _, r := range all {
		if !r.Draft {
			out = append(out, r)
		}
	}
	return out, nil
}

// Download streams rawURL into w, failing when the body exceeds limit bytes.
func (g *GitHub) Download(ctx context.Context, rawURL string, limit int64, w io.Writer) (int64, error) {
	resp, err := g.get(ctx, rawURL)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("download %s: HTTP %d", rawURL, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return 0, fmt.Errorf("download %s: %d bytes is over the %d byte limit", rawURL, resp.ContentLength, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return n, fmt.Errorf("download %s: %w", rawURL, err)
	}
	if n > limit {
		return n, fmt.Errorf("download %s: over the %d byte limit", rawURL, limit)
	}
	return n, nil
}

// latest returns the highest release accepted by keep.
func latest(rels []Release, keep func(Release, Version) bool) (Release, Version, bool) {
	var best Release
	var bv Version
	found := false
	for _, r := range rels {
		v, err := ParseVersion(r.Tag)
		if err != nil || r.Draft {
			continue
		}
		if keep != nil && !keep(r, v) {
			continue
		}
		if !found || Compare(v, bv) > 0 {
			best, bv, found = r, v, true
		}
	}
	return best, bv, found
}

// LatestStable is the highest non-pre-release release.
func LatestStable(rels []Release) (Release, Version, bool) {
	return latest(rels, func(r Release, v Version) bool { return !r.Prerelease && !v.IsPrerelease() })
}

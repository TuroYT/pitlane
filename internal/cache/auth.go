package cache

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Identity is what a runner token grants: the cache of one GitHub owner,
// restricted to one repository when Repo is set (repository targets).
// Both come from Pitlane's own state, never from the job.
type Identity struct {
	Owner string
	Repo  string
}

// loadKey reads the token signing key, creating it on first use. It lives in
// Pitlane's data directory, which jobs cannot reach.
func loadKey(dir string) ([]byte, error) {
	file := filepath.Join(dir, "token.key")
	b, err := os.ReadFile(file)
	if err == nil && len(b) == 32 {
		return b, nil
	}
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	b = make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	if err := os.WriteFile(file, b, 0o600); err != nil {
		return nil, err
	}
	return b, nil
}

// sign binds a token to the runner and the identity it was registered with:
// a runner name registered again with another identity gets another token.
func (s *Server) sign(runner string, id Identity) string {
	m := hmac.New(sha256.New, s.key)
	m.Write([]byte("pitlane-actions-cache\x00" + runner + "\x00" + strings.ToLower(id.Owner) + "\x00" + strings.ToLower(id.Repo)))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

// Register lets a runner use the cache and returns its token. Tokens are
// derived from the runner name and identity (runner names are random and
// used once), so registering an adopted runner again after a restart gives
// the token it already has. A token is only valid while its runner is
// registered: Revoke ends it with the job.
func (s *Server) Register(runner string, id Identity) string {
	s.mu.Lock()
	s.runners[runner] = id
	s.mu.Unlock()
	return runner + "." + s.sign(runner, id)
}

// Revoke invalidates a runner's token and drops its unfinished uploads.
func (s *Server) Revoke(runner string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runners, runner)
	for id, u := range s.pending {
		if u.runner == runner {
			s.dropUploadLocked(id, u)
		}
	}
}

// identify checks a token and returns its runner and identity.
func (s *Server) identify(token string) (string, Identity, bool) {
	i := strings.LastIndexByte(token, '.')
	if i <= 0 {
		return "", Identity{}, false
	}
	runner, mac := token[:i], token[i+1:]
	s.mu.Lock()
	id, ok := s.runners[runner]
	s.mu.Unlock()
	if !ok || !hmac.Equal([]byte(mac), []byte(s.sign(runner, id))) {
		return "", Identity{}, false
	}
	return runner, id, true
}

// GitHub logins and repository names.
var namePattern = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,100}$`)

// namespace returns the storage namespace of owner/repo for an identity, or
// false when the identity may not use it: never another owner, and only its
// own repository for a repository target.
func namespace(id Identity, owner, repo string) (string, bool) {
	if !namePattern.MatchString(owner) || !namePattern.MatchString(repo) || repo == "." || repo == ".." {
		return "", false
	}
	if id.Owner == "" || !strings.EqualFold(owner, id.Owner) {
		return "", false
	}
	if id.Repo != "" && !strings.EqualFold(repo, id.Repo) {
		return "", false
	}
	return strings.ToLower(owner) + "/" + strings.ToLower(repo), true
}

// scopes returns the branch scopes a request may read (the writable scope
// first) and the one it may write, from the `ac` claim of the job token the
// client sends (ACTIONS_RUNTIME_TOKEN). The token is GitHub's; Pitlane
// cannot verify its signature, so this is GitHub-like scoping between the
// branches of one repository, not a security boundary (the owner check is).
// Without a readable claim everything uses the scope "".
func scopes(authorization string) (read []string, write string, canWrite bool) {
	tok, ok := strings.CutPrefix(authorization, "Bearer ")
	parts := strings.Split(tok, ".")
	if !ok || len(parts) != 3 {
		return []string{""}, "", true
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return []string{""}, "", true
	}
	var claims struct {
		AC string `json:"ac"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.AC == "" {
		return []string{""}, "", true
	}
	var acs []struct {
		Scope      string `json:"Scope"`
		Permission int    `json:"Permission"`
	}
	if json.Unmarshal([]byte(claims.AC), &acs) != nil || len(acs) == 0 {
		return []string{""}, "", true
	}
	// The writable scope (the job's own ref) is searched first, then the
	// readable ones in the token's order (base branch, default branch).
	for _, a := range acs {
		if a.Permission&2 != 0 {
			write, canWrite = a.Scope, true
			if a.Permission&1 != 0 {
				read = append(read, a.Scope)
			}
			break
		}
	}
	for _, a := range acs {
		if a.Permission&1 != 0 && !slices.Contains(read, a.Scope) {
			read = append(read, a.Scope)
		}
	}
	return read, write, canWrite
}

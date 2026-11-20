package server

import (
	"net/http"

	"github.com/TuroYT/pitlane/internal/store"
)

// route is an HTTP route and the least role it needs ("" for a public
// route, which checks what it needs itself).
type route struct {
	pattern string
	role    string
	h       http.HandlerFunc
}

const (
	viewer   = store.RoleViewer
	operator = store.RoleOperator
	admin    = store.RoleAdmin
)

// routes lists every route. The role is the floor: a few handlers check
// more (an operator may only pause or resume a target).
func (s *Server) routes() []route {
	return []route{
		{"GET /healthz", "", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok\n")) }},
		{"GET /readyz", "", s.handleReadyz},
		{"GET /metrics", "", s.handleMetrics}, // own authorization (token, session or loopback)

		// Public: status, setup and login.
		{"GET /api/status", "", s.handleStatus},
		{"POST /api/setup/unlock", "", s.handleSetupUnlock},
		{"POST /api/setup/manifest", "", s.handleSetupManifest}, // setup token, then admins only
		{"GET /setup/callback", "", s.handleSetupCallback},
		{"GET /auth/login", "", s.handleLogin},
		{"GET /auth/callback", "", s.handleAuthCallback},
		{"POST /api/logout", "", s.handleLogout},

		// Read only.
		{"GET /api/state", viewer, s.handleState},
		{"GET /api/events", viewer, s.handleEvents},
		{"GET /api/jobs", viewer, s.handleJobs},
		{"GET /api/jobs/repos", viewer, s.handleJobRepos},
		{"GET /api/jobs/memory", viewer, s.handleJobMemory},
		{"GET /api/runners/{name}/logs", viewer, s.handleLogs},
		{"GET /api/runners/{name}/job-log", viewer, s.handleJobLog},
		{"GET /api/runners/{name}/github", viewer, s.handleGitHubJob},
		{"GET /api/compare", viewer, s.handleCompare},

		// Operators: pause or resume targets (checked in the handler), updates
		// status, update check, runner image pull.
		{"PATCH /api/targets/{id}", operator, s.handleUpdateTarget},
		{"GET /api/updates", operator, s.handleUpdates},
		{"POST /api/updates/check", operator, s.handleUpdates},
		{"POST /api/updates/pull-runner", operator, s.handleUpdates},

		// Admins.
		{"GET /api/installations", admin, s.handleInstallations},
		{"POST /api/targets", admin, s.handleCreateTarget},
		{"DELETE /api/targets/{id}", admin, s.handleDeleteTarget},
		{"GET /api/settings", admin, s.handleGetSettings},
		{"PUT /api/settings", admin, s.handlePutSettings},
		{"/api/updates", admin, s.handleUpdates},  // anything else under /api/updates:
		{"/api/updates/", admin, s.handleUpdates}, // apply, preferences
		{"GET /api/users", admin, s.handleUsers},
		{"PUT /api/users/{login}", admin, s.handlePutUser},
		{"DELETE /api/users/{login}", admin, s.handleDeleteUser},
		{"POST /api/access-rules", admin, s.handlePutRule},
		{"DELETE /api/access-rules/{id}", admin, s.handleDeleteRule},
		{"GET /api/audit", admin, s.handleAudit},

		{"/", "", s.handleWeb},
	}
}

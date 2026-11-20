package server

import (
	"net/http"
	"strings"
)

// contentSecurityPolicy is sent with every response. The UI is a Vue single
// page application built by Vite: its script and stylesheet are files of its
// own origin (no inline script or style, no eval), it reaches the API and
// the event stream on its own origin, and it loads no font, image, frame or
// plugin from anywhere else. Vue and Chart.js set styles through the CSSOM,
// which style-src allows.
//
// The setup posts a form to github.com to create the GitHub App (GitHub sends
// the browser back to Pitlane afterwards), hence form-action. Plain links to
// GitHub and the OAuth redirects are navigations, which no directive blocks.
var contentSecurityPolicy = strings.Join([]string{
	"default-src 'none'",
	"script-src 'self'",
	"style-src 'self'",
	"img-src 'self'",
	"connect-src 'self'", // the API and /api/events
	"form-action 'self' https://github.com",
	"base-uri 'none'",
	"frame-ancestors 'none'",
}, "; ")

// permissionsPolicy turns off the browser features Pitlane never uses.
const permissionsPolicy = "accelerometer=(), camera=(), display-capture=(), geolocation=(), gyroscope=(), " +
	"magnetometer=(), microphone=(), payment=(), usb=()"

// hstsPolicy asks browsers to use HTTPS only, for a year. It leaves out
// includeSubDomains and preload on purpose: Pitlane cannot know what else
// runs on the domain it is served from.
const hstsPolicy = "max-age=31536000"

// setSecurityHeaders adds the security headers of every response to h.
// Strict-Transport-Security is only meaningful, and only honored by browsers,
// over HTTPS.
func setSecurityHeaders(h http.Header, https bool) {
	h.Set("Content-Security-Policy", contentSecurityPolicy)
	h.Set("X-Frame-Options", "DENY") // old browsers, which ignore frame-ancestors
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Permissions-Policy", permissionsPolicy)
	if https {
		h.Set("Strict-Transport-Security", hstsPolicy)
	}
}

// securityHeaders sets the security headers on every response, the event
// stream and the errors included.
func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header(), s.servedOverHTTPS(r))
		next.ServeHTTP(w, r)
	})
}

// servedOverHTTPS reports whether the browser reached Pitlane over HTTPS:
// TLS ends here, or at a reverse proxy that is trusted to say so in
// X-Forwarded-Proto (anyone else could claim it).
func (s *Server) servedOverHTTPS(r *http.Request) bool {
	return r.TLS != nil || (s.trustedProxy(r) && r.Header.Get("X-Forwarded-Proto") == "https")
}

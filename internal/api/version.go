package api

import (
	_ "embed"
	"encoding/json"
	"net/http"
)

// buildinfo.json is stamped by ship.bat at deploy time (UTC timestamp + the
// commit/ship message) and embedded into the binary, so the running server can
// always tell you WHEN this build was shipped and WHAT was in it — visible at
// GET /v1/version, in the dashboard/Studio header, and in the serve banner.
//
//go:embed buildinfo.json
var buildInfoRaw []byte

// BuildInfo is the deploy stamp parsed once at startup.
type BuildInfo struct {
	Built string `json:"built"` // RFC3339 UTC, when ship.bat ran
	Desc  string `json:"desc"`  // short description (the ship/commit message)
}

// Build is the current binary's deploy stamp.
var Build = func() BuildInfo {
	var b BuildInfo
	_ = json.Unmarshal(buildInfoRaw, &b)
	if b.Built == "" {
		b.Built = "unknown"
	}
	if b.Desc == "" {
		b.Desc = "(no description — run ship.bat to stamp)"
	}
	return b
}()

// BuildLine is a one-line human summary for the CLI banner.
func BuildLine() string { return "build " + Build.Built + " — " + Build.Desc }

// handleVersion reports the deploy stamp. Mounted without auth (it carries no
// data) so the UI footer can always show what's running.
func (s *Server) handleVersion(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, Build)
}

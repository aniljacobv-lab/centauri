package api

import (
	_ "embed"
	"net/http"
)

// The dashboard is one self-contained HTML file embedded in the binary,
// served at "/" without auth (it contains no data; every data call it
// makes carries the token the user enters in the UI).
//
//go:embed ui.html
var uiHTML []byte

// The CeQL textbook, also embedded and served at /ceql.
//
//go:embed ceql.html
var ceqlHTML []byte

func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(uiHTML)
}

func (s *Server) handleCeqlBook(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(ceqlHTML)
}

package mcp

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/aniljacobv-lab/centauri/internal/store"
)

// HTTPOptions wires the Streamable HTTP transport into a host server. The host
// (internal/api) has already authenticated the request; these callbacks tell
// this package which store the request targets and whether it may write.
type HTTPOptions struct {
	// Store resolves the request's store (the host's ?db= rule).
	Store func(r *http.Request) (*store.Store, error)
	// ReadOnly reports whether the request authenticated with read-only
	// rights (read token, read-scoped SSO, follower node). Read-only requests
	// see and may call only tools that cannot append.
	ReadOnly func(r *http.Request) bool
}

// maxBody caps one MCP POST, matching the API's JSON body cap.
const maxBody = 8 << 20

// Handler serves MCP over Streamable HTTP (MCP 2025-03-26 / 2025-06-18), the
// transport cloud assistants (Claude's connectors, Meta Muse's connectors) use
// to reach a remote server. It is stateless: each POST carries one JSON-RPC
// message (or a batch) and gets its answer as application/json. There is no
// session id and no server-initiated stream, so GET answers 405 (the spec's
// way of saying "no SSE stream here") and DELETE has nothing to end.
func Handler(opts HTTPOptions) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// DNS-rebinding guard (the spec's MUST): a browser page on another
		// origin may not drive this endpoint. Server-to-server clients send no
		// Origin and pass.
		if o := r.Header.Get("Origin"); o != "" && !sameHost(o, r.Host) {
			jsonErr(w, http.StatusForbidden, "origin not allowed")
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			jsonErr(w, http.StatusMethodNotAllowed, "this MCP endpoint is stateless: POST JSON-RPC messages; no SSE stream is offered")
			return
		}
		if v := r.Header.Get("MCP-Protocol-Version"); v != "" && negotiate(v) != v {
			jsonErr(w, http.StatusBadRequest, "unsupported MCP-Protocol-Version "+v)
			return
		}
		st, err := opts.Store(r)
		if err != nil {
			jsonErr(w, http.StatusNotFound, err.Error())
			return
		}
		srv := New(st, nil, nil)
		if opts.ReadOnly != nil && opts.ReadOnly(r) {
			srv.ReadOnly()
		}
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))
		if err != nil {
			jsonErr(w, http.StatusRequestEntityTooLarge, "request body too large")
			return
		}
		out, ok := srv.HandleHTTPBody(body)
		if !ok {
			w.WriteHeader(http.StatusAccepted) // notifications / responses only: nothing to answer
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(out)
	})
}

// HandleHTTPBody answers one POST body: a single JSON-RPC message or a batch.
// It returns ok=false when the body held only notifications (no reply due).
func (s *Server) HandleHTTPBody(body []byte) (out []byte, ok bool) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(trimmed, &batch); err != nil || len(batch) == 0 {
			return mustJSON(parseError()), true
		}
		resps := []rpcResponse{}
		for _, raw := range batch {
			if r, due := s.one(raw); due {
				resps = append(resps, r)
			}
		}
		if len(resps) == 0 {
			return nil, false
		}
		return mustJSON(resps), true
	}
	r, due := s.one(trimmed)
	if !due {
		return nil, false
	}
	return mustJSON(r), true
}

// one answers a single message; due=false for a notification or a response.
func (s *Server) one(raw []byte) (rpcResponse, bool) {
	var req rpcRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return parseError(), true
	}
	if req.ID == nil || req.Method == "" {
		return rpcResponse{}, false // notification (no id) or a client's response to us
	}
	resp := s.handle(&req)
	resp.JSONRPC = "2.0"
	return resp, true
}

func parseError() rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{Code: -32700, Message: "parse error"}}
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"internal error"}}`)
	}
	return b
}

func sameHost(origin, host string) bool {
	u, err := url.Parse(origin)
	return err == nil && u.Host != "" && strings.EqualFold(u.Host, host)
}

func jsonErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

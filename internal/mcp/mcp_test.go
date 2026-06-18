package mcp

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/proxima360/centauri/internal/proc"
	"github.com/proxima360/centauri/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "c.log"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// A stored CePL procedure must surface as its own MCP tool, with its
// parameters in the schema, and be callable as proc_<name>.
func TestProcedureExposedAsTool(t *testing.T) {
	st := newStore(t)
	if _, err := proc.Save(st, "PROCEDURE dbl(n)\n  LET r = n * 2\n  RETURN r\nEND", 1000); err != nil {
		t.Fatalf("save procedure: %v", err)
	}
	s := New(st, nil, nil)

	var tool map[string]any
	for _, td := range s.procedureTools() {
		if td["name"] == "proc_dbl" {
			tool = td
		}
	}
	if tool == nil {
		t.Fatal("proc_dbl was not exposed as an MCP tool")
	}
	props, _ := tool["inputSchema"].(map[string]any)["properties"].(map[string]any)
	if _, ok := props["n"]; !ok {
		t.Fatalf("tool schema missing parameter 'n': %#v", tool["inputSchema"])
	}

	out, err := s.callTool("proc_dbl", map[string]any{"n": 5.0})
	if err != nil {
		t.Fatalf("call proc_dbl: %v", err)
	}
	if !strings.Contains(out, "10") {
		t.Fatalf("expected the procedure to return 10, got: %s", out)
	}
}

func TestUnknownProcedureToolErrors(t *testing.T) {
	s := New(newStore(t), nil, nil)
	if _, err := s.callTool("proc_nope", map[string]any{}); err == nil {
		t.Fatal("expected an error for an unknown procedure")
	}
}

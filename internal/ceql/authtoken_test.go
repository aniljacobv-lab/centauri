package ceql

import (
	"os"
	"path/filepath"
	"testing"
)

// authTokenFor: auth_file (trimmed file contents) wins over auth_env; a
// missing, unreadable, or empty file falls back to auth_env; nothing ever
// errors — worst case is an empty token.
func TestAuthTokenFor(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "cloud.key")
	if err := os.WriteFile(keyFile, []byte("  file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	emptyFile := filepath.Join(dir, "empty.key")
	if err := os.WriteFile(emptyFile, []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CENTAURI_TEST_AUTH", "env-secret")

	cases := []struct {
		name string
		cfg  map[string]any
		want string
	}{
		{"file only", map[string]any{"auth_file": keyFile}, "file-secret"},
		{"file wins over env", map[string]any{"auth_file": keyFile, "auth_env": "CENTAURI_TEST_AUTH"}, "file-secret"},
		{"missing file falls back to env", map[string]any{"auth_file": filepath.Join(dir, "nope.key"), "auth_env": "CENTAURI_TEST_AUTH"}, "env-secret"},
		{"empty file falls back to env", map[string]any{"auth_file": emptyFile, "auth_env": "CENTAURI_TEST_AUTH"}, "env-secret"},
		{"missing file, no env", map[string]any{"auth_file": filepath.Join(dir, "nope.key")}, ""},
		{"env only", map[string]any{"auth_env": "CENTAURI_TEST_AUTH"}, "env-secret"},
		{"unset env", map[string]any{"auth_env": "CENTAURI_TEST_AUTH_UNSET"}, ""},
		{"neither", map[string]any{}, ""},
		{"non-string fields ignored", map[string]any{"auth_file": 42, "auth_env": true}, ""},
	}
	for _, c := range cases {
		if got := authTokenFor(c.cfg); got != c.want {
			t.Errorf("%s: authTokenFor = %q, want %q", c.name, got, c.want)
		}
	}
}

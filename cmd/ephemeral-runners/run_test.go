package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunServiceErrors covers the ways runService gives up before it starts
// the loop. Running the loop needs libvirt, so it is left out.
func TestRunServiceErrors(t *testing.T) {
	dir := t.TempDir()
	noKey := filepath.Join(dir, "config.json")
	cfg := `{
  "github": {"app_id": 42, "installation_id": 7, "private_key_file": "` + filepath.Join(dir, "none.pem") + `"},
  "repos": ["example-org/example-repo"],
  "labels": ["self-hosted"],
  "name_prefix": "example"
}`
	if err := os.WriteFile(noKey, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown flag", []string{"-mint", "a/b"}, "flag provided but not defined: -mint"},
		{"extra argument", []string{"-config", "x", "extra"}, "unexpected arguments: [extra]"},
		{"missing config", []string{"-config", filepath.Join(dir, "missing.json")}, "missing.json: no such file or directory"},
		{"missing key", []string{"-config", noKey}, "none.pem: no such file or directory"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := runService(context.Background(), tc.args)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

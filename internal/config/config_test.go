package config

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadExample(t *testing.T) {
	c, err := Load(filepath.Join("..", "..", "examples", "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if c.Repos[0] != "example-org/example-repo" || c.PollInterval.D() != 5*time.Second {
		t.Errorf("unexpected config: %+v", c)
	}
	if c.Sizes["large"] != (Size{VCPUs: 8, MemoryGiB: 20}) {
		t.Errorf("large = %+v", c.Sizes["large"])
	}
	if c.Timeouts.Job.D() != 6*time.Hour || c.Spares.IdleAfter.D() != 20*time.Minute {
		t.Errorf("durations: %+v %+v", c.Timeouts, c.Spares)
	}
}

const minimal = `{
  "github": {"app_id": 1, "installation_id": 2, "private_key_file": "/k.pem"},
  "repos": ["example-org/example-repo"],
  "labels": ["self-hosted", "example"],
  "name_prefix": "example"
}`

func TestDefaults(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	if c.GitHub.APIURL != "https://api.github.com" {
		t.Errorf("api_url = %q", c.GitHub.APIURL)
	}
	if c.PollInterval.D() != 5*time.Second || c.DefaultSize != "medium" {
		t.Errorf("poll %v default %q", c.PollInterval, c.DefaultSize)
	}
	if len(c.Sizes) != 3 || c.Budget != (Size{24, 32}) {
		t.Errorf("sizes %v budget %v", c.Sizes, c.Budget)
	}
	if c.Spares != (Spares{"medium", 4, Duration(20 * time.Minute)}) {
		t.Errorf("spares %+v", c.Spares)
	}
	if c.Timeouts != (Timeouts{Duration(3 * time.Minute), Duration(10 * time.Minute), Duration(6 * time.Hour)}) {
		t.Errorf("timeouts %+v", c.Timeouts)
	}
	if c.VM.LibvirtURI != "qemu:///system" || c.VM.Network != "network:default" {
		t.Errorf("vm %+v", c.VM)
	}
}

func TestSizesReplaceDefaults(t *testing.T) {
	in := strings.Replace(minimal, `"name_prefix"`,
		`"sizes": {"tiny": {"vcpus": 1, "memory_gib": 1}}, "default_size": "tiny", "spares": {"count": 0}, "name_prefix"`, 1)
	c, err := Parse([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Sizes) != 1 {
		t.Errorf("sizes = %v, want only tiny", c.Sizes)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name, from, to, want string
	}{
		{"no app id", `"app_id": 1`, `"app_id": 0`, "github.app_id is missing"},
		{"no installation", `"installation_id": 2,`, ``, "github.installation_id is missing"},
		{"no key", `"/k.pem"`, `""`, "private_key_file is missing"},
		{"bad repo", `"example-org/example-repo"`, `"example-repo"`, `repo "example-repo" is not in owner/name form`},
		{"repo with extra part", `"example-org/example-repo"`, `"a/b/c"`, "not in owner/name form"},
		{"no repos", `["example-org/example-repo"]`, `[]`, "repos is empty"},
		{"no labels", `["self-hosted", "example"]`, `[]`, "labels is empty"},
		{"bad prefix", `"name_prefix": "example"`, `"name_prefix": "Ex ample"`, "name_prefix"},
		{"unknown default size", `"name_prefix"`, `"default_size": "huge", "name_prefix"`, `default_size "huge" is not one of the sizes`},
		{"unknown spare size", `"name_prefix"`, `"spares": {"size": "huge"}, "name_prefix"`, `spares.size "huge" is not one of the sizes`},
		{"size over budget", `"name_prefix"`, `"budget": {"vcpus": 6, "memory_gib": 32}, "name_prefix"`, `size "large" (8 vCPU, 20 GiB) is larger than the budget`},
		{"bad duration", `"name_prefix"`, `"poll_interval": "soon", "name_prefix"`, "invalid duration"},
		{"unknown field", `"name_prefix"`, `"repo": [], "name_prefix"`, `unknown field "repo"`},
		{"size also a label", `"example"]`, `"example", "large"]`, `size "large" is also in labels`},
		{"bad api url", `"/k.pem"`, `"/k.pem", "api_url": "api.github.com"`, "github.api_url"},
		{"sizes differ only in case", `"name_prefix"`, `"sizes": {"large": {"vcpus": 8, "memory_gib": 20}, "Large": {"vcpus": 2, "memory_gib": 4}, "medium": {"vcpus": 4, "memory_gib": 12}}, "name_prefix"`, `sizes "Large" and "large" differ only in case`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.from, tt.to, 1)
			if in == minimal {
				t.Fatalf("replacement %q not found", tt.from)
			}
			_, err := Parse([]byte(in))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want it to contain %q", err, tt.want)
			}
		})
	}
}

func TestSizeFor(t *testing.T) {
	c, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		labels []string
		size   string
		ok     bool
	}{
		{[]string{"self-hosted", "example", "large"}, "large", true},
		{[]string{"Self-Hosted", "EXAMPLE", "Small"}, "small", true},
		{[]string{"self-hosted", "example"}, "medium", true},
		{[]string{"self-hosted", "example", "small", "large"}, "", false},
		{[]string{"ubuntu-latest"}, "", false},
		{[]string{"self-hosted", "example", "gpu"}, "", false},
		{nil, "", false},
	}
	for _, tt := range tests {
		size, ok := c.SizeFor(tt.labels)
		if size != tt.size || ok != tt.ok {
			t.Errorf("SizeFor(%v) = %q, %v; want %q, %v", tt.labels, size, ok, tt.size, tt.ok)
		}
	}
}

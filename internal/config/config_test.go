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
	if c.Sizes["large"] != (Size{VCPUs: 8, MemoryGiB: 8}) {
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
	if len(c.Sizes) != 3 || c.Budget != (Size{24, 28}) {
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
		{"size over budget", `"name_prefix"`, `"budget": {"vcpus": 6, "memory_gib": 32}, "name_prefix"`, `size "large" (8 vCPU, 8 GiB) is larger than the budget`},
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
		{[]string{"self-hosted", "large", "LARGE"}, "large", true},
		{[]string{"small"}, "small", true},
		{[]string{"large", "self-hosted", "gpu"}, "", false},
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

func TestValidateErrors(t *testing.T) {
	tests := []struct {
		name, from, to, want string
	}{
		{"api url not http", `"/k.pem"`, `"/k.pem", "api_url": "ftp://api.github.com"`, `github.api_url "ftp://api.github.com" is not an http(s) URL`},
		{"api url no host", `"/k.pem"`, `"/k.pem", "api_url": "https://"`, `github.api_url "https://" is not an http(s) URL`},
		{"api url unparsable", `"/k.pem"`, `"/k.pem", "api_url": "http://[::1"`, `github.api_url "http://[::1" is not an http(s) URL`},
		{"repo twice", `["example-org/example-repo"]`, `["example-org/example-repo", "Example-Org/Example-Repo"]`, `repo "Example-Org/Example-Repo" is listed twice`},
		{"blank label", `"example"]`, `"example", " "]`, "labels has an empty label"},
		{"zero poll interval", `"name_prefix"`, `"poll_interval": "0s", "name_prefix"`, "poll_interval must be more than zero"},
		{"zero budget vcpus", `"name_prefix"`, `"budget": {"vcpus": 0, "memory_gib": 28}, "sizes": {"medium": {"vcpus": 0, "memory_gib": 4}}, "name_prefix"`,
			"budget needs vcpus and memory_gib above zero\n" + `size "medium" needs vcpus and memory_gib above zero`},
		{"zero budget memory", `"name_prefix"`, `"budget": {"vcpus": 24, "memory_gib": 0}, "sizes": {"medium": {"vcpus": 4, "memory_gib": 0}}, "name_prefix"`,
			"budget needs vcpus and memory_gib above zero\n" + `size "medium" needs vcpus and memory_gib above zero`},
		{"no sizes", `"name_prefix"`, `"sizes": {}, "name_prefix"`,
			"sizes is empty\n" + `default_size "medium" is not one of the sizes` + "\n" + `spares.size "medium" is not one of the sizes`},
		{"blank size name", `"name_prefix"`, `"sizes": {" ": {"vcpus": 1, "memory_gib": 1}, "medium": {"vcpus": 4, "memory_gib": 4}}, "name_prefix"`, "sizes has an empty name"},
		{"size zero vcpus", `"name_prefix"`, `"sizes": {"medium": {"vcpus": 0, "memory_gib": 4}}, "name_prefix"`, `size "medium" needs vcpus and memory_gib above zero`},
		{"size zero memory", `"name_prefix"`, `"sizes": {"medium": {"vcpus": 4, "memory_gib": 0}}, "name_prefix"`, `size "medium" needs vcpus and memory_gib above zero`},
		{"size memory over budget", `"name_prefix"`, `"sizes": {"medium": {"vcpus": 4, "memory_gib": 29}}, "name_prefix"`, `size "medium" (4 vCPU, 29 GiB) is larger than the budget (24 vCPU, 28 GiB)`},
		{"size vcpus over budget", `"name_prefix"`, `"sizes": {"medium": {"vcpus": 25, "memory_gib": 4}}, "name_prefix"`, `size "medium" (25 vCPU, 4 GiB) is larger than the budget (24 vCPU, 28 GiB)`},
		{"size label any case", `"example"]`, `"example", "MEDIUM"]`, `size "medium" is also in labels`},
		{"negative spares", `"name_prefix"`, `"spares": {"count": -1}, "name_prefix"`, "spares.count must not be negative"},
		{"zero spare idle", `"name_prefix"`, `"spares": {"count": 1, "idle_after": "0s"}, "name_prefix"`, "spares.idle_after must be more than zero"},
		{"no image", `"name_prefix"`, `"vm": {"image": ""}, "name_prefix"`, "vm.image is missing"},
		{"no state dir", `"name_prefix"`, `"vm": {"state_dir": ""}, "name_prefix"`, "vm.state_dir is missing"},
		{"no network", `"name_prefix"`, `"vm": {"network": ""}, "name_prefix"`, "vm.network is missing"},
		{"no libvirt uri", `"name_prefix"`, `"vm": {"libvirt_uri": ""}, "name_prefix"`, "vm.libvirt_uri is missing"},
		{"zero online", `"name_prefix"`, `"timeouts": {"online": "0s"}, "name_prefix"`, "timeouts online, idle and job must all be more than zero"},
		{"zero idle", `"name_prefix"`, `"timeouts": {"idle": "0s"}, "name_prefix"`, "timeouts online, idle and job must all be more than zero"},
		{"zero job", `"name_prefix"`, `"timeouts": {"job": "0s"}, "name_prefix"`, "timeouts online, idle and job must all be more than zero"},
		{"zero app id", `"app_id": 1`, `"app_id": 0`, "github.app_id is missing"},
		{"zero installation", `"installation_id": 2`, `"installation_id": 0`, "github.installation_id is missing"},
		{"empty prefix", `"name_prefix": "example"`, `"name_prefix": ""`, `name_prefix "" must be lower-case letters, digits and hyphens`},
		{"prefix ends in hyphen", `"name_prefix": "example"`, `"name_prefix": "example-"`, `name_prefix "example-" must be lower-case letters, digits and hyphens`},
		{"every problem", `"repos": ["example-org/example-repo"]`, `"repos": [], "poll_interval": "0s"`,
			"repos is empty\npoll_interval must be more than zero"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.from, tt.to, 1)
			if in == minimal {
				t.Fatalf("replacement %q not found", tt.from)
			}
			_, err := Parse([]byte(in))
			if err == nil || err.Error() != tt.want {
				t.Errorf("err = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestValidateAccepts(t *testing.T) {
	tests := []struct {
		name, from, to string
	}{
		{"http api url", `"/k.pem"`, `"/k.pem", "api_url": "http://ghe.example/api/v3"`},
		{"size equal to budget", `"name_prefix"`, `"budget": {"vcpus": 8, "memory_gib": 8}, "name_prefix"`},
		{"no spares ignores spare size", `"name_prefix"`, `"spares": {"count": 0, "size": "huge", "idle_after": "0s"}, "name_prefix"`},
		{"one-character prefix", `"name_prefix": "example"`, `"name_prefix": "x"`},
		{"prefix with inner hyphen", `"name_prefix": "example"`, `"name_prefix": "ex-1"`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := strings.Replace(minimal, tt.from, tt.to, 1)
			if in == minimal {
				t.Fatalf("replacement %q not found", tt.from)
			}
			if _, err := Parse([]byte(in)); err != nil {
				t.Errorf("err = %v, want nil", err)
			}
		})
	}
}

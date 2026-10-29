// Package config loads and checks the ephemeral-runners configuration file.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Config is the whole configuration file.
type Config struct {
	GitHub       GitHub          `json:"github"`
	Repos        []string        `json:"repos"`
	Labels       []string        `json:"labels"`
	NamePrefix   string          `json:"name_prefix"`
	PollInterval Duration        `json:"poll_interval"`
	Sizes        map[string]Size `json:"sizes"`
	DefaultSize  string          `json:"default_size"`
	Budget       Size            `json:"budget"`
	Spares       Spares          `json:"spares"`
	VM           VM              `json:"vm"`
	Timeouts     Timeouts        `json:"timeouts"`
	MetricsFile  string          `json:"metrics_file"`
}

// GitHub holds the GitHub App credentials and the API endpoint.
type GitHub struct {
	AppID          int64  `json:"app_id"`
	InstallationID int64  `json:"installation_id"`
	PrivateKeyFile string `json:"private_key_file"`
	APIURL         string `json:"api_url"`
}

// Size is a VM shape. The budget uses the same fields for its totals.
type Size struct {
	VCPUs     int `json:"vcpus"`
	MemoryGiB int `json:"memory_gib"`
}

// Spares controls the warm spare VMs.
type Spares struct {
	Size      string   `json:"size"`
	Count     int      `json:"count"`
	IdleAfter Duration `json:"idle_after"`
}

// VM holds the libvirt settings and paths for runner VMs.
type VM struct {
	Image      string `json:"image"`
	StateDir   string `json:"state_dir"`
	Network    string `json:"network"`
	LibvirtURI string `json:"libvirt_uri"`
}

// Timeouts are the limits on each stage of a runner VM's life.
type Timeouts struct {
	Online Duration `json:"online"`
	Idle   Duration `json:"idle"`
	Job    Duration `json:"job"`
}

// Duration is a time.Duration written in JSON as a Go duration string, such as "5s".
type Duration time.Duration

// D returns d as a time.Duration.
func (d Duration) D() time.Duration { return time.Duration(d) }

func (d Duration) String() string { return time.Duration(d).String() }

// MarshalJSON writes d as a duration string.
func (d Duration) MarshalJSON() ([]byte, error) {
	return json.Marshal(time.Duration(d).String())
}

// UnmarshalJSON reads a duration string.
func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("duration must be a string such as \"5s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// DefaultSizes are the sizes used when the file has none.
func DefaultSizes() map[string]Size {
	return map[string]Size{
		"small":  {VCPUs: 2, MemoryGiB: 2},
		"medium": {VCPUs: 4, MemoryGiB: 4},
		"large":  {VCPUs: 8, MemoryGiB: 8},
	}
}

func defaults() Config {
	return Config{
		GitHub:       GitHub{APIURL: "https://api.github.com"},
		PollInterval: Duration(5 * time.Second),
		DefaultSize:  "medium",
		Budget:       Size{VCPUs: 24, MemoryGiB: 28},
		Spares:       Spares{Size: "medium", Count: 4, IdleAfter: Duration(20 * time.Minute)},
		VM: VM{
			Image:      "/var/lib/ephemeral-runners/images/current.qcow2",
			StateDir:   "/var/lib/ephemeral-runners/vms",
			Network:    "network:default",
			LibvirtURI: "qemu:///system",
		},
		Timeouts: Timeouts{
			Online: Duration(3 * time.Minute),
			Idle:   Duration(10 * time.Minute),
			Job:    Duration(6 * time.Hour),
		},
	}
}

// Load reads, fills in defaults for and checks the file at path.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	c, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// Parse reads a configuration from JSON, fills in defaults and checks it.
// Fields left out of the JSON keep their defaults; unknown fields are an error.
func Parse(b []byte) (*Config, error) {
	c := defaults()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, err
	}
	if dec.More() {
		return nil, errors.New("unexpected data after the JSON object")
	}
	if c.Sizes == nil {
		c.Sizes = DefaultSizes()
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

var (
	repoPart   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	namePrefix = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)
)

// ValidRepo reports whether s has the form owner/name.
func ValidRepo(s string) bool {
	owner, name, ok := strings.Cut(s, "/")
	return ok && repoPart.MatchString(owner) && repoPart.MatchString(name) &&
		name != "." && name != ".."
}

// Validate checks c and returns every problem found, joined.
func (c *Config) Validate() error {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	c.checkGitHub(bad)
	c.checkRepos(bad)
	c.checkLabels(bad)
	c.checkSizes(bad)
	c.checkSpares(bad)
	c.checkVM(bad)
	return errors.Join(errs...)
}

// badFunc records one problem found by a check.
type badFunc func(format string, a ...any)

func (c *Config) checkGitHub(bad badFunc) {
	if c.GitHub.AppID <= 0 {
		bad("github.app_id is missing")
	}
	if c.GitHub.InstallationID <= 0 {
		bad("github.installation_id is missing")
	}
	if c.GitHub.PrivateKeyFile == "" {
		bad("github.private_key_file is missing")
	}
	if u, err := url.Parse(c.GitHub.APIURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		bad("github.api_url %q is not an http(s) URL", c.GitHub.APIURL)
	}
}

func (c *Config) checkRepos(bad badFunc) {
	if len(c.Repos) == 0 {
		bad("repos is empty")
	}
	seen := map[string]bool{}
	for _, r := range c.Repos {
		if !ValidRepo(r) {
			bad("repo %q is not in owner/name form", r)
		}
		if seen[strings.ToLower(r)] {
			bad("repo %q is listed twice", r)
		}
		seen[strings.ToLower(r)] = true
	}
}

// checkLabels also checks name_prefix and poll_interval, which come next in
// the file and so next in the error list.
func (c *Config) checkLabels(bad badFunc) {
	if len(c.Labels) == 0 {
		bad("labels is empty")
	}
	for _, l := range c.Labels {
		if strings.TrimSpace(l) == "" {
			bad("labels has an empty label")
		}
	}
	if !namePrefix.MatchString(c.NamePrefix) {
		bad("name_prefix %q must be lower-case letters, digits and hyphens", c.NamePrefix)
	}
	if c.PollInterval <= 0 {
		bad("poll_interval must be more than zero")
	}
}

// checkSizes checks the budget, every size and default_size.
func (c *Config) checkSizes(bad badFunc) {
	if c.Budget.VCPUs <= 0 || c.Budget.MemoryGiB <= 0 {
		bad("budget needs vcpus and memory_gib above zero")
	}
	if len(c.Sizes) == 0 {
		bad("sizes is empty")
	}
	sizeSeen := map[string]string{}
	for _, name := range c.SizeNames() {
		c.checkSize(name, sizeSeen, bad)
	}
	if _, ok := c.Sizes[c.DefaultSize]; !ok {
		bad("default_size %q is not one of the sizes", c.DefaultSize)
	}
}

// checkSize checks one size. sizeSeen maps each lower-cased name already
// checked to its original spelling.
func (c *Config) checkSize(name string, sizeSeen map[string]string, bad badFunc) {
	s := c.Sizes[name]
	if strings.TrimSpace(name) == "" {
		bad("sizes has an empty name")
	}
	if other, ok := sizeSeen[strings.ToLower(name)]; ok {
		bad("sizes %q and %q differ only in case", other, name)
	}
	sizeSeen[strings.ToLower(name)] = name
	if s.VCPUs <= 0 || s.MemoryGiB <= 0 {
		bad("size %q needs vcpus and memory_gib above zero", name)
	}
	if s.VCPUs > c.Budget.VCPUs || s.MemoryGiB > c.Budget.MemoryGiB {
		bad("size %q (%d vCPU, %d GiB) is larger than the budget (%d vCPU, %d GiB)",
			name, s.VCPUs, s.MemoryGiB, c.Budget.VCPUs, c.Budget.MemoryGiB)
	}
	if slices.ContainsFunc(c.Labels, func(l string) bool { return strings.EqualFold(l, name) }) {
		bad("size %q is also in labels", name)
	}
}

func (c *Config) checkSpares(bad badFunc) {
	if c.Spares.Count < 0 {
		bad("spares.count must not be negative")
	}
	if c.Spares.Count > 0 {
		if _, ok := c.Sizes[c.Spares.Size]; !ok {
			bad("spares.size %q is not one of the sizes", c.Spares.Size)
		}
		if c.Spares.IdleAfter <= 0 {
			bad("spares.idle_after must be more than zero")
		}
	}
}

// checkVM checks the vm paths and the timeouts.
func (c *Config) checkVM(bad badFunc) {
	if c.VM.Image == "" {
		bad("vm.image is missing")
	}
	if c.VM.StateDir == "" {
		bad("vm.state_dir is missing")
	}
	if c.VM.Network == "" {
		bad("vm.network is missing")
	}
	if c.VM.LibvirtURI == "" {
		bad("vm.libvirt_uri is missing")
	}
	if c.Timeouts.Online <= 0 || c.Timeouts.Idle <= 0 || c.Timeouts.Job <= 0 {
		bad("timeouts online, idle and job must all be more than zero")
	}
}

// SizeNames returns the size names in order.
func (c *Config) SizeNames() []string {
	names := make([]string, 0, len(c.Sizes))
	for n := range c.Sizes {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// SizeFor reports which size a job with these runs-on labels gets, and
// whether this host serves it at all. Every label must be one of the
// configured labels or a size name, compared without case. A job with no
// size label gets the default size; one with two size labels, or with no
// labels at all, is not served.
func (c *Config) SizeFor(labels []string) (string, bool) {
	if len(labels) == 0 {
		return "", false
	}
	size := ""
	for _, l := range labels {
		if slices.ContainsFunc(c.Labels, func(x string) bool { return strings.EqualFold(x, l) }) {
			continue
		}
		name, ok := c.sizeName(l)
		if !ok || (size != "" && size != name) {
			return "", false
		}
		size = name
	}
	if size == "" {
		size = c.DefaultSize
	}
	return size, true
}

func (c *Config) sizeName(label string) (string, bool) {
	for n := range c.Sizes {
		if strings.EqualFold(n, label) {
			return n, true
		}
	}
	return "", false
}

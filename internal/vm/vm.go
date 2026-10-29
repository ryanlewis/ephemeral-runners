// Package vm starts, lists and destroys single-use runner VMs through
// libvirt's virsh, and hands each one its job config.
//
// Each VM is a transient libvirt domain with its files in
// <StateDir>/<name>/: a qcow2 overlay on the image, the UEFI variables, the
// serial log and, once it has a job, the job config as a small raw disk.
// When the VM powers off, libvirt forgets the domain and the caller removes
// the directory.
package vm

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// JITConfigSize is the size of the raw disk that carries a job config. The
// config is padded with zero bytes, which the VM strips.
const JITConfigSize = 64 << 10

// Spec describes a VM to create.
type Spec struct {
	Name      string // domain name, and the name of its directory
	VCPUs     int
	MemoryMiB int
	// Image is the qcow2 image to boot. A symlink is resolved when the VM
	// is created, so rebuilding the image later doesn't change the VM.
	Image string
	// Network is "network:<name>" for a libvirt network or
	// "bridge:<name>" for a host bridge.
	Network  string
	StateDir string // the VM's files go in StateDir/Name
	// JITConfig is the runner's job config. Empty starts the VM without
	// one, to wait for Attach.
	JITConfig string
}

// Runner runs a command and returns its combined output.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Exec runs commands on the host.
type Exec struct{}

// Run runs the command in the C locale, so the error messages Destroy
// looks for are not translated.
func (Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.CombinedOutput()
}

// Launcher manages runner VMs on one libvirt connection. It is safe for
// concurrent use on different VM names.
type Launcher struct {
	URI    string // libvirt connection, such as qemu:///system
	Runner Runner // nil means Exec

	mu       sync.Mutex
	creating map[string]int // VMs being set up, by name
}

var validName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

func checkName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("vm name %q: want letters, digits, '.', '_' and '-'", name)
	}
	return nil
}

func checkJIT(jit string) error {
	if len(jit) > JITConfigSize {
		return fmt.Errorf("jit config is %d bytes, more than %d", len(jit), JITConfigSize)
	}
	if strings.IndexByte(jit, 0) >= 0 {
		return errors.New("jit config contains a zero byte")
	}
	return nil
}

// Create sets up the VM's files and starts it. If JITConfig is set, the VM
// boots with its job config attached; otherwise it waits for Attach. On
// failure Create removes what it made, unless it could not destroy a domain
// that may have started: then the files stay for Prune.
func (l *Launcher) Create(ctx context.Context, s Spec) (err error) {
	if err := checkName(s.Name); err != nil {
		return err
	}
	if s.VCPUs < 1 || s.MemoryMiB < 1 {
		return fmt.Errorf("vm %s: vcpus and memory must be positive", s.Name)
	}
	if s.StateDir == "" {
		return fmt.Errorf("vm %s: no state directory", s.Name)
	}
	if err := checkJIT(s.JITConfig); err != nil {
		return fmt.Errorf("vm %s: %w", s.Name, err)
	}
	n, err := nicFor(s.Network)
	if err != nil {
		return fmt.Errorf("vm %s: %w", s.Name, err)
	}
	image, err := filepath.EvalSymlinks(s.Image)
	if err != nil {
		return fmt.Errorf("vm %s: image: %w", s.Name, err)
	}
	if image, err = filepath.Abs(image); err != nil {
		return fmt.Errorf("vm %s: image: %w", s.Name, err)
	}
	if s.StateDir, err = filepath.Abs(s.StateDir); err != nil {
		return fmt.Errorf("vm %s: state directory: %w", s.Name, err)
	}
	doc, err := domainXML(s, n)
	if err != nil {
		return fmt.Errorf("vm %s: domain xml: %w", s.Name, err)
	}

	f := filesFor(s.StateDir, s.Name)
	l.begin(s.Name)
	defer l.end(s.Name)

	if err := os.MkdirAll(s.StateDir, 0o755); err != nil {
		return fmt.Errorf("vm %s: %w", s.Name, err)
	}
	// libvirt runs QEMU as its own user, which needs to reach the files.
	if err := os.Mkdir(f.dir, 0o755); err != nil {
		return fmt.Errorf("vm %s: %w", s.Name, err)
	}
	keep := false // the domain may be running, so its files must stay
	defer func() {
		if err != nil && !keep {
			_ = os.RemoveAll(f.dir)
		}
	}()

	if out, err := l.run(ctx, "qemu-img", "create", "-q", "-f", "qcow2",
		"-F", "qcow2", "-b", image, f.overlay); err != nil {
		return cmdError("vm "+s.Name+": qemu-img create", out, err)
	}
	if s.JITConfig != "" {
		if err := writeJIT(f.jitconfig, s.JITConfig); err != nil {
			return fmt.Errorf("vm %s: %w", s.Name, err)
		}
	}
	if err := os.WriteFile(f.xml, doc, 0o644); err != nil {
		return fmt.Errorf("vm %s: %w", s.Name, err)
	}
	if out, err := l.virsh(ctx, "create", f.xml); err != nil {
		// The domain may have started before virsh failed, such as when
		// ctx ended. Make sure it is gone before its files are.
		cerr := cmdError("vm "+s.Name+": virsh create", out, err)
		if derr := l.Destroy(context.WithoutCancel(ctx), s.Name); derr != nil {
			keep = true
			return errors.Join(cerr, derr)
		}
		return cerr
	}
	return nil
}

// Attach hands a job config to a running VM that has none, by hot-plugging
// it as a disk.
func (l *Launcher) Attach(ctx context.Context, stateDir, name, jitConfig string) error {
	if err := checkName(name); err != nil {
		return err
	}
	if jitConfig == "" {
		return fmt.Errorf("vm %s: empty jit config", name)
	}
	if err := checkJIT(jitConfig); err != nil {
		return fmt.Errorf("vm %s: %w", name, err)
	}
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return fmt.Errorf("vm %s: state directory: %w", name, err)
	}
	f := filesFor(abs, name)
	if err := writeJIT(f.jitconfig, jitConfig); err != nil {
		return fmt.Errorf("vm %s: %w", name, err)
	}
	out, err := l.virsh(ctx, "attach-disk", name, f.jitconfig, jitTarget,
		"--live", "--targetbus", "virtio", "--driver", "qemu", "--subdriver", "raw",
		"--mode", "readonly", "--serial", jitSerial)
	if err != nil {
		_ = os.Remove(f.jitconfig)
		return cmdError("vm "+name+": virsh attach-disk", out, err)
	}
	return nil
}

// List returns the names of running domains that start with prefix.
func (l *Launcher) List(ctx context.Context, prefix string) ([]string, error) {
	out, err := l.virsh(ctx, "list", "--name")
	if err != nil {
		return nil, cmdError("virsh list", out, err)
	}
	var names []string
	for line := range strings.Lines(string(out)) {
		name := strings.TrimSpace(line)
		if name != "" && strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	return names, nil
}

// Destroy stops a VM at once. A VM that is already gone is not an error.
// It leaves the VM's files for Remove.
func (l *Launcher) Destroy(ctx context.Context, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	out, err := l.virsh(ctx, "destroy", name)
	if err != nil && !gone(out) {
		return cmdError("vm "+name+": virsh destroy", out, err)
	}
	return nil
}

// gone reports whether virsh output says the domain doesn't exist or has
// already stopped.
func gone(out []byte) bool {
	return bytes.Contains(out, []byte("failed to get domain")) ||
		bytes.Contains(out, []byte("Domain not found")) ||
		bytes.Contains(out, []byte("domain is not running"))
}

// Remove deletes a VM's directory. Call it once the VM is no longer running.
func (l *Launcher) Remove(stateDir, name string) error {
	if err := checkName(name); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(stateDir, name))
}

// Prune removes the directories in stateDir that start with prefix and
// belong to no running VM, such as those of VMs that have powered off. It
// leaves alone VMs that Create is still setting up, and returns the names
// it removed.
func (l *Launcher) Prune(ctx context.Context, stateDir, prefix string) ([]string, error) {
	abs, err := filepath.Abs(stateDir)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(abs)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	// Order matters: a VM whose directory was read above is either still
	// being created now, or was started before List runs.
	l.mu.Lock()
	busy := make(map[string]bool, len(l.creating))
	for name := range l.creating {
		busy[name] = true
	}
	l.mu.Unlock()
	running, err := l.List(ctx, prefix)
	if err != nil {
		return nil, err
	}

	var removed []string
	var errs []error
	for _, e := range entries {
		name := e.Name()
		dir := filepath.Join(abs, name)
		if !e.IsDir() || !strings.HasPrefix(name, prefix) || busy[name] || slices.Contains(running, name) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, err)
			continue
		}
		removed = append(removed, name)
	}
	return removed, errors.Join(errs...)
}

func (l *Launcher) begin(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.creating == nil {
		l.creating = make(map[string]int)
	}
	l.creating[name]++
}

func (l *Launcher) end(name string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.creating[name]--; l.creating[name] <= 0 {
		delete(l.creating, name)
	}
}

// writeJIT writes the config padded to JITConfigSize. It refuses to replace
// an existing file, which would change the disk of a VM that already has one.
func writeJIT(path, jit string) error {
	buf := make([]byte, JITConfigSize)
	copy(buf, jit)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(buf); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return err
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return err
	}
	return nil
}

func (l *Launcher) virsh(ctx context.Context, args ...string) ([]byte, error) {
	if l.URI != "" {
		args = append([]string{"-c", l.URI}, args...)
	}
	return l.run(ctx, "virsh", args...)
}

func (l *Launcher) run(ctx context.Context, name string, args ...string) ([]byte, error) {
	r := l.Runner
	if r == nil {
		r = Exec{}
	}
	return r.Run(ctx, name, args...)
}

func cmdError(what string, out []byte, err error) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%s: %w: %s", what, err, strconv.Quote(msg))
	}
	return fmt.Errorf("%s: %w", what, err)
}

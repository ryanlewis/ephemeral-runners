package vm

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

// fakeRunner records commands and answers them from a script keyed by the
// command and its first argument after any "-c <uri>".
type fakeRunner struct {
	mu    sync.Mutex
	calls [][]string
	out   map[string][]byte
	err   map[string]error
	// hook runs for each call before it returns, outside the lock.
	hook func(args []string)
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	call := append([]string{name}, args...)
	key := name
	rest := args
	if len(rest) >= 2 && rest[0] == "-c" {
		rest = rest[2:]
	}
	if len(rest) > 0 {
		key += " " + rest[0]
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	out, err := f.out[key], f.err[key]
	hook := f.hook
	f.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	return out, err
}

func (f *fakeRunner) commands() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var cmds []string
	for _, c := range f.calls {
		cmds = append(cmds, strings.Join(c, " "))
	}
	return cmds
}

// testImage makes a dated image and a current.qcow2 symlink to it.
func testImage(t *testing.T) (link, target string) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target = filepath.Join(dir, "runner-1.qcow2")
	if err := os.WriteFile(target, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	link = filepath.Join(dir, "current.qcow2")
	if err := os.Symlink("runner-1.qcow2", link); err != nil {
		t.Fatal(err)
	}
	return link, target
}

func testSpec(t *testing.T) Spec {
	link, _ := testImage(t)
	return Spec{
		Name:      "er-example-repo-medium-3f9a",
		VCPUs:     4,
		MemoryMiB: 12288,
		Image:     link,
		Network:   "network:default",
		StateDir:  t.TempDir(),
		JITConfig: "eyJmYWtlIjoidHJ1ZSJ9",
	}
}

func parseDomain(t *testing.T, path string) domain {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var d domain
	if err := xml.Unmarshal(b, &d); err != nil {
		t.Fatalf("domain xml does not parse: %v\n%s", err, b)
	}
	return d
}

func TestCreateCold(t *testing.T) {
	s := testSpec(t)
	link, target := testImage(t)
	s.Image = link
	fr := &fakeRunner{}
	l := &Launcher{URI: "qemu:///system", Runner: fr}
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}

	dir := filepath.Join(s.StateDir, s.Name)
	want := []string{
		"qemu-img create -q -f qcow2 -F qcow2 -b " + target + " " + dir + "/disk.qcow2",
		"virsh -c qemu:///system create " + dir + "/domain.xml",
	}
	if got := fr.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n got %q\nwant %q", got, want)
	}

	jit, err := os.ReadFile(filepath.Join(dir, "jitconfig.img"))
	if err != nil {
		t.Fatal(err)
	}
	if len(jit) != JITConfigSize {
		t.Errorf("jit disk is %d bytes, want %d", len(jit), JITConfigSize)
	}
	if got := string(bytes.TrimRight(jit, "\x00")); got != s.JITConfig {
		t.Errorf("jit disk holds %q, want %q", got, s.JITConfig)
	}

	d := parseDomain(t, filepath.Join(dir, "domain.xml"))
	if d.Type != "kvm" || d.Name != s.Name || d.VCPU != 4 {
		t.Errorf("type %q name %q vcpu %d", d.Type, d.Name, d.VCPU)
	}
	if d.Memory != (memory{Unit: "MiB", Value: 12288}) {
		t.Errorf("memory %+v", d.Memory)
	}
	if d.OS.Firmware != "efi" || d.OS.Type.Machine != "q35" || d.OS.Type.Value != "hvm" {
		t.Errorf("os %+v", d.OS)
	}
	wantFW := []feature{{"no", "secure-boot"}, {"no", "enrolled-keys"}}
	if !slices.Equal(d.OS.FW.Features, wantFW) {
		t.Errorf("firmware features %+v, want %+v", d.OS.FW.Features, wantFW)
	}
	if d.OS.NVRAM != filepath.Join(dir, "VARS.fd") {
		t.Errorf("nvram %q is not in the VM's directory", d.OS.NVRAM)
	}
	if d.CPU.Mode != "host-passthrough" {
		t.Errorf("cpu mode %q", d.CPU.Mode)
	}
	var wantPorts []controller
	for i := 1; i <= rootPorts; i++ {
		wantPorts = append(wantPorts, controller{Type: "pci", Index: i, Model: "pcie-root-port"})
	}
	if !slices.Equal(d.Devices.Controllers, wantPorts) {
		t.Errorf("controllers %+v, want root ports to spare for hot-plug", d.Devices.Controllers)
	}
	if d.OnPoweroff != "destroy" {
		t.Errorf("on_poweroff %q", d.OnPoweroff)
	}

	if len(d.Devices.Disks) != 2 {
		t.Fatalf("%d disks, want 2", len(d.Devices.Disks))
	}
	sys := d.Devices.Disks[0]
	wantSys := disk{
		Type: "file", Device: "disk",
		Driver: diskDriver{Name: "qemu", Type: "qcow2", Cache: "none", Discard: "unmap"},
		Source: fileSource{File: filepath.Join(dir, "disk.qcow2")},
		Target: diskTarget{Dev: "vda", Bus: "virtio"},
	}
	if sys.ReadOnly != nil || sys.Serial != "" {
		t.Errorf("system disk %+v", sys)
	}
	sys.ReadOnly = nil
	if sys != wantSys {
		t.Errorf("system disk\n got %+v\nwant %+v", sys, wantSys)
	}
	j := d.Devices.Disks[1]
	if j.Serial != "jitconfig" || j.ReadOnly == nil || j.Driver.Type != "raw" ||
		j.Target != (diskTarget{Dev: "vdb", Bus: "virtio"}) ||
		j.Source.File != filepath.Join(dir, "jitconfig.img") {
		t.Errorf("jit disk %+v", j)
	}

	wantNIC := nic{Type: "network", Source: nicSource{Network: "default"}, Model: model{Type: "virtio"}}
	if !slices.Equal(d.Devices.Interfaces, []nic{wantNIC}) {
		t.Errorf("interfaces %+v", d.Devices.Interfaces)
	}
	wantSerial := serial{Type: "file", Source: serialSource{Path: filepath.Join(dir, "serial.log")}}
	if !slices.Equal(d.Devices.Serials, []serial{wantSerial}) {
		t.Errorf("serials %+v", d.Devices.Serials)
	}
	raw, _ := os.ReadFile(filepath.Join(dir, "domain.xml"))
	if bytes.Contains(raw, []byte("graphics")) {
		t.Error("domain has graphics")
	}
}

func TestCreateWarmHasNoJITDisk(t *testing.T) {
	s := testSpec(t)
	s.JITConfig = ""
	l := &Launcher{Runner: &fakeRunner{}}
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.StateDir, s.Name)
	if _, err := os.Stat(filepath.Join(dir, "jitconfig.img")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("jit disk exists: %v", err)
	}
	d := parseDomain(t, filepath.Join(dir, "domain.xml"))
	if len(d.Devices.Disks) != 1 {
		t.Errorf("%d disks, want 1", len(d.Devices.Disks))
	}
}

func TestCreateBridge(t *testing.T) {
	s := testSpec(t)
	s.Network = "bridge:br-runners"
	l := &Launcher{Runner: &fakeRunner{}}
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	d := parseDomain(t, filepath.Join(s.StateDir, s.Name, "domain.xml"))
	want := nic{Type: "bridge", Source: nicSource{Bridge: "br-runners"}, Model: model{Type: "virtio"}}
	if !slices.Equal(d.Devices.Interfaces, []nic{want}) {
		t.Errorf("interfaces %+v", d.Devices.Interfaces)
	}
}

func TestCreateRejects(t *testing.T) {
	big := strings.Repeat("a", JITConfigSize+1)
	tests := []struct {
		name string
		edit func(*Spec)
	}{
		{"empty name", func(s *Spec) { s.Name = "" }},
		{"slash in name", func(s *Spec) { s.Name = "a/b" }},
		{"dot-dot name", func(s *Spec) { s.Name = ".." }},
		{"zero vcpus", func(s *Spec) { s.VCPUs = 0 }},
		{"zero memory", func(s *Spec) { s.MemoryMiB = 0 }},
		{"no state dir", func(s *Spec) { s.StateDir = "" }},
		{"network without kind", func(s *Spec) { s.Network = "default" }},
		{"unknown network kind", func(s *Spec) { s.Network = "direct:eth0" }},
		{"network without name", func(s *Spec) { s.Network = "bridge:" }},
		{"missing image", func(s *Spec) { s.Image = "/nonexistent/current.qcow2" }},
		{"jit config too big", func(s *Spec) { s.JITConfig = big }},
		{"jit config with a zero byte", func(s *Spec) { s.JITConfig = "a\x00b" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := testSpec(t)
			tt.edit(&s)
			fr := &fakeRunner{}
			l := &Launcher{Runner: fr}
			if err := l.Create(context.Background(), s); err == nil {
				t.Fatal("no error")
			}
			if len(fr.calls) != 0 {
				t.Errorf("ran %q", fr.commands())
			}
		})
	}
}

func TestCreateJITExactlyFull(t *testing.T) {
	s := testSpec(t)
	s.JITConfig = strings.Repeat("a", JITConfigSize)
	l := &Launcher{Runner: &fakeRunner{}}
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(s.StateDir, s.Name, "jitconfig.img"))
	if string(b) != s.JITConfig {
		t.Error("jit disk differs from the config")
	}
}

func TestCreateFailureCleansUp(t *testing.T) {
	for _, cmd := range []string{"qemu-img create", "virsh create"} {
		t.Run(cmd, func(t *testing.T) {
			s := testSpec(t)
			fr := &fakeRunner{
				out: map[string][]byte{cmd: []byte("error: it broke\n")},
				err: map[string]error{cmd: errors.New("exit status 1")},
			}
			l := &Launcher{Runner: fr}
			err := l.Create(context.Background(), s)
			if err == nil || !strings.Contains(err.Error(), "it broke") {
				t.Fatalf("error %v, want the command's output", err)
			}
			if _, err := os.Stat(filepath.Join(s.StateDir, s.Name)); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("vm directory left behind: %v", err)
			}
		})
	}
}

func TestCreateExistingNameFails(t *testing.T) {
	s := testSpec(t)
	dir := filepath.Join(s.StateDir, s.Name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dir, "keep")
	os.WriteFile(marker, nil, 0o644)
	fr := &fakeRunner{}
	if err := (&Launcher{Runner: fr}).Create(context.Background(), s); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("existing directory was touched: %v", err)
	}
	if len(fr.calls) != 0 {
		t.Errorf("ran %q", fr.commands())
	}
}

func TestAttach(t *testing.T) {
	state := t.TempDir()
	name := "er-spare-1a2b"
	os.Mkdir(filepath.Join(state, name), 0o755)
	fr := &fakeRunner{}
	l := &Launcher{URI: "qemu:///system", Runner: fr}
	if err := l.Attach(context.Background(), state, name, "fake-jit"); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(state, name, "jitconfig.img")
	want := []string{"virsh -c qemu:///system attach-disk " + name + " " + file +
		" vdb --live --targetbus virtio --driver qemu --subdriver raw --mode readonly --serial jitconfig"}
	if got := fr.commands(); !slices.Equal(got, want) {
		t.Errorf("commands:\n got %q\nwant %q", got, want)
	}
	b, _ := os.ReadFile(file)
	if len(b) != JITConfigSize || string(bytes.TrimRight(b, "\x00")) != "fake-jit" {
		t.Errorf("jit disk: %d bytes, %q", len(b), bytes.TrimRight(b, "\x00"))
	}

	// A second config must not overwrite the first.
	if err := l.Attach(context.Background(), state, name, "other"); err == nil {
		t.Error("second attach: no error")
	}
	b, _ = os.ReadFile(file)
	if string(bytes.TrimRight(b, "\x00")) != "fake-jit" {
		t.Error("second attach changed the jit disk")
	}
}

func TestAttachFailureRemovesFile(t *testing.T) {
	state := t.TempDir()
	name := "er-spare-1a2b"
	os.Mkdir(filepath.Join(state, name), 0o755)
	fr := &fakeRunner{err: map[string]error{"virsh attach-disk": errors.New("exit status 1")}}
	if err := (&Launcher{Runner: fr}).Attach(context.Background(), state, name, "fake-jit"); err == nil {
		t.Fatal("no error")
	}
	if _, err := os.Stat(filepath.Join(state, name, "jitconfig.img")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("jit disk left behind: %v", err)
	}
}

func TestAttachRejects(t *testing.T) {
	state := t.TempDir()
	for _, jit := range []string{"", "a\x00b", strings.Repeat("a", JITConfigSize+1)} {
		fr := &fakeRunner{}
		if err := (&Launcher{Runner: fr}).Attach(context.Background(), state, "er-spare-1a2b", jit); err == nil {
			t.Errorf("jit of %d bytes: no error", len(jit))
		}
		if len(fr.calls) != 0 {
			t.Errorf("ran %q", fr.commands())
		}
	}
	if err := (&Launcher{Runner: &fakeRunner{}}).Attach(context.Background(), state, "../x", "jit"); err == nil {
		t.Error("bad name: no error")
	}
}

func TestList(t *testing.T) {
	fr := &fakeRunner{out: map[string][]byte{
		"virsh list": []byte("er-a-small-0001\nother-vm\ner-spare-0002\n\n"),
	}}
	l := &Launcher{URI: "qemu:///system", Runner: fr}
	got, err := l.List(context.Background(), "er-")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"er-a-small-0001", "er-spare-0002"}; !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
	if want := []string{"virsh -c qemu:///system list --name"}; !slices.Equal(fr.commands(), want) {
		t.Errorf("commands %q", fr.commands())
	}
}

func TestListError(t *testing.T) {
	fr := &fakeRunner{
		out: map[string][]byte{"virsh list": []byte("error: failed to connect\n")},
		err: map[string]error{"virsh list": errors.New("exit status 1")},
	}
	if _, err := (&Launcher{Runner: fr}).List(context.Background(), "er-"); err == nil {
		t.Error("no error")
	}
}

func TestDestroy(t *testing.T) {
	tests := []struct {
		name    string
		out     string
		err     error
		wantErr bool
	}{
		{"running", "Domain 'x' destroyed\n", nil, false},
		{"already gone", "error: failed to get domain 'x'\n", errors.New("exit status 1"), false},
		{"already stopped", "error: Requested operation is not valid: domain is not running\n", errors.New("exit status 1"), false},
		{"other failure", "error: failed to connect to the hypervisor\n", errors.New("exit status 1"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fr := &fakeRunner{
				out: map[string][]byte{"virsh destroy": []byte(tt.out)},
				err: map[string]error{"virsh destroy": tt.err},
			}
			l := &Launcher{URI: "qemu:///system", Runner: fr}
			err := l.Destroy(context.Background(), "er-spare-1a2b")
			if (err != nil) != tt.wantErr {
				t.Errorf("error %v, want error %v", err, tt.wantErr)
			}
			if want := []string{"virsh -c qemu:///system destroy er-spare-1a2b"}; !slices.Equal(fr.commands(), want) {
				t.Errorf("commands %q", fr.commands())
			}
		})
	}
}

func TestRemove(t *testing.T) {
	state := t.TempDir()
	dir := filepath.Join(state, "er-spare-1a2b")
	os.Mkdir(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "disk.qcow2"), []byte("x"), 0o644)
	l := &Launcher{Runner: &fakeRunner{}}
	if err := l.Remove(state, "er-spare-1a2b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("directory still there: %v", err)
	}
	if err := l.Remove(state, "er-spare-1a2b"); err != nil {
		t.Errorf("removing again: %v", err)
	}
	if err := l.Remove(state, ".."); err == nil {
		t.Error("removing ..: no error")
	}
}

func TestPrune(t *testing.T) {
	state := t.TempDir()
	for _, n := range []string{"er-running", "er-stopped", "other-vm"} {
		os.Mkdir(filepath.Join(state, n), 0o755)
	}
	os.WriteFile(filepath.Join(state, "er-file"), nil, 0o644)
	fr := &fakeRunner{out: map[string][]byte{"virsh list": []byte("er-running\nother-vm\n")}}
	l := &Launcher{Runner: fr}
	removed, err := l.Prune(context.Background(), state, "er-")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{"er-stopped"}) {
		t.Errorf("removed %q", removed)
	}
	for _, n := range []string{"er-running", "other-vm", "er-file"} {
		if _, err := os.Stat(filepath.Join(state, n)); err != nil {
			t.Errorf("%s: %v", n, err)
		}
	}
}

func TestPruneMissingDir(t *testing.T) {
	removed, err := (&Launcher{Runner: &fakeRunner{}}).Prune(context.Background(), filepath.Join(t.TempDir(), "none"), "er-")
	if err != nil || removed != nil {
		t.Errorf("removed %q, error %v", removed, err)
	}
}

// A VM that Create is still setting up has a directory but no domain yet;
// Prune must leave it alone.
func TestPruneSkipsVMsBeingCreated(t *testing.T) {
	s := testSpec(t)
	l := &Launcher{}
	pruned := make(chan []string, 1)
	fr := &fakeRunner{}
	fr.hook = func(args []string) {
		if args[0] != "qemu-img" {
			return
		}
		// Prune runs while Create is between mkdir and virsh create.
		removed, err := l.Prune(context.Background(), s.StateDir, "er-")
		if err != nil {
			t.Error(err)
		}
		pruned <- removed
	}
	l.Runner = fr
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if removed := <-pruned; len(removed) != 0 {
		t.Errorf("prune removed %q during create", removed)
	}
	if _, err := os.Stat(filepath.Join(s.StateDir, s.Name, "domain.xml")); err != nil {
		t.Error(err)
	}
}

func TestConcurrentCreates(t *testing.T) {
	link, _ := testImage(t)
	state := t.TempDir()
	l := &Launcher{Runner: &fakeRunner{}}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := range 8 {
		wg.Go(func() {
			s := Spec{
				Name: "er-vm-" + string(rune('a'+i)), VCPUs: 2, MemoryMiB: 4096,
				Image: link, Network: "network:default", StateDir: state, JITConfig: "jit",
			}
			errs <- l.Create(context.Background(), s)
			l.Prune(context.Background(), state, "none-")
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

// virsh create can fail after libvirt has started the domain, for example
// when ctx ends while it runs. Create must not leave a running domain
// whose files it has deleted.
func TestCreateFailureDestroysDomain(t *testing.T) {
	s := testSpec(t)
	fr := &fakeRunner{err: map[string]error{"virsh create": errors.New("signal: killed")}}
	l := &Launcher{URI: "qemu:///system", Runner: fr}
	if err := l.Create(context.Background(), s); err == nil {
		t.Fatal("no error")
	}
	cmds := fr.commands()
	if want := "virsh -c qemu:///system destroy " + s.Name; cmds[len(cmds)-1] != want {
		t.Errorf("last command %q, want %q", cmds[len(cmds)-1], want)
	}
}

func TestPruneSkipsVMsBeingCreatedUnderAnotherPath(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "state")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	s := testSpec(t)
	s.StateDir = real
	l := &Launcher{}
	pruned := make(chan []string, 1)
	fr := &fakeRunner{}
	fr.hook = func(args []string) {
		if args[0] == "qemu-img" {
			removed, _ := l.Prune(context.Background(), link, "er-")
			pruned <- removed
		}
	}
	l.Runner = fr
	if err := l.Create(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if removed := <-pruned; len(removed) != 0 {
		t.Errorf("prune removed %q during create", removed)
	}
}

func TestExecRunsInCLocale(t *testing.T) {
	t.Setenv("LC_ALL", "de_DE.UTF-8")
	out, err := Exec{}.Run(context.Background(), "sh", "-c", "echo $LC_ALL")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(out)); got != "C" {
		t.Errorf("LC_ALL=%q, want C", got)
	}
}

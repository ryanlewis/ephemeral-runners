//go:build integration

// Boots real VMs. Run as root on a libvirt host with a built image:
//
//	go test -c -tags integration -o vm.test ./internal/vm
//	sudo ./vm.test -test.v -test.run Integration
//
// VM_TEST_IMAGE, VM_TEST_STATE_DIR, VM_TEST_URI and VM_TEST_NETWORK
// override the defaults below.
package vm

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testPrefix = "ervmtest-"

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type rig struct {
	names []string // VMs this test made, for cleanup
	l     *Launcher
	image string
	state string
	net   string
}

func newRig(t *testing.T) *rig {
	r := &rig{
		l:     &Launcher{URI: env("VM_TEST_URI", "qemu:///system")},
		image: env("VM_TEST_IMAGE", "/var/lib/ephemeral-runners/images/current.qcow2"),
		state: env("VM_TEST_STATE_DIR", "/var/lib/ephemeral-runners/test"),
		net:   env("VM_TEST_NETWORK", "network:default"),
	}
	t.Cleanup(func() {
		ctx := context.Background()
		for _, n := range r.names {
			r.l.Destroy(ctx, n)
			r.l.Remove(r.state, n)
		}
	})
	return r
}

func (r *rig) spec(t *testing.T, kind string, vcpus, mem int, jit string) Spec {
	b := make([]byte, 2)
	rand.Read(b)
	name := testPrefix + kind + "-" + hex.EncodeToString(b)
	r.names = append(r.names, name)
	return Spec{
		Name:      name,
		VCPUs:     vcpus,
		MemoryMiB: mem,
		Image:     r.image,
		Network:   r.net,
		StateDir:  r.state,
		JITConfig: jit,
	}
}

// waitSerial waits for a line starting with marker on the VM's serial
// console and returns the line.
func waitSerial(t *testing.T, s Spec, marker string, timeout time.Duration) string {
	t.Helper()
	path := filepath.Join(s.StateDir, s.Name, "serial.log")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if line, ok := serialLine(path, marker); ok {
			return line
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("%s: no %s on the serial console after %s", s.Name, marker, timeout)
	return ""
}

func serialLine(path, marker string) (string, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if i := strings.Index(line, marker); i >= 0 {
			return line[i:], true
		}
	}
	return "", false
}

func waitGone(t *testing.T, l *Launcher, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		names, err := l.List(context.Background(), name)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(names, name) {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("%s still running after %s", name, timeout)
}

// checkNoTrace checks that nothing of the VM is left: no domain, no
// directory, and no firmware variables in libvirt's own directory.
func checkNoTrace(t *testing.T, r *rig, name string) {
	t.Helper()
	if out, err := exec.Command("virsh", "-c", r.l.URI, "dominfo", name).CombinedOutput(); err == nil {
		t.Errorf("%s: domain still exists:\n%s", name, out)
	}
	if _, err := os.Stat(filepath.Join(r.state, name)); !os.IsNotExist(err) {
		t.Errorf("%s: directory still exists: %v", name, err)
	}
	matches, _ := filepath.Glob("/var/lib/libvirt/qemu/nvram/" + name + "*")
	if len(matches) > 0 {
		t.Errorf("%s: firmware variables left in libvirt's directory: %q", name, matches)
	}
}

const fakeJIT = "eyJmYWtlIjoidGhpcyBpcyBub3QgYSByZWFsIGppdCBjb25maWcifQ=="

func TestIntegrationCold(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	s := r.spec(t, "cold", 2, 2048, fakeJIT)
	ctx := context.Background()

	start := time.Now()
	if err := r.l.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	line := waitSerial(t, s, "RUNNER-START", 3*time.Minute)
	t.Logf("cold: create -> RUNNER-START %s (%s)", time.Since(start).Round(10*time.Millisecond), line)
	if l, ok := serialLine(filepath.Join(s.StateDir, s.Name, "serial.log"), "RUNNER-WAIT"); ok {
		t.Errorf("cold VM waited for its config: %s", l)
	}

	// The runner fails on the fake config, and the VM powers off.
	waitGone(t, r.l, s.Name, 3*time.Minute)
	t.Logf("cold: powered off %s after create", time.Since(start).Round(time.Second))
	if err := r.l.Remove(s.StateDir, s.Name); err != nil {
		t.Fatal(err)
	}
	checkNoTrace(t, r, s.Name)
}

func TestIntegrationWarm(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	s := r.spec(t, "warm", 4, 4096, "")
	ctx := context.Background()

	start := time.Now()
	if err := r.l.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	line := waitSerial(t, s, "RUNNER-WAIT", 3*time.Minute)
	t.Logf("warm: create -> RUNNER-WAIT %s (%s)", time.Since(start).Round(10*time.Millisecond), line)

	time.Sleep(30 * time.Second)
	serial := filepath.Join(s.StateDir, s.Name, "serial.log")
	if l, ok := serialLine(serial, "RUNNER-START"); ok {
		t.Fatalf("started without a config: %s", l)
	}
	if names, _ := r.l.List(ctx, s.Name); !slices.Contains(names, s.Name) {
		t.Fatal("spare stopped while waiting")
	}

	attached := time.Now()
	if err := r.l.Attach(ctx, s.StateDir, s.Name, fakeJIT); err != nil {
		t.Fatal(err)
	}
	line = waitSerial(t, s, "RUNNER-START", time.Minute)
	t.Logf("warm: attach -> RUNNER-START %s (%s)", time.Since(attached).Round(10*time.Millisecond), line)

	waitGone(t, r.l, s.Name, 3*time.Minute)
	if err := r.l.Remove(s.StateDir, s.Name); err != nil {
		t.Fatal(err)
	}
	checkNoTrace(t, r, s.Name)
}

func TestIntegrationDestroy(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	s := r.spec(t, "destroy", 2, 2048, "")
	ctx := context.Background()

	if err := r.l.Create(ctx, s); err != nil {
		t.Fatal(err)
	}
	waitSerial(t, s, "RUNNER-WAIT", 3*time.Minute)
	// Give the spare a config file too, so destroying it must clear one.
	if err := r.l.Attach(ctx, s.StateDir, s.Name, fakeJIT); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Destroy(ctx, s.Name); err != nil {
		t.Fatal(err)
	}
	if err := r.l.Destroy(ctx, s.Name); err != nil {
		t.Errorf("destroying again: %v", err)
	}
	if names, _ := r.l.List(ctx, s.Name); slices.Contains(names, s.Name) {
		t.Error("still listed after destroy")
	}
	removed, err := r.l.Prune(ctx, s.StateDir, s.Name)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(removed, []string{s.Name}) {
		t.Errorf("prune removed %q", removed)
	}
	checkNoTrace(t, r, s.Name)
}

func TestIntegrationSizes(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	ctx := context.Background()
	specs := []Spec{
		r.spec(t, "small", 2, 4096, ""),
		r.spec(t, "large", 8, 20480, ""),
	}
	for _, s := range specs {
		if err := r.l.Create(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range specs {
		waitSerial(t, s, "RUNNER-WAIT", 3*time.Minute)
		out, err := exec.Command("virsh", "-c", r.l.URI, "dominfo", s.Name).CombinedOutput()
		if err != nil {
			t.Fatalf("dominfo: %v\n%s", err, out)
		}
		info := dominfo(string(out))
		t.Logf("%s: CPU(s) %s, Max memory %s", s.Name, info["CPU(s)"], info["Max memory"])
		if info["CPU(s)"] != strconv.Itoa(s.VCPUs) {
			t.Errorf("%s: CPU(s) %q, want %d", s.Name, info["CPU(s)"], s.VCPUs)
		}
		if want := strconv.Itoa(s.MemoryMiB*1024) + " KiB"; info["Max memory"] != want {
			t.Errorf("%s: Max memory %q, want %q", s.Name, info["Max memory"], want)
		}
	}
	for _, s := range specs {
		if err := r.l.Destroy(ctx, s.Name); err != nil {
			t.Error(err)
		}
		if err := r.l.Remove(s.StateDir, s.Name); err != nil {
			t.Error(err)
		}
		checkNoTrace(t, r, s.Name)
	}
}

func dominfo(out string) map[string]string {
	m := map[string]string{}
	for line := range strings.Lines(out) {
		k, v, ok := strings.Cut(line, ":")
		if ok {
			m[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return m
}

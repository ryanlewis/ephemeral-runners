package vm

import (
	"encoding/xml"
	"fmt"
	"path/filepath"
	"strings"
)

// domain is the part of libvirt's domain XML that a runner VM uses.
type domain struct {
	XMLName    xml.Name `xml:"domain"`
	Type       string   `xml:"type,attr"`
	Name       string   `xml:"name"`
	Memory     memory   `xml:"memory"`
	VCPU       int      `xml:"vcpu"`
	OS         osConf   `xml:"os"`
	Features   features `xml:"features"`
	CPU        cpu      `xml:"cpu"`
	Clock      clock    `xml:"clock"`
	OnPoweroff string   `xml:"on_poweroff"`
	OnReboot   string   `xml:"on_reboot"`
	OnCrash    string   `xml:"on_crash"`
	Devices    devices  `xml:"devices"`
}

type memory struct {
	Unit  string `xml:"unit,attr"`
	Value int    `xml:",chardata"`
}

// osConf asks libvirt for UEFI firmware without Secure Boot, and keeps the
// firmware variables in the VM's own directory so they go with it.
type osConf struct {
	Firmware string   `xml:"firmware,attr"`
	Type     osType   `xml:"type"`
	FW       firmware `xml:"firmware"`
	NVRAM    string   `xml:"nvram"`
	Boot     boot     `xml:"boot"`
}

type osType struct {
	Arch    string `xml:"arch,attr"`
	Machine string `xml:"machine,attr"`
	Value   string `xml:",chardata"`
}

type firmware struct {
	Features []feature `xml:"feature"`
}

type feature struct {
	Enabled string `xml:"enabled,attr"`
	Name    string `xml:"name,attr"`
}

type boot struct {
	Dev string `xml:"dev,attr"`
}

type features struct {
	ACPI *struct{} `xml:"acpi"`
	APIC *struct{} `xml:"apic"`
}

type cpu struct {
	Mode string `xml:"mode,attr"`
}

type clock struct {
	Offset string `xml:"offset,attr"`
}

type devices struct {
	Disks      []disk   `xml:"disk"`
	Interfaces []nic    `xml:"interface"`
	Serials    []serial `xml:"serial"`
}

type disk struct {
	Type     string     `xml:"type,attr"`
	Device   string     `xml:"device,attr"`
	Driver   diskDriver `xml:"driver"`
	Source   fileSource `xml:"source"`
	Target   diskTarget `xml:"target"`
	ReadOnly *struct{}  `xml:"readonly"`
	Serial   string     `xml:"serial,omitempty"`
}

type diskDriver struct {
	Name    string `xml:"name,attr"`
	Type    string `xml:"type,attr"`
	Cache   string `xml:"cache,attr,omitempty"`
	Discard string `xml:"discard,attr,omitempty"`
}

type fileSource struct {
	File string `xml:"file,attr"`
}

type diskTarget struct {
	Dev string `xml:"dev,attr"`
	Bus string `xml:"bus,attr"`
}

type nic struct {
	Type   string    `xml:"type,attr"`
	Source nicSource `xml:"source"`
	Model  model     `xml:"model"`
}

type nicSource struct {
	Network string `xml:"network,attr,omitempty"`
	Bridge  string `xml:"bridge,attr,omitempty"`
}

type model struct {
	Type string `xml:"type,attr"`
}

type serial struct {
	Type   string       `xml:"type,attr"`
	Source serialSource `xml:"source"`
	Target serialTarget `xml:"target"`
}

type serialSource struct {
	Path string `xml:"path,attr"`
}

type serialTarget struct {
	Port int `xml:"port,attr"`
}

const (
	jitSerial = "jitconfig"
	jitTarget = "vdb"
)

// files are the paths that make up one VM, all inside its directory.
type files struct {
	dir, overlay, jitconfig, nvram, serial, xml string
}

func filesFor(stateDir, name string) files {
	dir := filepath.Join(stateDir, name)
	return files{
		dir:       dir,
		overlay:   filepath.Join(dir, "disk.qcow2"),
		jitconfig: filepath.Join(dir, "jitconfig.img"),
		nvram:     filepath.Join(dir, "VARS.fd"),
		serial:    filepath.Join(dir, "serial.log"),
		xml:       filepath.Join(dir, "domain.xml"),
	}
}

// nicFor turns "network:<name>" or "bridge:<name>" into a virtio NIC.
func nicFor(network string) (nic, error) {
	kind, name, _ := strings.Cut(network, ":")
	n := nic{Type: kind, Model: model{Type: "virtio"}}
	switch {
	case name == "":
	case kind == "network":
		n.Source.Network = name
		return n, nil
	case kind == "bridge":
		n.Source.Bridge = name
		return n, nil
	}
	return nic{}, fmt.Errorf("network %q: want network:<name> or bridge:<name>", network)
}

// jitDisk is the job config disk, attached at boot or hot-plugged.
func jitDisk(file string) disk {
	return disk{
		Type:     "file",
		Device:   "disk",
		Driver:   diskDriver{Name: "qemu", Type: "raw"},
		Source:   fileSource{File: file},
		Target:   diskTarget{Dev: jitTarget, Bus: "virtio"},
		ReadOnly: &struct{}{},
		Serial:   jitSerial,
	}
}

// domainXML renders the domain for a spec that has passed validation.
func domainXML(s Spec, n nic) ([]byte, error) {
	f := filesFor(s.StateDir, s.Name)
	d := domain{
		Type:   "kvm",
		Name:   s.Name,
		Memory: memory{Unit: "MiB", Value: s.MemoryMiB},
		VCPU:   s.VCPUs,
		OS: osConf{
			Firmware: "efi",
			Type:     osType{Arch: "x86_64", Machine: "q35", Value: "hvm"},
			FW: firmware{Features: []feature{
				{Enabled: "no", Name: "secure-boot"},
				{Enabled: "no", Name: "enrolled-keys"},
			}},
			NVRAM: f.nvram,
			Boot:  boot{Dev: "hd"},
		},
		Features:   features{ACPI: &struct{}{}, APIC: &struct{}{}},
		CPU:        cpu{Mode: "host-passthrough"},
		Clock:      clock{Offset: "utc"},
		OnPoweroff: "destroy",
		OnReboot:   "restart",
		OnCrash:    "destroy",
		Devices: devices{
			Disks: []disk{{
				Type:   "file",
				Device: "disk",
				Driver: diskDriver{Name: "qemu", Type: "qcow2", Cache: "none", Discard: "unmap"},
				Source: fileSource{File: f.overlay},
				Target: diskTarget{Dev: "vda", Bus: "virtio"},
			}},
			Interfaces: []nic{n},
			Serials: []serial{{
				Type:   "file",
				Source: serialSource{Path: f.serial},
			}},
		},
	}
	if s.JITConfig != "" {
		d.Devices.Disks = append(d.Devices.Disks, jitDisk(f.jitconfig))
	}
	return xml.MarshalIndent(d, "", "  ")
}

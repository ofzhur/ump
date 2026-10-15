package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func repoFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "profiles", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func fakeResolver(ref string) ([]string, error) {
	switch ref {
	case "ump":
		return []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOiy2Pa5thF4H4bUQ5+xccU90Xt+8VpqYwHR9OKucTeN ump@test"}, nil
	case "local":
		return nil, nil
	case "agent":
		return []string{"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 me@laptop", "# a comment", ""}, nil
	}
	if IsKeyLine(ref) {
		return []string{ref}, nil
	}
	return nil, os.ErrNotExist
}

func TestDefaultProfileAndLayouts(t *testing.T) {
	p, err := Parse(repoFile(t, "ubuntu-26.04.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	layouts, err := ParseLayouts(repoFile(t, "layouts.yaml"))
	firewalls, err := ParseFirewalls(repoFile(t, "firewalls.yaml"))
	users, _ := ParseUsers(repoFile(t, "users.yaml"))
	userdata, uerr := ParseUserdata(repoFile(t, "userdata.yaml"))
	if uerr != nil {
		t.Fatal(uerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"lvm-linear", "lvm-striped", "lvm-raid1", "lvm-example"} {
		l, ok := layouts[name]
		if !ok {
			t.Fatalf("layout %s missing", name)
		}
		if err := l.Validate(); err != nil {
			t.Errorf("layout %s: %v", name, err)
		}
	}
	spec, err := p.Resolve(layouts, firewalls, users, userdata, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if spec.OS != "ubuntu-26.04" || spec.Timezone != "UTC" || spec.Kernel.Mitigations || spec.Kernel.IPv6 || spec.Layout.Name != "lvm-linear" || len(spec.Layout.Parts) != 5 {
		t.Errorf("spec: %+v", spec)
	}
	if len(spec.Users) != 1 || spec.Users[0].Name != "ump" || !spec.Users[0].Sudo || len(spec.Users[0].Keys) != 1 || !strings.HasPrefix(spec.Users[0].Keys[0], "ssh-ed25519 ") || spec.Users[0].Shell != "/bin/bash" {
		t.Errorf("users: %+v", spec.Users)
	}
	if spec.Admin() != "ump" || !spec.Network.Rename {
		t.Errorf("admin %q, network %+v", spec.Admin(), spec.Network)
	}
	if spec.Firewall.Policy != "drop" || len(spec.Firewall.Allow) != 2 || spec.Firewall.Allow[0].Proto != "icmp" || spec.Firewall.Allow[1] != (Allow{Proto: "tcp", Port: "22"}) {
		t.Errorf("firewall: %+v", spec.Firewall)
	}
	if spec.Provision.Type != "none" || len(spec.Userdata) != 1 || spec.Userdata[0].Name != "ubuntu" || spec.Userdata[0].Type != "bash" || !strings.Contains(spec.Userdata[0].Script, "set -euo pipefail") {
		t.Errorf("provision %+v userdata %v", spec.Provision, spec.Userdata)
	}
	data, err := spec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"os", "hostname", "timezone", "storage", "kernel", "layout", "users", "userdata", "firewall", "provision", "profile"} {
		if _, ok := back[key]; !ok {
			t.Errorf("json lacks %s", key)
		}
	}
	if !strings.Contains(string(data), `"userdata": [`) || !strings.Contains(string(data), `"name": "ubuntu"`) || !strings.Contains(string(data), `"allow": [`) {
		t.Errorf("json:\n%s", data)
	}
}

func TestProfileFeatures(t *testing.T) {
	src := `
name: full
os: ubuntu-26.04
kernel: {mitigations: true, ipv6: true, extra: "console=ttyS0"}
disks:
  parts:
    - {type: slice, target: [first], size: 1M, filesystem: bios}
    - {type: slice, target: [first], size: 512M, filesystem: efi, mountpoint: /boot/efi}
    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /boot}
    - {type: md, target: ["sd.*"], size: rest, raid: 1, filesystem: lvm, vg: vg0}
    - {type: lvm, vg: vg0, name: swap, size: 4G, filesystem: swap}
    - {type: lvm, vg: vg0, name: root, size: 30%, filesystem: xfs, mountpoint: /}
    - {type: lvm, vg: vg0, name: home, size: rest, raid: 0, filesystem: ext4, mountpoint: /home}
users:
  - name: root
    keys: [ump, agent]
  - name: ops
    sudo: true
    groups: [adm]
    shell: /bin/zsh
    keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFMuCiD2rTA/oSMVg0nhlB+wcM4K0BBRvt44SxPJz6W2 ops@x", agent]
userdata: [{name: web, script: "apt-get install -y nginx\n"}]
firewall:
  policy: drop
  allow: [icmp, ssh, tcp/80, udp/53, "tcp/8000-8100", {proto: tcp, port: 443, from: 10.0.0.0/8}]
provision:
  type: ansible-pull
  repo: https://git.example.net/infra.git
  playbook: site.yml
`
	p, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := p.Resolve(nil, nil, nil, nil, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if !spec.Kernel.Mitigations || !spec.Kernel.IPv6 || spec.Kernel.Extra != "console=ttyS0" {
		t.Errorf("kernel: %+v", spec.Kernel)
	}
	if spec.Layout.Name != "inline" || len(spec.Layout.Parts) != 7 || *spec.Layout.Parts[3].Raid != 1 {
		t.Errorf("layout: %+v", spec.Layout)
	}
	if len(spec.Users) != 2 || len(spec.Users[0].Keys) != 2 || spec.Users[1].Name != "ops" || !spec.Users[1].Sudo || spec.Users[1].Shell != "/bin/zsh" || len(spec.Users[1].Keys) != 2 {
		t.Errorf("users: %+v", spec.Users)
	}
	if len(spec.Firewall.Allow) != 6 || spec.Firewall.Allow[4] != (Allow{Proto: "tcp", Port: "8000-8100"}) || spec.Firewall.Allow[5] != (Allow{Proto: "tcp", Port: "443", From: "10.0.0.0/8"}) {
		t.Errorf("firewall: %+v", spec.Firewall.Allow)
	}
	if spec.Provision.Type != "ansible-pull" || spec.Provision.Options["repo"] != "https://git.example.net/infra.git" || spec.Provision.Options["playbook"] != "site.yml" {
		t.Errorf("provision: %+v", spec.Provision)
	}
	if len(spec.Userdata) != 1 || spec.Userdata[0].Name != "web" || spec.Userdata[0].Type != "bash" || spec.Userdata[0].Script != "apt-get install -y nginx\n" {
		t.Errorf("userdata: %+v", spec.Userdata)
	}
}

func TestProfileErrors(t *testing.T) {
	layouts, _ := ParseLayouts(repoFile(t, "layouts.yaml"))
	firewalls, _ := ParseFirewalls(repoFile(t, "firewalls.yaml"))
	userdata, _ := ParseUserdata(repoFile(t, "userdata.yaml"))
	users, _ := ParseUsers(repoFile(t, "users.yaml"))
	base := "name: t\nos: ubuntu-26.04\nusers:\n  - {name: root, keys: [ump]}\n"
	cases := []struct {
		name, src, want string
	}{
		{"unknown field", base + "disks: lvm-linear\ncolour: red\n", "field colour not found"},
		{"unknown layout", base + "disks: nope\n", `layout "nope" is not in layouts.yaml (known: lvm-example, lvm-linear, lvm-raid1, lvm-striped)`},
		{"no users", "name: t\nos: ubuntu-26.04\ndisks: lvm-linear\nusers: []\n", "no users"},
		{"user without keys", "name: t\nos: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: root, keys: []}\n", "has no keys"},
		{"bad user name", "name: t\nos: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: Root, keys: [ump]}\n", `user name "Root" is not valid`},
		{"bad key ref", "name: t\nos: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: root, keys: [nothing]}\n", `key "nothing"`},
		{"bad firewall", base + "disks: lvm-linear\nfirewall: {policy: drop, allow: [tcp/99999]}\n", "port 99999 is out of range"},
		{"bad firewall entry", base + "disks: lvm-linear\nfirewall: {policy: drop, allow: [smtp]}\n", `"smtp" is not icmp, ssh, tcp/PORT or udp/PORT`},
		{"bad policy", base + "disks: lvm-linear\nfirewall: {policy: deny}\n", `policy "deny" is not accept or drop`},
		{"unknown firewall", base + "disks: lvm-linear\nfirewall: nope\n", `firewall "nope" is not in firewalls.yaml`},
		{"no admin", "os: ubuntu-26.04\ndisks: lvm-linear\nusers:\n  - {name: ops, keys: [ump]}\n", "no user has sudo and root is not listed"},
		{"unknown user", "os: ubuntu-26.04\ndisks: lvm-linear\nusers: [nobody]\n", `user "nobody" is not in users.yaml (known: ump)`},
		{"user twice", "os: ubuntu-26.04\ndisks: lvm-linear\nusers: [ump, {name: ump, sudo: true, keys: [ump]}]\n", "user ump is listed twice"},
		{"bad provision", base + "disks: lvm-linear\nprovision: {type: chef}\n", `provision type "chef"`},
		{"bad userdata name", base + "disks: lvm-linear\nuserdata: [\"rm -rf\"]\n", "is not a userdata name"},
		{"unknown userdata", base + "disks: lvm-linear\nuserdata: [nope]\n", `userdata "nope" is not in userdata.yaml`},
		{"empty userdata", base + "disks: lvm-linear\nuserdata: [{name: x, script: \"\"}]\n", "the script is empty"},
		{"userdata type", base + "disks: lvm-linear\nuserdata: [{name: x, type: chef, script: x}]\n", `type "chef" is not bash, nix`},
		{"two roots", base + "disks:\n  parts:\n    - {type: slice, target: [first], size: rest, filesystem: ext4, mountpoint: /}\n    - {type: slice, target: [first], size: 1G, filesystem: ext4, mountpoint: /}\n", "mountpoint / is used twice"},
		{"root on lvm without boot", base + "disks:\n  parts:\n    - {type: slice, target: [first], size: rest, filesystem: lvm, vg: v}\n    - {type: lvm, vg: v, name: root, size: rest, filesystem: ext4, mountpoint: /}\n", "/boot must be a slice of its own"},
		{"lvm before vg", base + "disks:\n  parts:\n    - {type: lvm, vg: v, name: root, size: rest, filesystem: ext4, mountpoint: /}\n", `vg "v" is not made by an earlier slice or md part`},
		{"bad size", base + "disks:\n  parts:\n    - {type: slice, target: [first], size: big, filesystem: ext4, mountpoint: /}\n", `size "big"`},
		{"md without raid", base + "disks:\n  parts:\n    - {type: md, target: [first], size: rest, filesystem: ext4, mountpoint: /}\n", "an md part needs a raid level"},
		{"bad target", base + "disks:\n  parts:\n    - {type: slice, target: [\"(\"], size: rest, filesystem: ext4, mountpoint: /}\n", `target "("`},
		{"no root", base + "disks:\n  parts:\n    - {type: slice, target: [first], size: rest, filesystem: ext4, mountpoint: /data}\n", "no part is mounted at /"},
	}
	for _, c := range cases {
		p, err := Parse([]byte(c.src))
		if err == nil {
			_, err = p.Resolve(layouts, firewalls, users, userdata, fakeResolver)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.name, err, c.want)
		}
	}
}

func TestSet(t *testing.T) {
	layouts, _ := ParseLayouts(repoFile(t, "layouts.yaml"))
	firewalls, _ := ParseFirewalls(repoFile(t, "firewalls.yaml"))
	userdata, _ := ParseUserdata(repoFile(t, "userdata.yaml"))
	users, _ := ParseUsers(repoFile(t, "users.yaml"))
	p, err := Parse(repoFile(t, "ubuntu-26.04.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	q, err := p.Set([]string{"disks=lvm-striped", "kernel.ipv6=true", "kernel.extra=quiet splash", "users[0]={name: ump, sudo: true, keys: [ump, agent]}", "users[ump].shell=/bin/bash",
		"userdata=[ubuntu]", "users[+]={name: ops, sudo: true, keys: [ump]}", "network.rename=false", "firewall={policy: drop, allow: [icmp, ssh, tcp/80]}", "timezone=Europe/Berlin"})
	if err != nil {
		t.Fatal(err)
	}
	if p.Disks.Name != "lvm-linear" || *p.Kernel.IPv6 || len(p.Users) != 1 || p.Users[0].Name != "ump" || p.Users[0].User != nil {
		t.Errorf("the original changed: %+v", p)
	}
	spec, err := q.Resolve(layouts, firewalls, users, userdata, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Layout.Name != "lvm-striped" || !spec.Kernel.IPv6 || spec.Kernel.Mitigations || spec.Kernel.Extra != "quiet splash" || spec.Timezone != "Europe/Berlin" || spec.Network.Rename {
		t.Errorf("spec: %+v", spec)
	}
	if len(spec.Users) != 2 || spec.Users[0].Shell != "/bin/bash" || len(spec.Users[0].Keys) != 2 || spec.Users[1].Name != "ops" || !spec.Users[1].Sudo {
		t.Errorf("users: %+v", spec.Users)
	}
	if len(spec.Userdata) != 1 || spec.Userdata[0].Name != "ubuntu" || len(spec.Firewall.Allow) != 3 || spec.Firewall.Allow[2].Port != "80" {
		t.Errorf("userdata %v, firewall %+v", spec.Userdata, spec.Firewall)
	}
	for _, c := range []struct{ set, want string }{
		{"nope=1", "field nope not found"},
		{"kernel.colour=blue", "field colour not found"},
		{"users[3].shell=/bin/sh", "users has 1 entries, no index 3"},
		{"users[bob].shell=/bin/sh", `no entry of users named "bob"`},
		{"users[0].shell=/bin/sh", "users[0] is not a mapping"},
		{"disks", "expected FIELD=VALUE"},
		{"userdata=[a", "is not YAML"},
		{"os.name=x", "os is not a mapping"},
	} {
		if _, err := p.Set([]string{c.set}); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.set, err, c.want)
		}
	}
}

func TestNixOSProfileAndUserdataTypes(t *testing.T) {
	layouts, _ := ParseLayouts(repoFile(t, "layouts.yaml"))
	firewalls, _ := ParseFirewalls(repoFile(t, "firewalls.yaml"))
	users, _ := ParseUsers(repoFile(t, "users.yaml"))
	userdata, err := ParseUserdata(repoFile(t, "userdata.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, err := Parse(repoFile(t, "nixos-25.11.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	spec, err := p.Resolve(layouts, firewalls, users, userdata, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if spec.OS != "nixos-25.11" || len(spec.Userdata) != 1 || spec.Userdata[0].Type != "nix" || !strings.Contains(spec.Userdata[0].Script, "{ config, lib, pkgs, ... }:") {
		t.Errorf("nixos spec: %+v", spec.Userdata)
	}
	if Family("nixos-25.11") != "nixos" || Family("ubuntu-26.04") != "image" || UserdataType("nixos") != "nix" || UserdataType("image") != "bash" {
		t.Error("families")
	}
	// the other way round, in both directions, is refused by name
	q, _ := p.Set([]string{"userdata=[ubuntu]"})
	if _, err := q.Resolve(layouts, firewalls, users, userdata, fakeResolver); err == nil || !strings.Contains(err.Error(), "userdata ubuntu is bash, and nixos-25.11 takes nix userdata") {
		t.Errorf("bash on nixos: %v", err)
	}
	u, _ := Parse(repoFile(t, "ubuntu-26.04.yaml"))
	u, _ = u.Set([]string{"userdata=[nixos]"})
	if _, err := u.Resolve(layouts, firewalls, users, userdata, fakeResolver); err == nil || !strings.Contains(err.Error(), "userdata nixos is nix, and ubuntu-26.04 takes bash userdata") {
		t.Errorf("nix on ubuntu: %v", err)
	}
}

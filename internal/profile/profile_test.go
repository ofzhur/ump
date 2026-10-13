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
	p, err := Parse(repoFile(t, "default.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	layouts, err := ParseLayouts(repoFile(t, "layouts.yaml"))
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
	spec, err := p.Resolve(layouts, fakeResolver)
	if err != nil {
		t.Fatal(err)
	}
	if spec.OS != "ubuntu-26.04" || spec.Timezone != "UTC" || spec.Kernel.Mitigations || spec.Kernel.IPv6 || spec.Layout.Name != "lvm-linear" || len(spec.Layout.Parts) != 5 {
		t.Errorf("spec: %+v", spec)
	}
	if len(spec.Users) != 1 || spec.Users[0].Name != "root" || len(spec.Users[0].Keys) != 1 || !strings.HasPrefix(spec.Users[0].Keys[0], "ssh-ed25519 ") || spec.Users[0].Shell != "/bin/bash" {
		t.Errorf("users: %+v", spec.Users)
	}
	if spec.Firewall.Policy != "drop" || len(spec.Firewall.Allow) != 2 || spec.Firewall.Allow[0].Proto != "icmp" || spec.Firewall.Allow[1] != (Allow{Proto: "tcp", Port: "22"}) {
		t.Errorf("firewall: %+v", spec.Firewall)
	}
	if spec.Provision.Type != "none" || len(spec.Packages) != 0 {
		t.Errorf("provision %+v packages %v", spec.Provision, spec.Packages)
	}
	data, err := spec.JSON()
	if err != nil {
		t.Fatal(err)
	}
	var back map[string]any
	if err := json.Unmarshal(data, &back); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"os", "hostname", "timezone", "storage", "kernel", "layout", "users", "packages", "firewall", "provision", "profile"} {
		if _, ok := back[key]; !ok {
			t.Errorf("json lacks %s", key)
		}
	}
	if !strings.Contains(string(data), `"packages": []`) || !strings.Contains(string(data), `"allow": [`) {
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
packages: [htop, nginx]
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
	spec, err := p.Resolve(nil, fakeResolver)
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
}

func TestProfileErrors(t *testing.T) {
	layouts, _ := ParseLayouts(repoFile(t, "layouts.yaml"))
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
		{"bad provision", base + "disks: lvm-linear\nprovision: {type: chef}\n", `provision type "chef"`},
		{"bad package", base + "disks: lvm-linear\npackages: [\"rm -rf\"]\n", "is not a package name"},
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
			_, err = p.Resolve(layouts, fakeResolver)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v, want %q", c.name, err, c.want)
		}
	}
}

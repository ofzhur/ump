package cli

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/provider"
	"github.com/ofzhur/ump/internal/state"
)

// freebsdMachine is what one machine behind the fake ssh is to an
// installation of FreeBSD: staged says that the installer in the UMP
// rescue system put the UMP FreeBSD rescue system on its disks, rescue
// that it runs that system, booted that the installed FreeBSD answers;
// polls is how often it was asked whether it is the FreeBSD rescue
// system since its reboot.
type freebsdMachine struct {
	staged, rescue, booted bool
	polls                  int
}

// fakeFreeBSD plays FreeBSD behind the fake ssh, for the machine at hand
// (freebsdMachine) and by what the test sets.
type fakeFreeBSD struct {
	freebsdMachine
	// runs: the running system is a FreeBSD as ump installs it (root has
	// no SSH login, ump has sudo); inRescue: the machine sits in the UMP
	// FreeBSD rescue system, where an installation left it; keepsBooting:
	// the provider's reset starts the disk again, not its rescue system
	runs, inRescue, keepsBooting bool
	// the staged disk: noBoot, it never comes up; lingers is how many
	// times the UMP rescue system still answers after its reboot, after
	// how many more nothing answers before the FreeBSD rescue system does
	noBoot         bool
	lingers, after int
	// stageDrop ends the connection of the first step: "before" the
	// installer said that it writes, or "after"
	stageDrop string
	// the installer for FreeBSD: installs are its command lines and specs
	// what each got on stdin; exit is its exit status (255: the
	// connection ends), exitAt that per address; noScript makes the fetch
	// fail
	installs []string
	specs    []string
	exit     int
	exitAt   map[string]int
	noScript bool
	// shutdowns counts the reboots asked of the FreeBSD rescue system over
	// SSH, and actionsAtReboot is what the provider had been told when the
	// UMP rescue system was rebooted; sudoReboots are the reboots asked of
	// a running FreeBSD, each as USER: COMMAND
	shutdowns       int
	actionsAtReboot string
	sudoReboots     []string
	// queries are the questions to the new system, as asked
	queries []string
}

// freebsdSpec says that the specification an installer got is FreeBSD's.
func freebsdSpec(stdin string) bool {
	var spec struct {
		OS string `json:"os"`
	}
	return json.Unmarshal([]byte(stdin), &spec) == nil && strings.HasPrefix(spec.OS, "freebsd")
}

// apiReboot is a provider's reboot of the machine at hand: a staged disk
// starts the UMP FreeBSD rescue system, an installed one FreeBSD.
func (e *testEnv) apiReboot() {
	fb := &e.fb
	switch {
	case e.installed && fb.staged:
		fb.rescue, fb.booted, e.booted = false, true, true
	case fb.staged:
		e.kexeced, e.inImage = false, false
		fb.rescue, fb.polls = !fb.noBoot, 0
		fb.actionsAtReboot = strings.Join(fakePower.actions, ",")
	}
}

// remoteFreeBSD plays what of the machine at the other end is FreeBSD's:
// the probe of a machine that runs FreeBSD or the UMP FreeBSD rescue
// system, the installer in the UMP rescue system staging FreeBSD, the
// reboot into the FreeBSD rescue system, the question whether it is up,
// the installer for FreeBSD, the reboot into the new system and what
// that says. ok is false for everything else.
func (e *testEnv) remoteFreeBSD(argv []string, cmd string, inProviderRescue bool) (stdout, stderr string, code int, ok bool) {
	fb := &e.fb
	stdin := e.remoteStdin[len(e.remoteStdin)-1]
	addr := sshAddr(argv)
	user := "root"
	if i := slices.Index(argv, "--"); i > 0 {
		user, _, _ = strings.Cut(argv[i-1], "@")
	}
	denied := user + "@" + addr + ": Permission denied (publickey).\n"
	inLinuxRescue := e.kexeced || e.inImage
	stocked := strings.Contains(strings.Join(fakePower.actions, ","), "stock:")
	// what answers, when it is FreeBSD
	inRescue := fb.rescue || fb.inRescue && !fb.staged && !inLinuxRescue && !stocked && (!inProviderRescue || fb.keepsBooting)
	running := fb.booted || fb.runs && !fb.staged && !e.installed && !inLinuxRescue && !stocked && (!inProviderRescue || fb.keepsBooting)
	switch {
	case strings.HasPrefix(cmd, "id -u;") && inRescue:
		if user != "root" {
			return "", denied, 255, true
		}
		return "0\n2026.11.26\n\n-\namd64\nFreeBSD\n", "", 0, true
	case strings.HasPrefix(cmd, "id -u;") && running:
		if user != "ump" {
			return "", denied, 255, true
		}
		return "1001\n-\nFreeBSD 15.1-RELEASE\nsudo\namd64\nFreeBSD\n", "", 0, true
	case strings.Contains(cmd, "shutdown -h now") && running:
		// halted, as before a provider's reset for a keep-data installation
		e.halts = append(e.halts, user+": "+cmd)
		e.haltedAt = len(fakePower.actions) + 1
		return "", "", 0, true
	case strings.HasSuffix(cmd, "shutdown -r now") && running:
		fb.sudoReboots = append(fb.sudoReboots, user+": "+cmd)
		return "", "Connection reset by peer\n", 255, true
	case strings.Contains(cmd, "/install/install.sh") && freebsdSpec(stdin) && !strings.Contains(cmd, "--dry-run") && !e.installNoScript && e.installExit == 0:
		e.installs = append(e.installs, cmd)
		plan := "==> machine: amd64, UEFI firmware, 2 CPUs, 937 MB RAM\n==> disks (1): /dev/sda (40G SSD)\n==>   /dev/sda: 1: 512K bios, 2: 512M efi at /boot/efi, 3: 2G swap, 4: rest zfs (zroot)\n==>   zroot: stripe over /dev/sda, at /\n"
		switch fb.stageDrop {
		case "before":
			return plan + "==> fetching the manifest\n", "Connection reset by peer\n", 255, true
		case "after":
			return plan + "==> fetching the manifest\n==> releasing the disks\n==> wiping the disks\n", "Connection reset by peer\n", 255, true
		}
		fb.staged = true
		return plan + "==> fetching the manifest\n==> image freebsd-15.1 version 2026.11.26: FreeBSD 15.1-RELEASE, installed from the UMP FreeBSD rescue system (freebsd-15.1 2026.11.26, 592 MB of memory at least)\n==> releasing the disks\n==> wiping the disks\n==> writing the FreeBSD rescue system to /dev/sda\n==> staged: the UMP FreeBSD rescue system is on the disks, with its boot config; a reboot starts it, and the installation goes on there\n", "", 0, true
	case cmd == "systemctl reboot" && fb.staged && !e.installed:
		e.reboots++
		e.apiReboot()
		return "", "Connection reset by peer\n", 255, true
	case cmd == freebsdRescueCommand:
		switch {
		case fb.staged && !inLinuxRescue:
			fb.polls++
			switch {
			case fb.polls <= fb.lingers:
				return "2026.10.12\nLinux 6.12.48+deb13-amd64\n", "", 0, true
			case fb.polls <= fb.lingers+fb.after || !fb.rescue:
				return "", "ssh: connect to host " + addr + " port 22: Connection refused\n", 255, true
			}
			return "2026.11.26\nFreeBSD 15.1-RELEASE\n", "", 0, true
		case inLinuxRescue:
			return "2026.10.12\nLinux 6.12.48+deb13-amd64\n", "", 0, true
		}
		return "", "cat: /etc/ump-rescue-version: No such file or directory\n", 1, true
	case strings.Contains(cmd, "/install/freebsd/install.sh"):
		fb.installs, fb.specs = append(fb.installs, cmd), append(fb.specs, stdin)
		exit := fb.exit
		if at, ok := fb.exitAt[addr]; ok {
			exit = at
		}
		machine := "==> machine: amd64, UEFI firmware, 2 CPUs, 991 MB RAM\n==> disks (1): vtbd0 (40 GiB, /dev/sda in the plan, found by size)\n==> plan: the plan of the Linux rescue system (/etc/ump/boot/plan.json)\n"
		switch {
		case !fb.rescue:
			return "", "sh: fetch: not found\n", 127, true
		case fb.noScript:
			// fetch names the URL in its error, which ends in install.sh
			return "", "fetch: https://ump.zhurko.de/dl/install/freebsd/install.sh: Not Found\n", 69, true
		case exit == 2:
			return machine, "install.sh: the plan's disk /dev/sdb (10 GiB) is not here\ninstall.sh: nothing was written to the disks\n", 2, true
		case exit == 255:
			return machine + "==> releasing the disks\n", "Connection reset by peer\n", 255, true
		case exit != 0:
			return machine + "==> releasing the disks\n", "install.sh: failed while unpacking base.txz (exit status 1)\ninstall.sh: the installation is not complete; the machine stays in the rescue system, and a reboot finds no system on its disks\n", exit, true
		}
		e.installed = true
		return machine + "==> releasing the disks\n==> installed: freebsd-15.1 15.1-RELEASE as web-01, root on zroot/ROOT/default (623M used; layout zfs over 1 disk(s)); reboot to start it\n", "", 0, true
	case cmd == "shutdown -r now" && fb.rescue:
		fb.shutdowns++
		if e.installed && !e.bootFails {
			e.apiReboot()
		}
		return "", "", 0, true
	case strings.HasPrefix(cmd, "sed -n 's/^PRETTY_NAME=//p' /etc/os-release") && fb.booted:
		fb.queries = append(fb.queries, cmd)
		if user != "ump" {
			return "", denied, 255, true
		}
		return "FreeBSD 15.1-RELEASE\nzroot/ROOT/default\nos=freebsd-15.1 image_version=15.1-RELEASE image_kernel=15.1-RELEASE profile=freebsd-15.1 layout=zfs hostname=web-01 admin=ump interfaces=ext01=52:54:00:12:34:56 uplink=ext01 \n", "", 0, true
	}
	return "", "", 0, false
}

// freebsdInstallerSuffix is how the command that runs the installer for
// FreeBSD ends: fetch with its retries, then sh.
func freebsdInstallerSuffix(storage string) string {
	return `cat > /tmp/ump-install.json && f=/tmp/ump-install.sh; i=0; until fetch -q -T 10 -o "$f" ` + storage + `/install/freebsd/install.sh; do i=$((i+1)); [ $i -lt 6 ] || exit 69; sleep 5; done; sh "$f" --spec /tmp/ump-install.json`
}

// inOrder says that the wanted pieces are in text, one after the other.
func inOrder(t *testing.T, text string, want ...string) {
	t.Helper()
	rest := text
	for _, w := range want {
		i := strings.Index(rest, w)
		if i < 0 {
			t.Errorf("%q is missing, or not in its place, in\n%s", w, text)
			return
		}
		rest = rest[i+len(w):]
	}
}

// TestReinstallFreeBSD: FreeBSD is installed in two steps, the second
// from the UMP FreeBSD rescue system, which the installer in the UMP
// rescue system put on the disks; a ZFS layout and FreeBSD are refused
// apart.
func TestReinstallFreeBSD(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	e.fb.lingers, e.fb.after = 1, 2

	// through the provider's rescue system: staged there, the provider told
	// to start the disk before the first reboot, the FreeBSD rescue system,
	// its installer with the same specification, the reboot, the new system
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--yes", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	inOrder(t, errOut,
		"Checking root access to 203.0.113.10 over SSH ... no (tried root, ump).\nGoing through the provider's rescue system.\n",
		"Waiting for the UMP rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.10.12).\n",
		"Installing freebsd-15.1 (profile freebsd-15.1 from "+filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles", "freebsd-15.1.yaml")+", layout zfs, private address 10.0.0.1 in default) on web-01 as web-01 ...\n",
		"==>   zroot: stripe over /dev/sda, at /\n",
		"==> staged: the UMP FreeBSD rescue system is on the disks, with its boot config; a reboot starts it, and the installation goes on there\n",
		"Rebooting web-01 into the UMP FreeBSD rescue system ...\n",
		"Waiting for the UMP FreeBSD rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.11.26, FreeBSD 15.1-RELEASE).\n",
		"==> plan: the plan of the Linux rescue system (/etc/ump/boot/plan.json)\n",
		"==> installed: freebsd-15.1 15.1-RELEASE as web-01, root on zroot/ROOT/default",
		"Rebooting web-01 into freebsd-15.1 ...\n",
		"Waiting for SSH on 203.0.113.10 to go down ...",
		"Removed the old host key of 203.0.113.10 from ~/.ssh/known_hosts: the system is new, and so is its host key.\n",
		"web-01 is up: FreeBSD 15.1-RELEASE, hostname web-01, root on zroot/ROOT/default; log in as ump (ump ssh web-01).\n",
		"Wrote inventory/hosts/web-01.yml: web-01 in common.\n",
		"web-01 is provisioned.",
	)
	// the provider's rescue system was off before the first reboot, and
	// nothing was asked of the provider after it
	if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard,unrescue" || e.fb.actionsAtReboot != got {
		t.Errorf("provider actions %s, at the reboot of the rescue system %s", got, e.fb.actionsAtReboot)
	}
	// each rescue system rebooted once, with its own command; the FreeBSD
	// one answered at its fourth poll
	if e.reboots != 1 || e.fb.shutdowns != 1 || e.fb.polls != 4 {
		t.Errorf("reboots %d, shutdowns %d, polls %d", e.reboots, e.fb.shutdowns, e.fb.polls)
	}
	// both installers got the same specification, a ZFS layout for
	// FreeBSD; the second is fetched with fetch and run with sh, the keys
	// put into root's first
	if len(e.installs) != 1 || !strings.HasSuffix(e.installs[0], installerPrefix("https://ump.zhurko.de/dl")) {
		t.Errorf("installer in the UMP rescue system: %q", e.installs)
	}
	if len(e.fb.installs) != 1 || !strings.HasSuffix(e.fb.installs[0], "; "+freebsdInstallerSuffix("https://ump.zhurko.de/dl")) ||
		!strings.HasPrefix(e.fb.installs[0], `mkdir -p -m 700 /root/.ssh && touch /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && for k in 'ssh-ed25519 `) {
		t.Errorf("installer for FreeBSD: %q", e.fb.installs)
	}
	spec := e.lastSpec()
	layout := spec["layout"].(map[string]any)
	parts := layout["parts"].([]any)
	if spec["os"] != "freebsd-15.1" || layout["name"] != "zfs" || parts[len(parts)-1].(map[string]any)["type"] != "zfs" {
		t.Errorf("spec: %v", spec)
	}
	var second map[string]any
	if err := json.Unmarshal([]byte(e.fb.specs[0]), &second); err != nil || second["os"] != "freebsd-15.1" {
		t.Errorf("the second specification: %v\n%s", err, e.fb.specs[0])
	}
	for i, argv := range e.remoteCalls {
		if strings.Contains(argv[len(argv)-1], "/install/install.sh") && e.remoteStdin[i] != e.fb.specs[0] {
			t.Errorf("the two installers got different specifications:\n%s\n%s", e.remoteStdin[i], e.fb.specs[0])
		}
	}
	// the question whether the FreeBSD rescue system is up goes to root
	// with UMP's key alone and no host key check; the question to the new
	// system is one both kinds of system answer
	for _, argv := range e.remoteCalls {
		line := strings.Join(argv, " ")
		if argv[len(argv)-1] == freebsdRescueCommand && !strings.Contains(line, "-o IdentitiesOnly=yes -o UserKnownHostsFile=/dev/null -o StrictHostKeyChecking=no -o LogLevel=ERROR root@203.0.113.10 -- ") {
			t.Errorf("poll: %s", line)
		}
	}
	if len(e.fb.queries) != 1 || !strings.Contains(e.fb.queries[0], `{ findmnt -no SOURCE / 2>/dev/null || mount -p 2>/dev/null | awk '$2 == "/" {print $1}'; }`) {
		t.Errorf("the question to the new system: %q", e.fb.queries)
	}
	// the result, the record and the host file
	for _, want := range []string{`"os": "freebsd-15.1"`, `"layout": "zfs"`, `"via": "provider"`, `"rebooted": true`, `"name": "FreeBSD 15.1-RELEASE"`, `"root": "zroot/ROOT/default"`, `"provisioned": true`} {
		if !strings.Contains(out, want) {
			t.Errorf("json lacks %s:\n%s", want, out)
		}
	}
	if st := e.readState(); !strings.Contains(st, "      os: freebsd-15.1\n      image_version: 15.1-RELEASE\n      kernel: 15.1-RELEASE\n      profile: freebsd-15.1\n      layout: zfs\n      hostname: web-01\n      admin: ump\n      interfaces:\n        ext01: \"52:54:00:12:34:56\"\n") {
		t.Errorf("state:\n%s", st)
	}
	if hf := e.hostFile("web-01.yml"); !strings.Contains(hf, "vars:\n  ansible_host: 203.0.113.10\n  ansible_python_interpreter: /usr/local/bin/python3\n  ump_admin: ump\n") || !strings.Contains(hf, "  ump_os: freebsd-15.1\n") {
		t.Errorf("host file:\n%s", hf)
	}
	// ump as the inventory says the same
	if out, _, code := e.run("", false, "server", "list", "--account", "one", "-o", "ansible"); code != 0 || !strings.Contains(out, `"ansible_python_interpreter": "/usr/local/bin/python3"`) {
		t.Errorf("-o ansible: code %d\n%s", code, out)
	}

	// the direct way, the other release, another layout; nothing is asked
	// of the provider
	resetFakePower()
	e.fb = fakeFreeBSD{}
	e.directRoot = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-14.5", "--layout", "zfs-mirror", "--no-provision", "--yes", "--account", "one")
	if code != 0 || len(fakePower.actions) != 0 || e.lastSpec()["os"] != "freebsd-14.5" || e.lastSpec()["layout"].(map[string]any)["name"] != "zfs-mirror" {
		t.Errorf("direct: code %d, actions %v\n%s", code, fakePower.actions, errOut)
	}
	inOrder(t, errOut, "Checking root access to 203.0.113.10 over SSH ... yes (Ubuntu 26.04 LTS).\n", "==> staged:", "Waiting for the UMP FreeBSD rescue system on 203.0.113.10", "web-01 is up: FreeBSD 15.1-RELEASE",
		"Not provisioned (--no-provision); the next ump provision includes it (ump provision --hosts web-01 for it alone).\n")

	// --dry-run is the plan of the first step: the server stays in the UMP
	// rescue system, and nothing of the second happens
	e.fb = fakeFreeBSD{}
	installs := len(e.installs)
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--dry-run", "--yes", "--account", "one")
	if code != 0 || len(e.installs) != installs+1 || !strings.HasSuffix(e.installs[installs], " --dry-run") || e.reboots != 0 || len(e.fb.installs) != 0 || e.fb.polls != 0 ||
		!strings.Contains(errOut, "Installation plan for freebsd-15.1 (profile freebsd-15.1 from ") || !strings.Contains(errOut, "==> dry run: stopping here, no disk was touched\nDry run done; web-01 stays in the UMP rescue system (ump ssh web-01 --staging).\n") {
		t.Errorf("--dry-run: code %d, reboots %d, installers for FreeBSD %v\n%s", code, e.reboots, e.fb.installs, errOut)
	}
	e.inImage = false

	// --no-reboot: the reboot into the FreeBSD rescue system is part of
	// the installation, the last one is not
	e.fb = fakeFreeBSD{}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-reboot", "--yes", "--account", "one")
	if code != 0 || e.reboots != 1 || e.fb.shutdowns != 0 || len(e.fb.installs) != 1 || e.fb.booted {
		t.Errorf("--no-reboot: code %d, reboots %d, shutdowns %d\n%s", code, e.reboots, e.fb.shutdowns, errOut)
	}
	inOrder(t, errOut, "Rebooting web-01 into the UMP FreeBSD rescue system ...\n", "==> installed: freebsd-15.1", "Installed; web-01 stays in the UMP FreeBSD rescue system, a reboot starts freebsd-15.1.\n", "Not provisioned: after the reboot, ump provision --hosts web-01 runs the playbooks on it.")
	if strings.Contains(errOut, "Rebooting web-01 into freebsd-15.1") || !strings.Contains(e.readState(), "      os: freebsd-15.1\n") {
		t.Errorf("--no-reboot rebooted, or did not record:\n%s", errOut)
	}

	// what FreeBSD does not take is refused before the server is touched
	e.fb = fakeFreeBSD{}
	e.remoteCalls, e.installs = nil, nil
	for _, c := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"--profile", "freebsd-15.1", "--layout", "lvm-raid1"}, 1, "ump: profile freebsd-15.1: layout lvm-raid1 has md, LVM or Linux file systems, which freebsd-15.1 does not take; its layouts are ZFS ones (zfs, zfs-mirror, zfs-raidz in layouts.yaml)\n"},
		{[]string{"--os", "freebsd-15.1"}, 1, "ump: profile ubuntu-26.04: layout lvm-linear has md, LVM or Linux file systems, which freebsd-15.1 does not take; its layouts are ZFS ones (zfs, zfs-mirror, zfs-raidz in layouts.yaml)\n"},
		{[]string{"--layout", "zfs"}, 1, "ump: profile ubuntu-26.04: layout zfs is a ZFS layout, which only FreeBSD takes; ubuntu-26.04 takes the others (lvm-linear, ...)\n"},
		{[]string{"--profile", "debian-13", "--layout", "zfs-raidz"}, 1, "ump: profile debian-13: layout zfs-raidz is a ZFS layout, which only FreeBSD takes; debian-13 takes the others (lvm-linear, ...)\n"},
		{[]string{"--profile", "freebsd-15.1", "--userdata", "nixos"}, 1, "ump: profile freebsd-15.1: userdata nixos is nix, and freebsd-15.1 takes bash userdata (see docs/userdata.md)\n"},
		{[]string{"--profile", "freebsd-15.1", "--keep-data"}, 2, "ump: --keep-data installs the images unpacked onto the old system's volumes (Ubuntu, Debian); freebsd-15.1 is of the family freebsd, whose installation makes its own file systems, and is not supported with it yet. web-01 was not touched\n"},
		{[]string{"--profile", "freebsd-14.5", "--keep-data", "--dry-run"}, 2, "freebsd-14.5 is of the family freebsd, whose installation makes its own file systems, and is not supported with it yet. web-01 was not touched\n"},
	} {
		_, errOut, code := e.run("", false, append([]string{"server", "reinstall", "web-01", "--yes", "--account", "one"}, c.args...)...)
		if code != c.code || !strings.HasSuffix(errOut, c.msg) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
	}
	if len(e.installs) != 0 || len(e.remoteCalls) != 0 {
		t.Errorf("the server was touched: installs %v, remote calls %v", e.installs, e.remoteCalls)
	}
	// the published profiles and layouts are there to look at and start from
	if out, _, code := e.run("", false, "config", "os", "list"); code != 0 || !strings.Contains(out, "freebsd-15.1") || !strings.Contains(out, "freebsd-14.5") || !strings.Contains(out, "FreeBSD 15.1 on every disk as one ZFS pool") {
		t.Errorf("config os list: code %d\n%s", code, out)
	}
	if out, _, code := e.run("", false, "config", "dl", "list"); code != 0 || !strings.Contains(out, "zfs-mirror") || strings.Contains(out, "unknown part type") {
		t.Errorf("config dl list: code %d\n%s", code, out)
	}
}

// privateOf is the private address the state has for a server, or "".
func (e *testEnv) privateOf(ref string) string {
	e.t.Helper()
	data, err := os.ReadFile(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"))
	if err != nil {
		return ""
	}
	doc, err := state.Parse(data)
	if err != nil {
		e.t.Fatalf("state: %v", err)
	}
	var out []string
	for _, p := range doc.Servers[ref].Private {
		out = append(out, p.Address)
	}
	return strings.Join(out, ",")
}

// TestReinstallFreeBSDStops: where an installation of FreeBSD stops, what
// ump says of the disks then, its exit status, and whether the private
// address stays the server's.
func TestReinstallFreeBSDStops(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	oldTimeout := imageWaitTimeout
	t.Cleanup(func() { imageWaitTimeout = oldTimeout })
	run := func(args ...string) (string, int) {
		t.Helper()
		resetFakePower()
		e.inImage = false
		_, errOut, code := e.run("", false, append([]string{"server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes", "--account", "one"}, args...)...)
		return errOut, code
	}

	// the first step: a connection that ended before the installer said
	// that it writes left the disks as they were, and so did its refusal,
	// which has today's sentence: the address goes back. After it, the
	// disks count as written, and the address stays the server's
	e.fb = fakeFreeBSD{stageDrop: "before"}
	errOut, code := run()
	if code != 5 || !strings.HasSuffix(errOut, "ump: the connection to 203.0.113.10 ended during the installation; the server is in the UMP rescue system, look there (ump ssh web-01 --staging)\n") || e.privateOf("fake/1") != "" || e.reboots != 0 {
		t.Errorf("dropped before the first write: code %d, private %q\n%s", code, e.privateOf("fake/1"), errOut)
	}
	e.fb = fakeFreeBSD{}
	e.installExit = 2
	errOut, code = run()
	if code != 1 || !strings.HasSuffix(errOut, "ump: the installer refused the specification or the machine (exit status 2, see above); nothing was changed, web-01 stays in the UMP rescue system\n") || e.privateOf("fake/1") != "" || e.reboots != 0 {
		t.Errorf("refused by the first step: code %d, private %q\n%s", code, e.privateOf("fake/1"), errOut)
	}
	e.installExit = 0
	e.fb = fakeFreeBSD{stageDrop: "after"}
	errOut, code = run()
	if code != 5 || !strings.Contains(errOut, "==> wiping the disks\n") || e.privateOf("fake/1") != "10.0.0.1" || e.reboots != 0 || len(e.fb.installs) != 0 {
		t.Errorf("dropped after the first write: code %d, private %q\n%s", code, e.privateOf("fake/1"), errOut)
	}

	// the FreeBSD rescue system does not come up: exit status 5, what the
	// disks hold and the way back; the disks were written
	imageWaitTimeout = 30 * time.Millisecond
	e.fb = fakeFreeBSD{noBoot: true}
	errOut, code = run()
	if code != 5 || !strings.HasSuffix(errOut, "Waiting for the UMP FreeBSD rescue system on 203.0.113.10 (up to 30ms) ...\n"+
		"ump: the UMP FreeBSD rescue system on 203.0.113.10 did not answer within 30ms. The disks of web-01 hold that rescue system and nothing else: the old system is gone, and freebsd-15.1 is not installed. "+
		"ump server rescue web-01 brings the server into the UMP rescue system through its provider; the same reinstall again, or one with another profile, goes that way by itself and starts over. "+
		"The installer of the next attempt shows the boot log the FreeBSD rescue system left on a disk, when it came as far as its network (with --dry-run it shows it and writes nothing); the server's console shows what its firmware and the kernel say. "+
		"A machine whose firmware is slow may need a longer wait (wait: 30m in config.yaml, or UMP_WAIT=30m)\n") {
		t.Errorf("no FreeBSD rescue system: code %d\n%s", code, errOut)
	}
	if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard,unrescue" || e.reboots != 1 || len(e.fb.installs) != 0 || e.privateOf("fake/1") != "10.0.0.1" || strings.Contains(e.readState(), "installed:") {
		t.Errorf("no FreeBSD rescue system: actions %s, reboots %d, private %q\n%s", got, e.reboots, e.privateOf("fake/1"), e.readState())
	}
	imageWaitTimeout = oldTimeout

	// the installer for FreeBSD: refused (2), failed (1), the connection
	// ended (255), not fetched; the server stays in the FreeBSD rescue
	// system each time, and is not rebooted
	for _, c := range []struct {
		name string
		fb   fakeFreeBSD
		code int
		out  string
		msg  string
	}{
		{"refused", fakeFreeBSD{exit: 2}, 1, "install.sh: the plan's disk /dev/sdb (10 GiB) is not here\ninstall.sh: nothing was written to the disks\n",
			"ump: the installer for FreeBSD refused the specification or the machine (exit status 2, see above) and wrote nothing; web-01 stays in the UMP FreeBSD rescue system (ump ssh web-01 --staging): its disks hold that rescue system alone, and the old system is gone; the same reinstall again starts over, through the provider's rescue system\n"},
		{"failed", fakeFreeBSD{exit: 1}, 1, "install.sh: failed while unpacking base.txz (exit status 1)\n",
			"ump: the installation on web-01 failed in the UMP FreeBSD rescue system (exit status 1, see above); the server stays there (ump ssh web-01 --staging), and a reboot finds no system on its disks; the same reinstall again starts over, through the provider's rescue system\n"},
		{"the connection ended", fakeFreeBSD{exit: 255}, 5, "==> releasing the disks\n",
			"ump: the connection to 203.0.113.10 ended during the installation; the server is in the UMP FreeBSD rescue system, or was: look there (ump ssh web-01 --staging), and at its console. freebsd-15.1 is not installed until the installer said so; the same reinstall again starts over, through the provider's rescue system\n"},
		{"not fetched", fakeFreeBSD{noScript: true}, 1, "fetch: https://ump.zhurko.de/dl/install/freebsd/install.sh: Not Found\n",
			"ump: 203.0.113.10 could not fetch the installer for FreeBSD from https://ump.zhurko.de/dl/install/freebsd/install.sh (see above); the server must reach the storage. web-01 stays in the UMP FreeBSD rescue system (ump ssh web-01 --staging): its disks hold that rescue system alone, and the old system is gone; the same reinstall again starts over, through the provider's rescue system\n"},
	} {
		e.fb = c.fb
		errOut, code := run()
		if code != c.code || !strings.Contains(errOut, c.out) || !strings.HasSuffix(errOut, c.msg) {
			t.Errorf("%s: code %d\n%s", c.name, code, errOut)
		}
		if len(e.fb.installs) != 1 || e.fb.shutdowns != 0 || !e.fb.rescue || e.privateOf("fake/1") != "10.0.0.1" || strings.Contains(e.readState(), "installed:") {
			t.Errorf("%s: installers %d, shutdowns %d, private %q", c.name, len(e.fb.installs), e.fb.shutdowns, e.privateOf("fake/1"))
		}
	}

	// the new system does not answer as the administrator
	e.fb = fakeFreeBSD{}
	e.bootFails = true
	errOut, code = run()
	if code != 5 || e.fb.shutdowns != 1 || !strings.HasSuffix(errOut, "ump: SSH answers on 203.0.113.10, but not as ump with UMP's key; the new system may not have booted, look at the server's console\n") {
		t.Errorf("the new system does not answer: code %d\n%s", code, errOut)
	}
	e.bootFails = false
}

// TestReinstallFreeBSDDiskBoot: a provider that starts its own kernel
// unless told otherwise is switched to the disks before the first
// reboot, and both reboots are its own.
func TestReinstallFreeBSDDiskBoot(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  db:\n    type: zdiskboot\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	fakeAPIReboot = e.apiReboot
	defer func() { fakeAPIReboot = nil }()
	e.directRoot = true
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes")
	if code != 0 || !strings.Contains(errOut, "web-01 is up: FreeBSD 15.1-RELEASE") {
		t.Fatalf("install: code %d\n%s", code, errOut)
	}
	if got := strings.Join(fakePower.actions, " "); got != "bootdisk reboot:hard reboot:hard" || e.fb.actionsAtReboot != "bootdisk,reboot:hard" || e.reboots != 0 || e.fb.shutdowns != 0 {
		t.Errorf("actions %q (at the first reboot %q), reboots from inside %d and %d", got, e.fb.actionsAtReboot, e.reboots, e.fb.shutdowns)
	}
	resetFakePower()
	e.fb = fakeFreeBSD{}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--no-reboot", "--yes")
	if code != 0 || !strings.Contains(errOut, "Installed; web-01 stays in the UMP FreeBSD rescue system, a reboot through the provider (ump server reboot web-01) starts freebsd-15.1.\n") || strings.Join(fakePower.actions, " ") != "bootdisk reboot:hard" {
		t.Errorf("--no-reboot: code %d, actions %q\n%s", code, fakePower.actions, errOut)
	}
}

// TestReinstallFreeBSDWithoutRescue: on a provider without a rescue
// system FreeBSD is installed the direct way, once: the question says
// that no reinstall by ump leaves it again.
func TestReinstallFreeBSDWithoutRescue(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	e.directRoot = true
	const sharper = "There is no rescue system to fall back on: a failed installation is yours to recover. FreeBSD cannot be left again from here: the next reinstall of this server is the provider's. [y/N]:"
	_, errOut, code := e.run("n\n", true, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision")
	if code != 3 || !strings.Contains(errOut, "as web-01? Every disk of the server is erased. "+sharper) || len(e.remoteCalls) != 0 {
		t.Errorf("the question: code %d, remote calls %d\n%s", code, len(e.remoteCalls), errOut)
	}
	// another system has the sentence of today alone
	_, errOut, _ = e.run("n\n", true, "server", "reinstall", "web-01", "--no-provision")
	if !strings.Contains(errOut, "a failed installation is yours to recover. [y/N]:") || strings.Contains(errOut, "cannot be left again") {
		t.Errorf("the question for Ubuntu:\n%s", errOut)
	}
	// allowed: the installation runs
	_, errOut, code = e.run("y\n", true, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision")
	if code != 0 || !strings.Contains(errOut, "web-01 is up: FreeBSD 15.1-RELEASE") || len(fakePower.actions) != 0 {
		t.Errorf("installation: code %d, actions %v\n%s", code, fakePower.actions, errOut)
	}
	// a FreeBSD rescue system that does not come up there: the way back is
	// the provider's
	oldTimeout := imageWaitTimeout
	imageWaitTimeout = 30 * time.Millisecond
	t.Cleanup(func() { imageWaitTimeout = oldTimeout })
	e.fb = fakeFreeBSD{noBoot: true}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes")
	if code != 5 || !strings.Contains(errOut, "freebsd-15.1 is not installed. Provider type norescue has no rescue system: the same reinstall again, or one with another profile, starts over once the provider reinstalled the server with a stock Linux, which ump asks it for. The installer of the next attempt shows the boot log") {
		t.Errorf("no FreeBSD rescue system: code %d\n%s", code, errOut)
	}

	// several servers: the question names those FreeBSD goes to
	e.fb = fakeFreeBSD{}
	e.machines = map[string]*fakeMachine{}
	plan := writePlan(t, "servers:\n  web-01:\n    profile: freebsd-15.1\n  db-01:\n")
	_, errOut, code = e.run("n\n", true, "server", "multiinstall", "--plan", plan, "--no-provision")
	if code != 3 || !strings.Contains(errOut, "Install on these 2 servers? Every disk of each is erased. web-01, db-01 have no rescue system to fall back on: a failed installation is yours to recover. FreeBSD cannot be left again from web-01: the next reinstall of this server is the provider's. [y/N]:") {
		t.Errorf("multiinstall's question: code %d\n%s", code, errOut)
	}
}

// TestFreeBSDCannotJump: a server that runs FreeBSD, or the UMP FreeBSD
// rescue system, lets UMP in and still has no way into the UMP rescue
// system by itself: ump says so and goes through the provider, for a
// reinstall to whatever system and for ump server rescue; --via direct
// and a provider without a rescue system end with exit status 5.
func TestFreeBSDCannotJump(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	e.fb.runs = true
	const cannot = "Checking root access to 203.0.113.10 over SSH ... yes (FreeBSD 15.1-RELEASE, as ump with sudo); the running system is FreeBSD, which cannot jump into the UMP rescue system.\n"

	// a reinstall with Ubuntu: the provider's way, and the installation
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--no-provision", "--yes", "--account", "one")
	if code != 0 || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" || len(e.entryCommands()) != 1 || strings.HasPrefix(e.entryCommands()[0], "sudo") {
		t.Fatalf("reinstall: code %d, actions %v, entries %q\n%s", code, fakePower.actions, e.entryCommands(), errOut)
	}
	inOrder(t, errOut, cannot+"Going through the provider's rescue system.\n", "Provider rescue system reached (Debian GNU/Linux 12 (bookworm)); loading the UMP rescue image from https://ump.zhurko.de/dl/rescue.\n",
		"Installing ubuntu-26.04 (", "web-01 is up: Ubuntu 26.04.1 LTS")
	// ... and with FreeBSD again
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("reinstall with FreeBSD: code %d\n%s", code, errOut)
	}
	inOrder(t, errOut, cannot+"Going through the provider's rescue system.\n", "==> staged:", "web-01 is up: FreeBSD 15.1-RELEASE")

	// --via direct has no way: exit status 5, nothing touched
	resetFakePower()
	e.remoteCalls = nil
	for _, args := range [][]string{
		{"server", "reinstall", "web-01", "--no-provision", "--yes", "--via", "direct", "--account", "one"},
		{"server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes", "--via", "direct", "--account", "one"},
		{"server", "rescue", "web-01", "--yes", "--via", "direct", "--account", "one"},
	} {
		_, errOut, code := e.run("", false, args...)
		if code != 5 || !strings.HasSuffix(errOut, cannot+"ump: the running system of web-01 is FreeBSD, and no kexec leads from FreeBSD into the UMP rescue system: --via direct has no way in. --via provider, or no --via, goes through the provider's rescue system. Nothing was changed\n") {
			t.Errorf("%v: code %d\n%s", args, code, errOut)
		}
	}
	if len(fakePower.actions) != 0 || len(e.entryCommands()) != 0 || len(e.installs) != 2 {
		t.Errorf("--via direct touched the server: actions %v, entries %v, installs %d", fakePower.actions, e.entryCommands(), len(e.installs))
	}

	// ump server rescue: the provider's way too, into the UMP rescue
	// system, which is Linux
	out, errOut, code := e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 0 || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard" || !strings.Contains(out, "Connect: ssh ") {
		t.Errorf("rescue: code %d, actions %v\n%s\n%s", code, fakePower.actions, out, errOut)
	}
	inOrder(t, errOut, cannot+"Going through the provider's rescue system.\n", "Waiting for the UMP rescue system on 203.0.113.10 (up to 15m) ... up after 0s (ump-rescue 2026.10.12).\n")

	// a server left in the UMP FreeBSD rescue system, by an installation
	// that stopped there or by --no-reboot: the same
	resetFakePower()
	e.fb = fakeFreeBSD{inRescue: true}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--no-provision", "--yes", "--account", "one")
	if code != 0 || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" {
		t.Fatalf("from the FreeBSD rescue system: code %d, actions %v\n%s", code, fakePower.actions, errOut)
	}
	inOrder(t, errOut, "Checking root access to 203.0.113.10 over SSH ... web-01 runs the UMP FreeBSD rescue system 2026.11.26, which cannot jump into the UMP rescue system.\nGoing through the provider's rescue system.\n",
		"Provider rescue system reached", "==> staged:", "web-01 is up: FreeBSD 15.1-RELEASE")
	// the provider's reset starts the disk again: the FreeBSD rescue system
	// answers where the provider's should, as root with UMP's key
	resetFakePower()
	e.fb = fakeFreeBSD{inRescue: true, keepsBooting: true}
	entries := len(e.entryCommands())
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--account", "one")
	if code != 5 || !strings.HasSuffix(errOut, "ump: after the reset FreeBSD answers at 203.0.113.10, not the provider's rescue system: web-01 started from its disks again, and no kexec leads from FreeBSD into the UMP rescue system. Look at the server at its provider (ump server describe web-01, its console) and run this again\n") || len(e.entryCommands()) != entries {
		t.Errorf("the disk again: code %d\n%s", code, errOut)
	}
	e.fb.keepsBooting = false
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "rescue", "web-01", "--yes", "--via", "direct", "--account", "one")
	if code != 5 || !strings.HasSuffix(errOut, "ump: web-01 runs the UMP FreeBSD rescue system 2026.11.26, and no kexec leads from FreeBSD into the UMP rescue system: --via direct has no way in. --via provider, or no --via, goes through the provider's rescue system. Nothing was changed\n") {
		t.Errorf("rescue --via direct from the FreeBSD rescue system: code %d\n%s", code, errOut)
	}

	// a provider without a rescue system: exit status 5 and why, for
	// rescue, a dry run and a keep-data installation, none of which may
	// take the provider's reinstall; a reinstall takes it
	e2 := newTestEnv(t)
	e2.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e2.writeProfiles()
	resetFakePower()
	e2.fb.runs = true
	const noWay = "ump: the running system of web-01 is FreeBSD, and no kexec leads from FreeBSD into the UMP rescue system; provider type norescue has no rescue system, so ump has no way in. Leaving FreeBSD on this server is the provider's: its console, its own rescue system, or its reinstall with a Linux, which ump reaches again. Nothing was changed\n"
	for _, args := range [][]string{
		{"server", "rescue", "web-01", "--yes", "--force"},
		{"server", "reinstall", "web-01", "--no-provision", "--yes", "--dry-run"},
		{"server", "reinstall", "web-01", "--no-provision", "--yes", "--keep-data"},
	} {
		_, errOut, code := e2.run("", false, args...)
		if code != 5 || !strings.HasSuffix(errOut, noWay) || len(fakePower.actions) != 0 || len(e2.entryCommands()) != 0 {
			t.Errorf("%v: code %d, actions %v\n%s", args, code, fakePower.actions, errOut)
		}
	}
	fakeStock = true
	defer func() { fakeStock = false }()
	e2.stockRoot = true
	_, errOut, code = e2.run("", false, "server", "reinstall", "web-01", "--no-provision", "--yes")
	if code != 0 || !strings.HasPrefix(strings.Join(fakePower.actions, ","), "stock:ump") {
		t.Fatalf("the stock way: code %d, actions %v\n%s", code, fakePower.actions, errOut)
	}
	inOrder(t, errOut, "the running system is FreeBSD, which cannot jump into the UMP rescue system.\n",
		"Provider type norescue has no rescue system and FreeBSD cannot jump into the UMP rescue system; the provider reinstalls web-01 with a stock Linux first",
		"web-01 runs the provider's Debian 13 now, with UMP's key for root; waiting for root access over SSH (up to 15m) ... yes", "web-01 is up: Ubuntu 26.04.1 LTS")

	// a provider that can neither: exit status 5 for a reinstall too
	e3 := newTestEnv(t)
	e3.write("accounts:\n  lo:\n    type: listonly\n    password: s3cret\n")
	e3.writeProfiles()
	resetFakePower()
	e3.fb.runs = true
	_, errOut, code = e3.run("", false, "server", "reinstall", "web-01", "--no-provision", "--yes")
	if code != 5 || !strings.Contains(errOut, "provider type listonly has no rescue system, so ump has no way in.") {
		t.Errorf("no rescue system, no stock image: code %d\n%s", code, errOut)
	}
}

// TestReinstallFreeBSDHostFileInAStorage: a host file in a storage takes
// none of Ansible's variables but the address, so that of a FreeBSD
// server names no Python there.
func TestReinstallFreeBSDHostFileInAStorage(t *testing.T) {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	e.directRoot = true
	store := filepath.Join(t.TempDir(), "fleet", "inventory")
	os.WriteFile(filepath.Join(e.repo, "inventory", "hosts.yml"), []byte("plugin: ofzhur.ansible.host\nurl: file://"+store+"\n"), 0o644)
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--profile", "freebsd-15.1", "--yes", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "Wrote file://"+store+"/web-01.yml: web-01 in common.\n") {
		t.Fatalf("reinstall: code %d\n%s", code, errOut)
	}
	object, _ := os.ReadFile(filepath.Join(store, "web-01.yml"))
	if !strings.Contains(string(object), "vars:\n  ansible_host: 203.0.113.10\n  ump_admin: ump\n") || !strings.Contains(string(object), "  ump_os: freebsd-15.1\n") || strings.Contains(string(object), "ansible_python_interpreter") {
		t.Errorf("host file in the storage:\n%s", object)
	}
}

// TestMultiinstallFreeBSD: a FreeBSD server among Linux ones is installed
// side by side with them, in its two steps.
func TestMultiinstallFreeBSD(t *testing.T) {
	e := multiEnv(t)
	plan := writePlan(t, "servers:\n  web-01:\n    group: web\n  db-01:\n    profile: freebsd-15.1\n    layout: zfs-mirror\n    group: [db]\n")
	out, errOut, code := e.run("", false, "server", "multiinstall", "--plan", plan, "--yes", "--account", "one")
	if code != 0 {
		t.Fatalf("multiinstall: code %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"web-01   fake/1     ubuntu-26.04   lvm-linear   web-01     10.0.0.1       common,web\ndb-01    fake/2     freebsd-15.1   zfs-mirror   db-01      10.0.0.2       common,db\n",
		"[db-01] ==> staged: the UMP FreeBSD rescue system is on the disks, with its boot config; a reboot starts it, and the installation goes on there\n",
		"[db-01] Rebooting db-01 into the UMP FreeBSD rescue system ...\n",
		"[db-01] Waiting for the UMP FreeBSD rescue system on 203.0.113.11 (up to 15m) ... up after 0s (ump-rescue 2026.11.26, FreeBSD 15.1-RELEASE).\n",
		"[db-01] Rebooting db-01 into freebsd-15.1 ...\n",
		"[db-01] db-01 is up: FreeBSD 15.1-RELEASE, hostname db-01, root on zroot/ROOT/default; log in as ump (ump ssh db-01).\n",
		"[web-01] web-01 is up: Ubuntu 26.04.1 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).\n",
		"Wrote inventory/hosts/db-01.yml: db-01 in common, db.\nWrote inventory/hosts/web-01.yml: web-01 in common, web.\nProvisioning: 00-base on the fleet (2 hosts) ...\n",
	} {
		if !strings.Contains(errOut, want) {
			t.Errorf("progress lacks %q:\n%s", want, errOut)
		}
	}
	// the installer for FreeBSD ran on db-01 alone, and no line of web-01
	// is of FreeBSD
	if len(e.fb.installs) != 1 || !slices.Equal(e.installsOn(), []string{"203.0.113.10", "203.0.113.11"}) && !slices.Equal(e.installsOn(), []string{"203.0.113.11", "203.0.113.10"}) {
		t.Errorf("installers: for FreeBSD %d, in the UMP rescue system on %v", len(e.fb.installs), e.installsOn())
	}
	for _, l := range installLines(errOut) {
		if strings.HasPrefix(l, "[web-01] ") && (strings.Contains(l, "FreeBSD rescue system") || strings.Contains(l, "freebsd-15.1")) {
			t.Errorf("web-01 got a line of FreeBSD's: %q", l)
		}
	}
	if db := e.specOf("203.0.113.11"); db["os"] != "freebsd-15.1" || db["layout"].(map[string]any)["name"] != "zfs-mirror" {
		t.Errorf("db-01's spec: %v", db)
	}
	st := e.readState()
	for _, want := range []string{"  fake/1:\n    installed:\n      os: ubuntu-26.04\n", "  fake/2:\n    installed:\n      os: freebsd-15.1\n      image_version: 15.1-RELEASE\n      kernel: 15.1-RELEASE\n      profile: freebsd-15.1\n      layout: zfs-mirror\n      hostname: db-01\n"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q:\n%s", want, st)
		}
	}
	if hf := e.hostFile("db-01.yml"); !strings.Contains(hf, "  ansible_python_interpreter: /usr/local/bin/python3\n") || strings.Contains(e.hostFile("web-01.yml"), "ansible_python_interpreter") {
		t.Errorf("host files:\n%s\n%s", hf, e.hostFile("web-01.yml"))
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "yes", "yes"},
		{"db-01", "db-01", "yes", "yes"},
	})

	// the FreeBSD server's second step fails: the other is installed and
	// provisioned, and the failure is the run's
	e.fb = fakeFreeBSD{exitAt: map[string]int{"203.0.113.11": 1}}
	out, errOut, code = e.run("", false, "server", "multiinstall", "--plan", plan, "--yes", "--account", "one")
	if code != 1 || !strings.Contains(errOut, "[db-01] ump: the installation on db-01 failed in the UMP FreeBSD rescue system (exit status 1, see above); the server stays there (ump ssh db-01 --staging), and a reboot finds no system on its disks; the same reinstall again starts over, through the provider's rescue system\n") ||
		!strings.Contains(errOut, "ump: the installation failed on db-01, see above\n") {
		t.Errorf("a failed second step: code %d\n%s", code, errOut)
	}
	rows := fields(out)
	if len(rows) != 3 || strings.Join(rows[1], " ") != "web-01 web-01 yes yes" || !strings.HasPrefix(strings.Join(rows[2], " "), "db-01 db-01 no - the installation on db-01 failed in the UMP FreeBSD rescue system") {
		t.Errorf("summary:\n%s", out)
	}
	// its private address stays its own: the disks were written
	if e.privateOf("fake/2") != "10.0.0.2" {
		t.Errorf("db-01's private address: %q", e.privateOf("fake/2"))
	}
}

// TestProbeFreeBSD: the probe tells FreeBSD by its kernel, and its
// rescue system from the UMP rescue system, which has the version file
// too.
func TestProbeFreeBSD(t *testing.T) {
	for _, c := range []struct {
		answer          string
		arch            string
		freebsd, rescue bool
	}{
		{"1001\n-\nFreeBSD 15.1-RELEASE\nsudo\namd64\nFreeBSD\n", "amd64", true, false},
		{"0\n2026.11.26\n\n-\narm64\nFreeBSD\n", "arm64", true, false},
		{"0\n2026.11.26\nDebian GNU/Linux 13 (trixie)\n-\nx86_64\nLinux\n", "amd64", false, true},
		{"0\n-\nUbuntu 26.04 LTS\n-\naarch64\nLinux\n", "arm64", false, false},
		// an answer of five lines, as before the kernel was asked
		{"0\n2026.10.12\nDebian GNU/Linux 13 (trixie)\n-\nx86_64\n", "amd64", false, true},
	} {
		a := &app{stderr: io.Discard, sshExec: func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
			if cmd := argv[len(argv)-1]; !strings.HasSuffix(cmd, "; uname -m; uname -s") {
				t.Errorf("probe: %s", cmd)
			}
			io.WriteString(stdout, c.answer)
			return 0, nil
		}}
		p, ok := a.probeHost(context.Background(), "root", "203.0.113.10", nil, false)
		if !ok || p.arch != c.arch || p.freebsd() != c.freebsd || p.rescue() != c.rescue {
			t.Errorf("%q: %+v, %v", c.answer, p, ok)
		}
	}
}

// TestFreeBSDShutDownAndReboot: where ump shuts a running system down or
// reboots it over SSH on the provider's way, a FreeBSD gets its own
// commands.
func TestFreeBSDShutDownAndReboot(t *testing.T) {
	// before the provider's reset of a keep-data installation
	e := keepEnv(t)
	e.fb.runs = true
	_, errOut, _ := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	inOrder(t, errOut, "the running system is FreeBSD, which cannot jump into the UMP rescue system.\nGoing through the provider's rescue system.\n",
		"Checking root access to 203.0.113.10 over SSH, to shut the running system down first ... yes; shutting it down as ump (up to 100ms) ... down after ",
		"Rescue system enabled for fake/1 (web-01, 203.0.113.10); rebooting into it.\n")
	if len(e.halts) != 1 || e.halts[0] != "ump: sudo -n sh -c 'shutdown -h now'" {
		t.Errorf("halts: %q", e.halts)
	}

	// before a PXE machine's network boot of the rescue system
	var asked []string
	var errOut2 strings.Builder
	a := &app{stderr: &errOut2, sshExec: func(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		user, _, _ := strings.Cut(argv[slices.Index(argv, "--")-1], "@")
		cmd := argv[len(argv)-1]
		switch {
		case user != "ump":
			return 255, nil
		case strings.HasPrefix(cmd, "id -u;"):
			io.WriteString(stdout, "1001\n-\nFreeBSD 15.1-RELEASE\nsudo\namd64\nFreeBSD\n")
			return 0, nil
		}
		asked = append(asked, cmd)
		return 255, nil
	}}
	if !a.rebootOverSSH(context.Background(), provider.Machine{Provider: "pxe", ID: "web-07", Name: "web-07", IPv4: "192.0.2.17"}, []string{"root", "ump"}, "/nonexistent/key") ||
		!slices.Equal(asked, []string{"sudo -n shutdown -r now"}) || errOut2.String() != "Checking root access to 192.0.2.17 over SSH, for the reboot ... yes; rebooting it as ump.\n" {
		t.Errorf("reboot over SSH: asked %q\n%s", asked, errOut2.String())
	}
}

// TestPXEServerReinstallFreeBSD: a PXE machine gets FreeBSD attended,
// like any server: the UMP FreeBSD rescue system answers where the UMP
// rescue system reported in, through the jump host, with host keys of
// its own; the new system at the machine's own address.
func TestPXEServerReinstallFreeBSD(t *testing.T) {
	e := setupFleetPXE(t)
	e.writeProfiles()
	startFakePXE(t, e, true)
	out, errOut, code := e.run("", false, "server", "reinstall", "pxe/web-07", "--profile", "freebsd-15.1", "--yes", "--no-provision")
	if code != 0 {
		t.Fatalf("reinstall: code %d\n%s\n%s", code, out, errOut)
	}
	inOrder(t, errOut, "Power-cycle web-07 now (or reset it): it boots UMP's rescue system at its next boot.\n", "==> staged:", "Rebooting web-07 into the UMP FreeBSD rescue system ...\n",
		"Waiting for the UMP FreeBSD rescue system on 192.0.2.50 (up to 15m) ... up after 0s (ump-rescue 2026.11.26, FreeBSD 15.1-RELEASE).\n", "Rebooting web-07 into freebsd-15.1 ...\n",
		"Waiting for SSH on 192.0.2.17 (up to 15m) ... up after ", "web-07 is up: FreeBSD 15.1-RELEASE, hostname web-07")
	var staging, reboot, poll, install, shutdown []string
	for _, argv := range e.remoteCalls {
		switch cmd := argv[len(argv)-1]; {
		case strings.Contains(cmd, "/install/install.sh"):
			staging = argv
		case cmd == "systemctl reboot":
			reboot = argv
		case cmd == freebsdRescueCommand:
			poll = argv
		case strings.Contains(cmd, "/install/freebsd/install.sh"):
			install = argv
		case cmd == "shutdown -r now":
			shutdown = argv
		}
	}
	// the UMP rescue system with the host key it reported pinned, the
	// FreeBSD one, which has new ones, without
	for name, argv := range map[string][]string{"the installer in the UMP rescue system": staging, "its reboot": reboot} {
		if !hasOptions(argv, "ProxyJump=ump@198.51.100.7", "StrictHostKeyChecking=yes") || slices.Contains(argv, "UserKnownHostsFile=/dev/null") || !slices.Contains(argv, "root@192.0.2.50") {
			t.Errorf("%s: %v", name, argv)
		}
	}
	for name, argv := range map[string][]string{"the poll": poll, "the installer for FreeBSD": install, "its reboot": shutdown} {
		if !hasOptions(argv, "ProxyJump=ump@198.51.100.7", "UserKnownHostsFile=/dev/null", "StrictHostKeyChecking=no") || !slices.Contains(argv, "root@192.0.2.50") {
			t.Errorf("%s: %v", name, argv)
		}
	}
	// pxe.yaml's uplink is the new system's, as the installer for FreeBSD
	// gets it
	var spec map[string]any
	json.Unmarshal([]byte(e.fb.specs[0]), &spec)
	if uplink, _ := json.Marshal(spec["network"].(map[string]any)["uplink"]); string(uplink) != `{"dns":["192.0.2.53"],"ipv4":{"address":"192.0.2.17/24","gateway":"192.0.2.1","mode":"static"},"ipv6":{"mode":"none"},"mac":"0c:c4:7a:aa:bb:01"}` {
		t.Errorf("uplink %s", uplink)
	}
	if st := e.readState(); !strings.Contains(st, "pxe/web-07:") || !strings.Contains(st, "os: freebsd-15.1") {
		t.Errorf("state:\n%s", st)
	}
	if hf := e.hostFile("web-07.yml"); !strings.Contains(hf, "  ansible_host: 192.0.2.17\n  ansible_python_interpreter: /usr/local/bin/python3\n  ansible_ssh_common_args: -o ProxyJump=ump@198.51.100.7 -o StrictHostKeyChecking=accept-new\n") {
		t.Errorf("host file:\n%s", hf)
	}
}

// TestUnattendedFreeBSD: FreeBSD's two steps are driven over SSH, so an
// unattended installation of it is refused before anything is armed.
func TestUnattendedFreeBSD(t *testing.T) {
	e := setupUnattended(t, fleetPXE)
	const refusal = "freebsd-15.1 is installed in two steps that ump drives over SSH, the second from its FreeBSD rescue system; an unattended installation of it is not in this version. Without --unattended, ump installs it on pxe/web-07 as on any server. web-07 was not touched\n"
	for _, args := range [][]string{
		{"server", "reinstall", "pxe/web-07", "--unattended", "--profile", "freebsd-15.1", "--yes", "--no-provision"},
		{"server", "reinstall", "pxe/web-07", "--unattended", "--profile", "freebsd-15.1", "--no-wait", "--yes"},
		{"server", "reinstall", "pxe/web-07", "--unattended", "--profile", "freebsd-15.1", "--dry-run", "--no-provision"},
		{"server", "multiinstall", "pxe/web-07", "--unattended", "--profile", "freebsd-15.1", "--yes", "--no-provision"},
	} {
		_, errOut, code := e.run("", false, args...)
		if code != 2 || !strings.HasSuffix(errOut, refusal) {
			t.Errorf("%v: code %d\n%s", args, code, errOut)
		}
	}
	if armOf(t, "web-07") != nil || len(e.remoteCalls) != 0 {
		t.Errorf("armed %+v, remote calls %d", armOf(t, "web-07"), len(e.remoteCalls))
	}
}

// TestFreeBSDKeylessRescue: where the provider's rescue system takes no
// key, the refusal of a server that runs FreeBSD does not send people to
// the direct way, which it does not have.
func TestFreeBSDKeylessRescue(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  kl:\n    type: keyless\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	e.fb.runs = true
	_, errOut, code := e.run("", false, "server", "rescue", "web-01", "--yes")
	if code != 5 || !strings.HasSuffix(errOut, "ump: the provider's rescue system of web-01 takes no SSH key (the password goes by mail), so UMP could not get in by itself, and the running system of web-01 is FreeBSD, from where no kexec leads into the UMP rescue system; use ump server rescue --no-image with the mailed password\n") || len(fakePower.actions) != 0 {
		t.Errorf("keyless: code %d, actions %v\n%s", code, fakePower.actions, errOut)
	}
}

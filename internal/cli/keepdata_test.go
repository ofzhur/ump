package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ofzhur/ump/internal/backend"
)

// keepFixture is a machine of the installer's keep-data test
// (images/testdata/keep-data/NAME/expected): the plan as the installer
// prints it, the plan file, and the refusal when it refuses the machine.
func keepFixture(t *testing.T, name string) (text, file, refused string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", "images", "testdata", "keep-data", name, "expected"))
	if err != nil {
		t.Fatal(err)
	}
	text, rest, _ := strings.Cut(string(data), "--- ")
	if _, after, ok := strings.Cut(string(data), "--- the plan (--plan)\n"); ok {
		file, _, _ = strings.Cut(after, "\n--- ")
	} else if !strings.HasPrefix(text, "refused: ") {
		t.Fatalf("%s: no plan in %q", name, rest)
	}
	if r, ok := strings.CutPrefix(text, "refused: "); ok {
		return "", file, strings.TrimSpace(r)
	}
	return text, file, ""
}

// keepHash is the hash of a fixture's plan.
func keepHash(t *testing.T, name string) string {
	t.Helper()
	_, file, _ := keepFixture(t, name)
	var plan struct {
		Hash string `json:"hash"`
	}
	if err := json.Unmarshal([]byte(file), &plan); err != nil || len(plan.Hash) != 64 {
		t.Fatalf("%s: hash %q, %v", name, plan.Hash, err)
	}
	return plan.Hash
}

// keepInstaller plays the installer with --keep-data on the machine at
// addr, as install/install.sh behaves: the plan run prints the plan of
// the fixture, leaves it as the plan file and writes nothing, or
// refuses with status 2; the run with --confirm installs when the hash
// is the plan's, and fails with status 2 before it writes or 1 after.
func (e *testEnv) keepInstaller(addr, cmd string) (stdout, stderr string, code int) {
	name := e.keepCase
	if at := e.keepCaseAt[addr]; at != "" {
		name = at
	}
	if name == "" {
		name = "lvm"
	}
	planning := strings.Contains(cmd, " --plan /tmp/ump-keep-data.json")
	if !planning && e.keepDrift != "" {
		name = e.keepDrift
	}
	text, file, refused := keepFixture(e.t, name)
	const unwritten = "install.sh: keep-data: nothing was written to the disks\n"
	out := "==> machine: arm64, UEFI firmware, 2 CPUs, 3915 MB RAM\n==> keep-data: looking for the old system (read-only: nothing is written)\n"
	if planning {
		if !strings.Contains(cmd, "rm -f /tmp/ump-keep-data.json; cat > /tmp/ump-install.json && ") {
			e.t.Errorf("the plan run leaves an older plan file: %q", cmd)
		}
		if e.keepFiles == nil {
			e.keepFiles = map[string]string{}
		}
		delete(e.keepFiles, addr)
		if !e.keepNoFile {
			e.keepFiles[addr] = file
		}
	}
	if refused != "" {
		return out, "install.sh: keep-data: refused: " + refused + "\n" + unwritten, 2
	}
	for _, line := range strings.Split(strings.TrimRight(text, "\n"), "\n") {
		out += "==> " + line + "\n"
	}
	hash := keepHash(e.t, name)
	out += "==> plan: " + hash + " (sha256; --confirm takes it)\n"
	if planning {
		return out + "==> plan file: /tmp/ump-keep-data.json\n==> dry run: stopping here, no disk was touched\n", "", 0
	}
	confirmed := ""
	if _, after, ok := strings.Cut(cmd, " --confirm "); ok {
		confirmed = strings.Fields(after)[0]
	}
	switch {
	case confirmed != hash:
		return out, "install.sh: keep-data: refused: the plan (above, " + hash + ") is not the one that was confirmed (" + confirmed + "): the disks or the options changed since; look at it again and confirm it\n" + unwritten, 2
	case e.keepExit == 2:
		return out, "install.sh: the image's checksum is not the manifest's\n" + unwritten, 2
	case e.keepExit != 0:
		return out + "==> keep-data: writing begins\n", "install.sh: line 2301: tar exited with status 2\n", e.keepExit
	}
	e.installed = true
	return out + "==> keep-data: writing begins\n==> installed: ubuntu-26.04 2026.10.12 (kernel 7.0.0-39-generic) as web-01, root on /dev/mapper/vg0-root (10240 MB; keep-data: 2 formatted, 4 kept, 1 aside, 0 untouched); reboot to start it\n", "", 0
}

// keepRuns are the installer's arguments after the specification in the
// keep-data runs, in order.
func (e *testEnv) keepRuns() []string {
	var out []string
	for _, cmd := range e.installs {
		if _, args, ok := strings.Cut(cmd, ` bash "$f" --spec /tmp/ump-install.json `); ok && strings.Contains(cmd, "--keep-data") {
			out = append(out, args)
		}
	}
	return out
}

func keepEnv(t *testing.T) *testEnv {
	e := newTestEnv(t)
	e.write(testConfig)
	e.writeProfiles()
	resetFakePower()
	return e
}

// touched says what happened to the server: the provider's actions, the
// kexec, the installer's runs.
func (e *testEnv) touched() bool {
	return len(fakePower.actions) != 0 || e.kexeced || len(e.installs) != 0
}

func TestKeepDataFlags(t *testing.T) {
	e := keepEnv(t)
	e.directRoot = true
	// refused before the server is touched, exit status 2
	for _, c := range []struct {
		args []string
		msg  string
	}{
		{[]string{"--keep-data", "--layout", "lvm-raid1"}, "--keep-data and --layout lvm-raid1: a keep-data installation applies no layout"},
		{[]string{"--keep-data", "--disks", "/dev/sda"}, "--keep-data and --disks /dev/sda: a keep-data installation chooses no disks"},
		{[]string{"--keep-data", "--unattended"}, "--keep-data and --unattended: a keep-data installation shows its plan and asks before it writes"},
		{[]string{"--root", "/dev/vg0/root"}, "--root, --keep-root and --format belong to --keep-data"},
		{[]string{"--keep-root"}, "--root, --keep-root and --format belong to --keep-data"},
		{[]string{"--format", "/var"}, "--root, --keep-root and --format belong to --keep-data"},
		{[]string{"--keep-data", "--format", "var"}, `--format "var": a mountpoint of the old system's fstab (/var, /srv/cache)`},
		{[]string{"--keep-data", "--format", "/var", "--format", "/var/"}, "--format /var is given twice"},
		{[]string{"--keep-data", "--keep-root", "--format", "/"}, "--format / and --keep-root contradict each other"},
		{[]string{"--keep-data", "--root", "two devices"}, `--root "two devices": the old root is a device`},
		{[]string{"--keep-data", "--profile", "nixos-25.11"}, "--keep-data installs the images unpacked onto the old system's volumes (Ubuntu, Debian); nixos-25.11 is of the family nixos"},
		{[]string{"--keep-data", "--profile", "archlinux"}, "archlinux is of the family arch, whose installation makes its own file systems, and is not supported with it yet. web-01 was not touched"},
		{[]string{"--keep-data", "--profile", "cachyos"}, "cachyos is of the family arch"},
		{[]string{"--keep-data", "--os", "nixos-25.11"}, "nixos-25.11 is of the family nixos"},
	} {
		args := append([]string{"server", "reinstall", "web-01", "--yes", "--account", "one"}, c.args...)
		if _, errOut, code := e.run("", false, args...); code != exitUsage || !strings.Contains(errOut, c.msg) {
			t.Errorf("%v: code %d, %q", c.args, code, errOut)
		}
		if e.touched() {
			t.Fatalf("%v: the server was touched: %v, %v", c.args, fakePower.actions, e.installs)
		}
	}

	// an image of another family by the storage's word
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/os/ubuntu-26.04/amd64/manifest.json", "/os/ubuntu-26.04/arm64/manifest.json":
			io.WriteString(w, `{"family": "image"}`)
		case "/os/void/amd64/manifest.json":
			io.WriteString(w, `{"family": "arch"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer storage.Close()
	e.write(testConfig + "storage: " + storage.URL + "\n")
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--keep-data", "--os", "void"); code != exitUsage || !strings.Contains(errOut, "void is of the family arch") || e.touched() {
		t.Errorf("a family by the manifest: code %d, %q", code, errOut)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--keep-data", "--dry-run"); code != 0 {
		t.Errorf("an image by the manifest: code %d, %q", code, errOut)
	}
}

func TestKeepDataReinstall(t *testing.T) {
	e := keepEnv(t)
	hash := keepHash(t, "lvm")
	profile := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles", "ubuntu-26.04.yaml")
	what := "ubuntu-26.04 (profile ubuntu-26.04 from " + profile + ", keeping the data, private address 10.0.0.1 in default)"

	// two questions: the way into the rescue system, then the plan
	out, errOut, code := e.run("y\ny\n", true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != 0 {
		t.Fatalf("keep-data: code %d, %q\n%s", code, errOut, out)
	}
	wants := []string{
		"Reinstall fake/1 (web-01, 203.0.113.10) with " + what + " as web-01? Its data is kept: no disk is wiped. The running system goes down and the server boots the rescue system, where the installer finds the old system and plans what it formats and what it keeps; nothing is written to a disk until you confirm that plan. Then ump provision runs 00-base on the fleet. [y/N]: ",
		"Keep-data plan for " + what + " on web-01 (the installer looks, and writes nothing):\n",
		"==> keep-data: looking for the old system (read-only: nothing is written)\n",
		"==> keep-data: the old system Debian GNU/Linux 12 (bookworm) (old.example.net) on /dev/mapper/vg0-root; UEFI firmware; the layout of the profile is not applied, no disk is wiped or partitioned\n",
		"==> FORMAT (what is on them is lost)\n==>   /dev/mapper/vg0-root  ext4   /  (10G)\n==>   /dev/vda2             ext4   /boot  (1G)\n==> KEEP (as they are, mounted where they were)\n",
		"==> ASIDE (kept, under another name)\n==>   /dev/mapper/vg0-var   ext4   /var -> /var.old  (mounted)\n==> LOST (on the old root, beyond the system itself)\n",
		"==> plan: " + hash + " (sha256; --confirm takes it)\n==> plan file: /tmp/ump-keep-data.json\n==> dry run: stopping here, no disk was touched\n" +
			"Install ubuntu-26.04 on web-01 as the plan above says (plan " + hash[:12] + ")? What it lists under FORMAT and LOST is lost. [y/N]: " +
			"Installing " + what + " on web-01 as web-01, as planned (plan " + hash[:12] + ") ...\nProvisioning after the first boot: 00-base on the fleet.\n",
		"==> keep-data: writing begins\n==> installed: ubuntu-26.04 2026.10.12",
		"Rebooting web-01 into ubuntu-26.04 ...\n",
		"web-01 is up: Ubuntu 26.04.1 LTS, hostname web-01, root on /dev/mapper/vg0-root; log in as ump (ump ssh web-01).\n",
		"web-01 is provisioned.",
	}
	at := 0
	for _, want := range wants {
		i := strings.Index(errOut[at:], want)
		if i < 0 {
			t.Fatalf("progress lacks, in this order, %q:\n%s", want, errOut)
		}
		at += i + len(want)
	}
	// the installer ran twice with the same options: the plan, then the
	// installation with the plan's hash
	runs := e.keepRuns()
	if len(runs) != 2 || runs[0] != "--keep-data --plan /tmp/ump-keep-data.json" || runs[1] != "--keep-data --confirm "+hash {
		t.Errorf("installer runs: %q", runs)
	}
	// the specification names no disks; the layout is the profile's, for
	// the root's file system
	spec := e.lastSpec()
	if _, has := spec["disks"]; has || spec["layout"].(map[string]any)["name"] != "lvm-linear" || spec["keep_data"] != nil {
		t.Errorf("spec: disks %v, layout %v", spec["disks"], spec["layout"])
	}
	if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard,unrescue" {
		t.Errorf("provider actions: %s", got)
	}
	// the record: the layout is keep-data, with the old root and the plan
	st := e.readState()
	if want := "      layout: keep-data\n      hostname: web-01\n      admin: ump\n      keep_data:\n        root: /dev/mapper/vg0-root\n        plan: " + hash + "\n      at: "; !strings.Contains(st, want) {
		t.Errorf("state lacks %q:\n%s", want, st)
	}
	if strings.Contains(st, "keep_root") || !strings.Contains(st, "version: 3\n") {
		t.Errorf("state: %s", st)
	}
	if out, errOut, code := e.run("", false, "server", "show", "web-01", "--account", "one"); code != 0 || !strings.Contains(out, "(profile ubuntu-26.04, layout keep-data, administrator ump)") || !strings.Contains(out, "  Keep-data:  the old system's volumes kept, its root /dev/mapper/vg0-root made anew; plan "+hash+"\n") {
		t.Errorf("show: code %d, %q\n%s", code, errOut, out)
	}
	out, _, _ = e.run("", false, "server", "list", "-o", "wide", "--account", "one")
	if rows := fields(out); len(rows) != 3 || rows[2][3] != "web-01" || rows[2][11] != "keep-data" {
		t.Errorf("list -o wide: %v", rows)
	}
	if out, _, _ := e.run("", false, "server", "list", "-o", "json", "--account", "one"); !strings.Contains(out, `"layout": "keep-data"`) {
		t.Errorf("list -o json: %s", out)
	}
	if !strings.Contains(e.hostFile("web-01.yml"), "ump_os: ubuntu-26.04\n") {
		t.Errorf("host file: %s", e.hostFile("web-01.yml"))
	}

	// --keep-root and --format reach both runs; the record says keep_root
	resetFakePower()
	e.keepCase = "lvm-keep-root"
	kept := keepHash(t, "lvm-keep-root")
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--keep-root", "--format", "/srv/plain/", "--format", "/data", "--root", "/dev/vg0/root", "--yes", "--no-provision", "--account", "one")
	if code != 0 {
		t.Fatalf("--keep-root: code %d, %q", code, errOut)
	}
	if runs := e.keepRuns(); len(runs) != 4 || runs[2] != "--keep-data --root /dev/vg0/root --keep-root --format /srv/plain --format /data --plan /tmp/ump-keep-data.json" || runs[3] != "--keep-data --root /dev/vg0/root --keep-root --format /srv/plain --format /data --confirm "+kept {
		t.Errorf("installer runs: %q", e.keepRuns())
	}
	if st := e.readState(); !strings.Contains(st, "      keep_data:\n        root: /dev/mapper/vg0-root\n        keep_root: true\n        plan: "+kept+"\n") {
		t.Errorf("state: %s", st)
	}
	if out, _, _ := e.run("", false, "server", "show", "web-01", "--account", "one"); !strings.Contains(out, "  Keep-data:  the old system's volumes kept, the old system itself moved aside on its root /dev/mapper/vg0-root; plan "+kept+"\n") {
		t.Errorf("show: %s", out)
	}

	// an installation that wipes the disks takes the record's place
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	if st := e.readState(); strings.Contains(st, "keep_data") || !strings.Contains(st, "layout: lvm-linear\n") {
		t.Errorf("state after an ordinary reinstall: %s", st)
	}
}

func TestKeepDataDeclined(t *testing.T) {
	e := keepEnv(t)
	// no at the first question: nothing happened
	_, errOut, code := e.run("n\n", true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != exitAborted || !strings.Contains(errOut, "ump: aborted") || e.touched() {
		t.Errorf("no at the first question: code %d, touched %v, %q", code, e.touched(), errOut)
	}
	// without a terminal and --yes: no question, nothing happened
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != exitAborted || !strings.Contains(errOut, "needs confirmation") || e.touched() {
		t.Errorf("no terminal: code %d, %q", code, errOut)
	}

	// no at the second: the plan run alone, the server stays in the rescue
	// system, the provider's rescue system armed, and its private address
	// is free again
	for _, answer := range []string{"y\nn\n", "y\n", "y\nq\n"} {
		resetFakePower()
		e.installs = nil
		_, errOut, code = e.run(answer, true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
		if code != exitAborted || !strings.Contains(errOut, "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server unrescue web-01 --reboot starts the old system again.\n") {
			t.Errorf("%q: code %d, %q", answer, code, errOut)
		}
		if runs := e.keepRuns(); len(runs) != 1 || !strings.HasSuffix(runs[0], "--plan /tmp/ump-keep-data.json") || e.installed || e.reboots != 0 {
			t.Errorf("%q: installer runs %q, installed %v, reboots %d", answer, runs, e.installed, e.reboots)
		}
		if got := strings.Join(fakePower.actions, ","); got != "rescue,reboot:hard" {
			t.Errorf("%q: provider actions %s", answer, got)
		}
		if st := e.readState(); strings.Contains(st, "installed") || strings.Contains(st, "10.0.0.1") {
			t.Errorf("%q: state: %s", answer, st)
		}
	}

	// from the running system (a kexec), the provider's rescue system was
	// never armed: a reboot is the way back
	resetFakePower()
	e.directRoot = true
	_, errOut, code = e.run("y\nn\n", true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != exitAborted || !strings.Contains(errOut, "which stays in the UMP rescue system (ump ssh web-01 --staging). ump server reboot web-01 --hard starts the old system again.\n") || len(fakePower.actions) != 0 {
		t.Errorf("direct: code %d, actions %v, %q", code, fakePower.actions, errOut)
	}
}

func TestKeepDataDryRun(t *testing.T) {
	e := keepEnv(t)
	hash := keepHash(t, "lvm")
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--dry-run", "--yes", "--account", "one", "-o", "json")
	if code != 0 {
		t.Fatalf("--dry-run: code %d, %q", code, errOut)
	}
	if runs := e.keepRuns(); len(runs) != 1 || runs[0] != "--keep-data --plan /tmp/ump-keep-data.json" || e.installed || e.reboots != 0 {
		t.Errorf("installer runs: %q", runs)
	}
	if !strings.Contains(errOut, "==> FORMAT (what is on them is lost)\n") || !strings.Contains(errOut, "Dry run done; nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server unrescue web-01 --reboot starts the old system again.\n") {
		t.Errorf("progress: %q", errOut)
	}
	// the result carries the installer's plan
	var res struct {
		Name     string `json:"name"`
		Layout   string `json:"layout"`
		DryRun   bool   `json:"dry_run"`
		Via      string `json:"via"`
		KeepData struct {
			Version int    `json:"version"`
			OS      string `json:"os"`
			Hash    string `json:"hash"`
			Root    struct {
				Device string `json:"device"`
				By     string `json:"by"`
			} `json:"root"`
			Format []struct {
				Device     string `json:"device"`
				Mountpoint string `json:"mountpoint"`
			} `json:"format"`
			Aside []struct {
				From       string `json:"from"`
				Mountpoint string `json:"mountpoint"`
			} `json:"aside"`
			Lost []struct {
				Path string `json:"path"`
				MB   int    `json:"mb"`
			} `json:"lost"`
		} `json:"keep_data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	k := res.KeepData
	if res.Name != "web-01" || res.Layout != "keep-data" || !res.DryRun || res.Via != "provider" || k.Version != 1 || k.Hash != hash || k.Root.Device != "/dev/mapper/vg0-root" || k.Root.By != "detection" ||
		len(k.Format) != 2 || k.Format[1].Mountpoint != "/boot" || len(k.Aside) != 1 || k.Aside[0].Mountpoint != "/var.old" || len(k.Lost) != 9 || k.Lost[8].MB != 8790 {
		t.Errorf("result: %+v", res)
	}
	// nothing recorded, no address reserved
	if st := e.readState(); strings.Contains(st, "installed") || strings.Contains(st, "10.0.0.1") {
		t.Errorf("state: %s", st)
	}
	// as YAML
	out, _, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--dry-run", "--yes", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.Contains(out, "layout: keep-data\n") || !strings.Contains(out, "keep_data:\n") || !strings.Contains(out, "\n  hash: "+hash+"\n") {
		t.Errorf("yaml: code %d\n%s", code, out)
	}

	// an installation carries the plan too
	resetFakePower()
	out, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one", "-o", "json")
	if code != 0 || !strings.Contains(out, `"hash": "`+hash+`"`) || !strings.Contains(out, `"layout": "keep-data"`) || !strings.Contains(out, `"rebooted": true`) {
		t.Errorf("installed, json: code %d, %q\n%s", code, errOut, out)
	}
}

func TestKeepDataYes(t *testing.T) {
	e := keepEnv(t)
	// --yes takes a plan whose old system the installer found by itself
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != 0 || len(e.keepRuns()) != 2 || !e.booted || strings.Contains(errOut, "[y/N]") {
		t.Errorf("--yes: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}

	// and one whose old system --root names, among several
	resetFakePower()
	e.installs, e.keepCase = nil, "two-systems-root"
	hash := keepHash(t, "two-systems-root")
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--root", "/dev/sda3", "--yes", "--no-provision", "--account", "one")
	if code != 0 || !strings.Contains(errOut, "==> keep-data: the old system CentOS Linux 7 (Core) (two) on /dev/sda3 (--root); UEFI firmware") {
		t.Errorf("--yes --root: code %d, %q", code, errOut)
	}
	if runs := e.keepRuns(); len(runs) != 2 || runs[1] != "--keep-data --root /dev/sda3 --confirm "+hash {
		t.Errorf("--yes --root: installer runs %q", runs)
	}
	if st := e.readState(); !strings.Contains(st, "      keep_data:\n        root: /dev/sda3\n        plan: "+hash+"\n") {
		t.Errorf("state: %s", st)
	}

	// a plan made by an option nobody gave is not --yes's to take
	resetFakePower()
	e.installs = nil
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != exitAborted || len(e.keepRuns()) != 1 || !strings.Contains(errOut, "ump: --yes accepts a keep-data plan whose old system the installer found by itself, or that --root names; the plan for web-01 has its root on /dev/sda3 by option") || !strings.Contains(errOut, "Nothing was written to the disks of web-01") {
		t.Errorf("--yes, by option: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}

	// several old systems: the installer refuses, with or without --yes,
	// and ump names them for --root; nothing was written
	for _, args := range [][]string{{"--yes"}, nil} {
		resetFakePower()
		e.installs, e.keepCase = nil, "two-systems"
		_, errOut, code = e.run("y\ny\n", args == nil, append([]string{"server", "reinstall", "web-01", "--keep-data", "--no-provision", "--account", "one"}, args...)...)
		if code != exitAborted || e.installed || e.reboots != 0 {
			t.Errorf("%v, several systems: code %d, installed %v", args, code, e.installed)
		}
		for _, want := range []string{
			"install.sh: keep-data: refused: several old systems are on this machine: /dev/sda2 (Debian GNU/Linux 12 (bookworm)), /dev/sda3 (CentOS Linux 7 (Core)); --root DEVICE names the one to replace\n",
			"ump: the installer refused to keep the data of web-01 (exit status 2, see above). --root names the old system to replace: /dev/sda2 (Debian GNU/Linux 12 (bookworm), one), /dev/sda3 (CentOS Linux 7 (Core), two). Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n",
		} {
			if !strings.Contains(errOut, want) {
				t.Errorf("%v, several systems: %q lacks %q", args, errOut, want)
			}
		}
		if runs := e.keepRuns(); len(runs) != 1 {
			t.Errorf("%v, several systems: installer runs %q", args, runs)
		}
		if st := e.readState(); strings.Contains(st, "10.0.0.1") && !strings.Contains(st, "installed") {
			t.Errorf("%v: the private address stayed: %s", args, st)
		}
	}

	// a refusal without old systems to choose from (an encrypted root)
	resetFakePower()
	e.keepCase = "luks-root"
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != exitAborted || !strings.Contains(errOut, "install.sh: keep-data: refused: ") || !strings.Contains(errOut, "ump: the installer refused to keep the data of web-01 (exit status 2, see above). Nothing was written to the disks of web-01") {
		t.Errorf("encrypted root: code %d, %q", code, errOut)
	}
}

func TestKeepDataInstallerStatus(t *testing.T) {
	e := keepEnv(t)
	run := func() (string, int) {
		resetFakePower()
		e.installs = nil
		_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
		return errOut, code
	}
	// status 2 from the run that installs: nothing was written, the
	// address goes back, exit status 3
	e.keepExit = 2
	errOut, code := run()
	if code != exitAborted || !strings.Contains(errOut, "ump: the installer stopped before it wrote anything (exit status 2, see above). Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n") || e.reboots != 0 {
		t.Errorf("status 2: code %d, %q", code, errOut)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
		t.Errorf("status 2: state %s", st)
	}
	// status 1: writing began; the data volumes are untouched, the same
	// command recovers; the address is the server's, exit status 1
	e.keepExit = 1
	errOut, code = run()
	if code != exitGeneric || !strings.Contains(errOut, "==> keep-data: writing begins\n") || !strings.Contains(errOut, "ump: the installation on web-01 failed after the installer began to write (exit status 1, see above): the old root is gone or moved aside, the data volumes are untouched. The server stays in the UMP rescue system (ump ssh web-01 --staging); run the same command again: the installer finds what it saved of the old system's fstab on the new root, plans anew, and ump asks again\n") || e.reboots != 0 {
		t.Errorf("status 1: code %d, %q", code, errOut)
	}
	if st := e.readState(); !strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
		t.Errorf("status 1: state %s", st)
	}
	e.keepExit = 0

	// the machine changed between the plan and the installation: the
	// installer's plan has another hash, and it writes nothing
	e.keepDrift = "lvm-format"
	errOut, code = run()
	if code != exitAborted || !strings.Contains(errOut, "install.sh: keep-data: refused: the plan (above, "+keepHash(t, "lvm-format")+") is not the one that was confirmed ("+keepHash(t, "lvm")+")") || !strings.Contains(errOut, "ump: the installer stopped before it wrote anything") {
		t.Errorf("another plan: code %d, %q", code, errOut)
	}
	e.keepDrift = ""

	// no plan file to read back: no installation
	e.keepNoFile = true
	errOut, code = run()
	if code != exitGeneric || len(e.keepRuns()) != 1 || !strings.Contains(errOut, "ump: the installer's plan for web-01 could not be read back from the rescue system (/tmp/ump-keep-data.json): cat: /tmp/ump-keep-data.json: No such file or directory. Nothing was written to the disks of web-01") {
		t.Errorf("no plan file: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}
	e.keepNoFile = false

	// the installer cannot be fetched
	e.installNoScript = true
	errOut, code = run()
	if code != exitGeneric || !strings.Contains(errOut, "could not fetch the installer from https://ump.zhurko.de/dl/install/install.sh (exit status 22)") {
		t.Errorf("no installer: code %d, %q", code, errOut)
	}
	e.installNoScript = false

	// all well again; and with --no-reboot: recorded, in the rescue system
	errOut, code = run()
	if code != 0 || !strings.Contains(errOut, "web-01 is up") {
		t.Errorf("after the failures: code %d, %q", code, errOut)
	}
	resetFakePower()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-reboot", "--account", "one")
	if code != 0 || e.reboots != 0 || !strings.Contains(errOut, "Installed; web-01 stays in the UMP rescue system, a reboot starts ubuntu-26.04.") || !strings.Contains(e.readState(), "layout: keep-data\n") {
		t.Errorf("--no-reboot: code %d, reboots %d, %q", code, e.reboots, errOut)
	}
}

// TestKeepDataNeverTakesTheStockWay: a provider's reinstall with a
// stock Linux is a way into the rescue system for an installation that
// wipes the disks, and none for one that keeps them.
func TestKeepDataNeverTakesTheStockWay(t *testing.T) {
	e := newTestEnv(t)
	e.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	fakeStock = true
	defer func() { fakeStock = false }()
	e.stockRoot = true
	_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision")
	if code != exitUnreachable || len(fakePower.actions) != 0 || len(e.installs) != 0 {
		t.Fatalf("stock way: code %d, actions %v, %q", code, fakePower.actions, errOut)
	}
	if want := "ump: no root access to 203.0.113.10 over SSH, and provider type norescue has no rescue system: the only other way in is the provider's reinstall with a stock Linux, which erases the disks that --keep-data keeps. ump keeps the data of such a server through its running system alone: let UMP in there as root or a sudo user (with UMP's key, ump config key show, or one of yours) and run this again. Nothing was changed\n"; !strings.Contains(errOut, want) {
		t.Errorf("stock way: %q", errOut)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.1") {
		t.Errorf("the private address stayed: %s", st)
	}
	// the same where the provider's rescue system takes no key
	e = keepEnv(t)
	fakeKeyless = true
	defer func() { fakeKeyless = false }()
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != exitUnreachable || len(fakePower.actions) != 0 || len(e.installs) != 0 || !strings.Contains(errOut, "ump: no root access to 203.0.113.10 over SSH, and the provider's rescue system of web-01 takes no SSH key: the only other way in is the provider's reinstall with a stock Linux, which erases the disks that --keep-data keeps.") {
		t.Errorf("keyless rescue system: code %d, actions %v, %q", code, fakePower.actions, errOut)
	}
	fakeKeyless = false

	// through its running system it works, and a reboot is the way back
	e = newTestEnv(t)
	e.write("accounts:\n  nr:\n    type: norescue\n    password: s3cret\n")
	e.writeProfiles()
	resetFakePower()
	e.directRoot = true
	_, errOut, code = e.run("y\nn\n", true, "server", "reinstall", "web-01", "--keep-data", "--no-provision")
	if code != exitAborted || !strings.Contains(errOut, "There is no rescue system to fall back on: a failed installation is yours to recover.") || !strings.Contains(errOut, "ump server reboot web-01 --hard starts the old system again.\n") {
		t.Errorf("direct, declined: code %d, %q", code, errOut)
	}
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision")
	if code != 0 || len(fakePower.actions) != 0 || len(e.keepRuns()) != 3 {
		t.Errorf("direct: code %d, actions %v, runs %q, %q", code, fakePower.actions, e.keepRuns(), errOut)
	}
}

func TestKeepDataMultiinstall(t *testing.T) {
	e := multiEnv(t)
	hash := keepHash(t, "lvm")
	// a plan with one server that keeps its data and one that does not:
	// the first plans, a second question is about its plan, and only then
	// the other's installation starts
	plan := writePlan(t, `servers:
  web-01:
    keep-data: true
    format: /data
  db-01:
    hostname: db-01.example.net
`)
	var order []string
	e.remoteGate = func(addr string, argv []string) {
		if cmd := argv[len(argv)-1]; strings.Contains(cmd, "/install/install.sh") || strings.Contains(cmd, "/kexec-into-rescue.sh") {
			e.mu.Lock()
			order = append(order, addr)
			e.mu.Unlock()
		}
	}
	e.keepCase = "lvm"
	out, errOut, code := e.run("y\ny\n", true, "server", "multiinstall", "--plan", plan, "--no-provision", "--account", "one")
	if code != 0 {
		t.Fatalf("multiinstall: code %d, %q\n%s", code, errOut, out)
	}
	at := 0
	for _, want := range []string{
		"web-01   fake/1     ubuntu-26.04   keep-data    web-01              10.0.0.1       common\ndb-01    fake/2     ubuntu-26.04   lvm-linear   db-01.example.net   10.0.0.2       common\n",
		"Install on these 2 servers? Every disk of each is erased, but for web-01 (keep-data): there the installer plans first, in the rescue system, and nothing is written to a disk until you confirm the plans; the other installations start after that. [y/N]: ",
		"Keep-data plans for web-01, all at once ...\n",
		"[web-01] ==> FORMAT (what is on them is lost)\n",
		"[web-01] ==> dry run: stopping here, no disk was touched\n",
		"Install ubuntu-26.04 on web-01 as the plan above says (plan " + hash[:12] + ")? What it lists under FORMAT and LOST is lost. [y/N]: ",
		"Installing 2 servers, all at once ...\n",
	} {
		i := strings.Index(errOut[at:], want)
		if i < 0 {
			t.Fatalf("progress lacks, in this order, %q:\n%s", want, errOut)
		}
		at += i + len(want)
	}
	for _, want := range []string{"[web-01] ==> keep-data: writing begins\n", "[web-01] web-01 is up: ", "[db-01] db-01 is up: "} {
		if !strings.Contains(errOut[at:], want) {
			t.Errorf("after the second question, progress lacks %q:\n%s", want, errOut[at:])
		}
	}
	// db-01 was not touched before web-01 had its plan
	if len(order) < 4 || strings.Join(order[:2], " ") != "203.0.113.10 203.0.113.10" {
		t.Errorf("order: %v", order)
	}
	if runs := e.keepRuns(); len(runs) != 2 || runs[0] != "--keep-data --format /data --plan /tmp/ump-keep-data.json" || runs[1] != "--keep-data --format /data --confirm "+hash {
		t.Errorf("installer runs: %q", runs)
	}
	st := e.readState()
	for _, want := range []string{"      layout: keep-data\n      hostname: web-01\n", "      keep_data:\n        root: /dev/mapper/vg0-root\n        plan: " + hash + "\n", "      layout: lvm-linear\n      hostname: db-01.example.net\n"} {
		if !strings.Contains(st, want) {
			t.Errorf("state lacks %q:\n%s", want, st)
		}
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "yes", "-"},
		{"db-01", "db-01.example.net", "yes", "-"},
	})

	// no at the second question: nothing was written anywhere, the other
	// server was not touched, no lock is left and the addresses are free
	os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"))
	order, e.installs = nil, nil
	out, errOut, code = e.run("y\nn\n", true, "server", "multiinstall", "--plan", plan, "--no-provision", "--account", "one")
	if code != exitAborted || out != "" || !strings.Contains(errOut, "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server reboot web-01 --hard starts the old system again.\nump: aborted\n") {
		t.Errorf("no at the second question: code %d, %q\n%s", code, errOut, out)
	}
	if strings.Contains(strings.Join(order, " "), "203.0.113.11") || len(e.keepRuns()) != 1 {
		t.Errorf("no at the second question: touched %v, installer runs %q", order, e.keepRuns())
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.") || strings.Contains(st, "installed") {
		t.Errorf("no at the second question: state %s", st)
	}
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	if locks, err := b.Locks(context.Background()); err != nil || len(locks) != 0 {
		t.Errorf("locks left: %+v, %v", locks, err)
	}

	// both keep their data, one at a time; the installer refuses one,
	// which fails alone; one question is not asked with --yes; the result
	// carries the plan
	order, e.installs = nil, nil
	e.keepCaseAt = map[string]string{"203.0.113.11": "two-systems"}
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--keep-data", "--parallel", "1", "--yes", "--no-provision", "--account", "one", "-o", "json")
	if code != exitGeneric || !strings.Contains(errOut, "Keep-data plans for web-01, db-01, 1 at a time ...\n") || !strings.Contains(errOut, "[db-01] ump: the installer refused to keep the data of db-01 (exit status 2, see above). --root names the old system to replace: /dev/sda2 (Debian GNU/Linux 12 (bookworm), one), /dev/sda3 (CentOS Linux 7 (Core), two). Nothing was written to the disks of db-01") || !strings.Contains(errOut, "ump: the installation failed on db-01, see above\n") {
		t.Errorf("one refused: code %d, %q", code, errOut)
	}
	if strings.Contains(errOut, "[y/N]") {
		t.Errorf("questions with --yes: %q", errOut)
	}
	if locks, err := b.Locks(context.Background()); err != nil || len(locks) != 0 {
		t.Errorf("locks left: %+v, %v", locks, err)
	}
	var res struct {
		Servers []struct {
			Name      string `json:"name"`
			Layout    string `json:"layout"`
			Installed bool   `json:"installed"`
			Reason    string `json:"reason"`
			KeepData  *struct {
				Hash string `json:"hash"`
			} `json:"keep_data"`
		} `json:"servers"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil || len(res.Servers) != 2 {
		t.Fatalf("json: %v\n%s", err, out)
	}
	if w, d := res.Servers[0], res.Servers[1]; !w.Installed || w.Layout != "keep-data" || w.KeepData == nil || w.KeepData.Hash != hash || d.Installed || d.KeepData != nil || !strings.HasPrefix(d.Reason, "the installer refused to keep the data of db-01") {
		t.Errorf("result: %+v", res.Servers)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.2") || !strings.Contains(st, "layout: keep-data\n") {
		t.Errorf("one refused: state %s", st)
	}
	e.keepCaseAt = nil

	// the first question when all keep their data, and a dry run: the
	// plans alone
	order, e.installs = nil, nil
	_, errOut, code = e.run("n\n", true, "server", "multiinstall", "web-01", "db-01", "--keep-data", "--no-provision", "--account", "one")
	if code != exitAborted || len(order) != 0 || !strings.Contains(errOut, "Reinstall these 2 servers? Their data is kept: no disk is wiped. Each goes down and boots the rescue system, where the installer finds the old system and plans what it formats and what it keeps; nothing is written to a disk until you confirm the plans. [y/N]: ") {
		t.Errorf("all keep their data: code %d, %v, %q", code, order, errOut)
	}
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--keep-data", "--dry-run", "--yes", "--account", "one")
	if code != 0 || len(e.keepRuns()) != 2 || strings.Contains(strings.Join(e.keepRuns(), " "), "--confirm") {
		t.Errorf("dry run: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}
	expectRows(t, out, [][]string{
		{"SERVER", "HOSTNAME", "INSTALLED", "PROVISIONED", "REASON"},
		{"web-01", "web-01", "dry-run", "-"},
		{"db-01", "db-01", "dry-run", "-"},
	})

	// the checks, before any server is touched (exit status 2)
	order = nil
	for _, c := range []struct {
		plan string
		args []string
		msg  string
	}{
		{"servers:\n  web-01:\n    root: /dev/sda2\n", nil, "servers.web-01: --root, --keep-root and --format belong to --keep-data"},
		{"defaults:\n  keep-data: true\nservers:\n  web-01:\n    layout: lvm-raid1\n", nil, "servers.web-01: --keep-data and --layout lvm-raid1"},
		{"defaults:\n  keep-data: true\nservers:\n  web-01:\n    disks: [/dev/sda]\n", nil, "servers.web-01: --keep-data and --disks /dev/sda"},
		{"defaults:\n  keep-data: true\nservers:\n  web-01:\n  db-01:\n    keep-data: false\n    keep-root: true\n", nil, "servers.db-01: --root, --keep-root and --format belong to --keep-data"},
		{"servers:\n  web-01:\n    keep-data: yes please\n", nil, "cannot unmarshal !!str `yes please` into bool"},
		{"servers:\n  web-01:\n    keep-data: true\n    profile: nixos-25.11\n", nil, "nixos-25.11 is of the family nixos"},
		{"servers:\n  web-01:\n    keep-data: true\n", []string{"--unattended"}, "--keep-data and --unattended"},
		{"servers:\n  web-01: {}\n", []string{"--keep-data"}, "--keep-data and --plan: the plan has the servers' values"},
	} {
		args := append([]string{"server", "multiinstall", "--plan", writePlan(t, c.plan), "--yes", "--no-provision", "--account", "one"}, c.args...)
		if _, errOut, code := e.run("", false, args...); code != exitUsage || !strings.Contains(errOut, c.msg) || len(order) != 0 {
			t.Errorf("%q %v: code %d, touched %v, %q", c.plan, c.args, code, order, errOut)
		}
	}
}

func TestKeepDataPlanKeys(t *testing.T) {
	p, err := parsePlan([]byte("defaults:\n  keep-data: true\n  format: [/var, /srv/cache]\nservers:\n  web-01:\n    root: UUID=fdefec4e-baf2-514b-be0a-48bf9c43a89d\n    keep-root: true\n    format: /var\n  db-01:\n    keep-data: false\n    format: []\n"))
	if err != nil {
		t.Fatal(err)
	}
	web, db := planOptions(p.defaults.Over(p.servers[0].values)), planOptions(p.defaults.Over(p.servers[1].values))
	if !web.keepData || web.root != "UUID=fdefec4e-baf2-514b-be0a-48bf9c43a89d" || !web.keepRoot || strings.Join(web.format, " ") != "/var" {
		t.Errorf("web-01: %+v", web)
	}
	if db.keepData || db.root != "" || db.keepRoot || len(db.format) != 0 {
		t.Errorf("db-01: %+v", db)
	}
}

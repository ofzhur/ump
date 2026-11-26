package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/provider"
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
			e.keepFiles[addr] = nonEmpty(e.keepPlanFile, file)
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
	switch {
	case planning && e.keepPlanExit == 255:
		return out, "Connection to " + addr + " closed by remote host.\n", 255
	case planning && e.keepPlanExit != 0:
		return out, "install.sh: line 1811: jq exited with status 5\n", e.keepPlanExit
	case planning:
		return out + "==> plan file: /tmp/ump-keep-data.json\n==> the plan is made: stopping here, no disk was touched; --confirm with its hash installs it\n", "", 0
	}
	confirmed := ""
	if _, after, ok := strings.Cut(cmd, " --confirm "); ok {
		confirmed = strings.Fields(after)[0]
	}
	switch {
	case confirmed != hash:
		return out, "install.sh: keep-data: refused: the plan (above, " + hash + ") is not the one that was confirmed (" + confirmed + "): the disks, the options or the image on the storage changed since; look at it again and confirm it\n" + unwritten, 2
	case e.keepExit == 2:
		return out, "install.sh: the image's checksum is not the manifest's\n" + unwritten, 2
	case e.keepExit != 0 && e.keepQuiet:
		return "", "bash: line 1: /tmp/ump-install.json: No space left on device\n", e.keepExit
	case e.keepExit != 0 && e.keepBefore:
		return out, "install.sh: line 1502: mount exited with status 32\n", e.keepExit
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
	fetched := map[string]int{}
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetched[r.URL.Path]++
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
	clear(fetched)
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--keep-data", "--dry-run"); code != 0 {
		t.Errorf("an image by the manifest: code %d, %q", code, errOut)
	}
	// the check of the family and the check of the architectures read the
	// same manifests, each fetched once
	if fetched["/os/ubuntu-26.04/amd64/manifest.json"] != 1 || fetched["/os/ubuntu-26.04/arm64/manifest.json"] != 1 {
		t.Errorf("manifests fetched: %v", fetched)
	}
	// the question of a dry run
	e.write(testConfig)
	resetFakePower()
	e.installs = nil
	if _, errOut, code := e.run("n\n", true, "server", "reinstall", "web-01", "--account", "one", "--keep-data", "--dry-run"); code != exitAborted || !strings.Contains(errOut, "? (dry run: the running system is shut down as for the installation, not always cleanly; nothing is written to a disk) [y/N]: ") || e.touched() {
		t.Errorf("the question of a dry run: code %d, %q", code, errOut)
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
		"Reinstall fake/1 (web-01, 203.0.113.10) with " + what + " as web-01? Its data is kept: no disk is wiped. The running system is shut down (not always cleanly: services that write to the volumes that are kept are best stopped first) and the server boots the rescue system, where the installer finds the old system and plans what it formats and what it keeps; nothing is written to a disk until you confirm that plan. Then ump provision runs 00-base on the fleet. [y/N]: ",
		"Checking root access to 203.0.113.10 over SSH ... no (tried root, ump).\nGoing through the provider's rescue system.\nThe running system does not let UMP in to shut it down first: the provider resets web-01 hard, as it runs.\n",
		"Keep-data plan for " + what + " on web-01 (the installer looks, and writes nothing):\n",
		"==> keep-data: looking for the old system (read-only: nothing is written)\n",
		"==> keep-data: the old system Debian GNU/Linux 12 (bookworm) (old.example.net) on /dev/mapper/vg0-root; UEFI firmware; the layout of the profile is not applied, no disk is wiped or partitioned\n",
		"==> FORMAT (what is on them is lost)\n==>   /dev/mapper/vg0-root  ext4   /  (10G)\n==>   /dev/vda2             ext4   /boot  (1G)\n==> KEEP (as they are, mounted where they were)\n",
		"==> ASIDE (kept, under another name)\n==>   /dev/mapper/vg0-var   ext4   /var -> /var.old  (mounted)\n==> LOST (on the old root, beyond the system itself)\n",
		"==> plan: " + hash + " (sha256; --confirm takes it)\n==> plan file: /tmp/ump-keep-data.json\n==> the plan is made: stopping here, no disk was touched; --confirm with its hash installs it\n" +
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
	// the record: the layout is keep-data, in the keys the record always
	// had, which an ump from before keep-data reads; the old root and the
	// plan are on the server and in the result
	st := e.readState()
	if want := "      layout: keep-data\n      hostname: web-01\n      admin: ump\n      at: "; !strings.Contains(st, want) {
		t.Errorf("state lacks %q:\n%s", want, st)
	}
	if strings.Contains(st, "keep_data") || strings.Contains(st, "keep_root") || strings.Contains(st, hash) || !strings.Contains(st, "version: 3\n") {
		t.Errorf("state: %s", st)
	}
	if out, errOut, code := e.run("", false, "server", "show", "web-01", "--account", "one"); code != 0 || !strings.Contains(out, "(profile ubuntu-26.04, layout keep-data, administrator ump)") || strings.Contains(out, "Keep-data:") {
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

	// --keep-root and --format reach both runs; the record is the same
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
	if st := e.readState(); !strings.Contains(st, "      layout: keep-data\n") || strings.Contains(st, "keep_") || strings.Contains(st, kept) {
		t.Errorf("state: %s", st)
	}
	if out, _, _ := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--keep-root", "--root", "/dev/vg0/root", "--dry-run", "--yes", "--account", "one", "-o", "json"); !strings.Contains(out, `"hash": "`+kept+`"`) || !strings.Contains(out, `"device": "/dev/mapper/vg0-root"`) {
		t.Errorf("the result of a run with --keep-root: %s", out)
	}

	// an installation that wipes the disks takes the record's place
	resetFakePower()
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"); code != 0 {
		t.Fatalf("reinstall: code %d, %q", code, errOut)
	}
	if st := e.readState(); strings.Contains(st, "keep-data") || !strings.Contains(st, "layout: lvm-linear\n") {
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
	if st := e.readState(); !strings.Contains(st, "      layout: keep-data\n") {
		t.Errorf("state: %s", st)
	}

	// a plan made by an option nobody gave is not --yes's to take
	resetFakePower()
	e.installs = nil
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != exitAborted || len(e.keepRuns()) != 1 || !strings.Contains(errOut, "ump: --yes accepts a keep-data plan whose old system the installer found by itself, or that --root names; the plan for web-01 has its root on /dev/sda3 by option; run the command without --yes and read the plan\n") || !strings.Contains(errOut, "Nothing was written to the disks of web-01") {
		t.Errorf("--yes, by option: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}

	// several old systems: the installer refuses, with or without --yes,
	// and ump names them for --root; nothing was written, on a server
	// that had no address and no record before
	for _, args := range [][]string{{"--yes"}, nil} {
		e = keepEnv(t)
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
		if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
			t.Errorf("%v: the private address stayed, or a record: %s", args, st)
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
	// status 1: writing began; the volumes the plan keeps hold their data,
	// the same command recovers; the address is the server's, exit status 1
	e.keepExit = 1
	errOut, code = run()
	if code != exitGeneric || !strings.Contains(errOut, "==> keep-data: writing begins\n") || !strings.Contains(errOut, "ump: the installation on web-01 failed after the installer began to write (exit status 1, see above): the old root is gone or moved aside, the volumes the plan keeps hold their data. The server stays in the UMP rescue system (ump ssh web-01 --staging); run the same command again: the installer finds what it saved of the old system's fstab on the new root, plans anew, and ump asks again\n") || e.reboots != 0 {
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
	if code != exitGeneric || !strings.Contains(errOut, "could not fetch the installer from https://ump.zhurko.de/dl/install/install.sh (exit status 22); the server must reach the storage. Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n") {
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
	// with --via provider the running system was not asked, and the
	// refusal does not say that it was
	e.directRoot = true
	_, errOut, code = e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--via", "provider", "--yes", "--no-provision", "--account", "one")
	if code != exitUnreachable || len(fakePower.actions) != 0 || len(e.installs) != 0 || len(e.halts) != 0 || strings.Contains(errOut, "no root access") || !strings.Contains(errOut, "ump: --via provider leaves the running system of 203.0.113.10 aside, and the provider's rescue system of web-01 takes no SSH key: the only other way in is the provider's reinstall with a stock Linux, which erases the disks that --keep-data keeps. ump keeps the data of such a server through its running system alone: let UMP in there") {
		t.Errorf("keyless rescue system, --via provider: code %d, actions %v, %q", code, fakePower.actions, errOut)
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
		"Install on these 2 servers? Every disk of each is erased, but for web-01 (keep-data): there the running system is shut down (not always cleanly: services that write to the volumes that are kept are best stopped first), the installer plans in the rescue system, and nothing is written to a disk until you confirm the plans; the other installations start after that. [y/N]: ",
		"Keep-data plans for web-01, all at once ...\n",
		"[web-01] ==> FORMAT (what is on them is lost)\n",
		"[web-01] ==> the plan is made: stopping here, no disk was touched; --confirm with its hash installs it\n",
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
	for _, want := range []string{"      layout: keep-data\n      hostname: web-01\n", "      layout: lvm-linear\n      hostname: db-01.example.net\n"} {
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
	if code != exitAborted || len(order) != 0 || !strings.Contains(errOut, "Reinstall these 2 servers? Their data is kept: no disk is wiped. Each is shut down (not always cleanly: services that write to the volumes that are kept are best stopped first) and boots the rescue system, where the installer finds the old system and plans what it formats and what it keeps; nothing is written to a disk until you confirm the plans. [y/N]: ") {
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

// keepPlanWith is the plan file of a fixture with fields set: a path
// of keys, dots between them, and its value.
func keepPlanWith(t *testing.T, name string, set map[string]any) string {
	t.Helper()
	_, file, _ := keepFixture(t, name)
	var plan map[string]any
	if err := json.Unmarshal([]byte(file), &plan); err != nil {
		t.Fatal(err)
	}
	for path, v := range set {
		at := plan
		keys := strings.Split(path, ".")
		for _, k := range keys[:len(keys)-1] {
			at = at[k].(map[string]any)
		}
		if v == nil {
			delete(at, keys[len(keys)-1])
		} else {
			at[keys[len(keys)-1]] = v
		}
	}
	out, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}

// noLocks fails the test when a lock is left in the local home.
func noLocks(t *testing.T, what string) {
	t.Helper()
	b := &backend.Local{Dir: filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump")}
	if locks, err := b.Locks(context.Background()); err != nil || len(locks) != 0 {
		t.Errorf("%s: locks left: %+v, %v", what, locks, err)
	}
}

// TestKeepDataYesReadsThePlan: --yes takes no plan that only a person
// can judge: one whose root --root names without the installer knowing
// an old system there, and one that goes on after a failed installation
// and keeps less than the first plan did. Plans without those fields,
// as an installer from before them makes, are taken as before.
func TestKeepDataYesReadsThePlan(t *testing.T) {
	e := keepEnv(t)
	e.keepCase = "two-systems-root"
	hash := keepHash(t, "two-systems-root")
	run := func(yes bool, args ...string) (string, int) {
		resetFakePower()
		e.installs = nil
		args = append([]string{"server", "reinstall", "web-01", "--keep-data", "--no-provision", "--account", "one"}, args...)
		if yes {
			args = append(args, "--yes")
		}
		_, errOut, code := e.run("y\ny\n", !yes, args...)
		return errOut, code
	}
	unwritten := "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server unrescue web-01 --reboot starts the old system again.\n"
	for _, c := range []struct {
		set  map[string]any
		want string
	}{
		{map[string]any{"root.known": false}, "ump: --yes does not accept the keep-data plan for web-01: --root names /dev/sda3, where the installer found no old system it knows, so only the option says that this is the root to replace; run the command without --yes and read the plan\n"},
		{map[string]any{"resumed": true, "shrunk": true}, "ump: --yes does not accept the keep-data plan for web-01: it goes on after an installation that failed and keeps less than the first plan did; run the command without --yes and read the plan\n"},
		{map[string]any{"shrunk": true}, "ump: --yes does not accept the keep-data plan for web-01: it goes on after an installation that failed and keeps less than the first plan did; run the command without --yes and read the plan\n"},
	} {
		e.keepPlanFile = keepPlanWith(t, "two-systems-root", c.set)
		errOut, code := run(true, "--root", "/dev/sda3")
		if code != exitAborted || !strings.HasSuffix(errOut, unwritten+c.want) {
			t.Errorf("%v with --yes: code %d, %q", c.set, code, errOut)
		}
		if runs := e.keepRuns(); len(runs) != 1 || strings.Contains(runs[0], "--confirm") || e.installed {
			t.Errorf("%v with --yes: installer runs %q", c.set, runs)
		}
		if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
			t.Errorf("%v with --yes: state %s", c.set, st)
		}
		noLocks(t, "a plan --yes does not accept")
		// a person reads the plan and may take it
		if errOut, code := run(false, "--root", "/dev/sda3"); code != 0 || !strings.Contains(errOut, "as the plan above says (plan "+hash[:12]+")? What it lists under FORMAT and LOST is lost. [y/N]: ") || len(e.keepRuns()) != 2 {
			t.Errorf("%v, asked: code %d, runs %q, %q", c.set, code, e.keepRuns(), errOut)
		}
		os.Remove(filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "state.yaml"))
	}
	// what the fields say when they say nothing against the plan
	for _, set := range []map[string]any{
		{"root.known": true},
		{"resumed": true},
		{"resumed": true, "shrunk": false},
		{"root.known": nil},
	} {
		e.keepPlanFile = keepPlanWith(t, "two-systems-root", set)
		if errOut, code := run(true, "--root", "/dev/sda3"); code != 0 || len(e.keepRuns()) != 2 {
			t.Errorf("%v with --yes: code %d, runs %q, %q", set, code, e.keepRuns(), errOut)
		}
	}
}

// TestKeepDataInstallerPlans: the plans the installer makes of the
// machines of its own test that only a person can judge, read as it
// writes them: the root that --root alone names (root.known false), the
// second attempt that keeps less than the first plan did (resumed and
// shrunk), and the second attempt that keeps it all, which --yes takes;
// what the new system makes anew at every boot is shown, and carried in
// the result; and another image on the storage between the plan and the
// installation is another plan.
func TestKeepDataInstallerPlans(t *testing.T) {
	e := keepEnv(t)
	run := func(args ...string) (string, string, int) {
		resetFakePower()
		e.installs = nil
		return e.run("", false, append([]string{"server", "reinstall", "web-01", "--keep-data", "--no-provision", "--account", "one"}, args...)...)
	}
	for _, c := range []struct {
		fixture string
		args    []string
		shown   string
		want    string
	}{
		{"no-system-root", []string{"--root", "/dev/sda2", "--yes"},
			"==>   /dev/sda2 holds no old system (no /etc/fstab and /etc/os-release): it becomes the root because --root says so, and no other volume is mounted in the new system\n",
			"ump: --yes does not accept the keep-data plan for web-01: --root names /dev/sda2, where the installer found no old system it knows, so only the option says that this is the root to replace; run the command without --yes and read the plan\n"},
		{"resume-shrunk", []string{"--yes"},
			"==>   this plan keeps less than the one the interrupted installation was confirmed with: /boot is not in it any more; what the old fstab named there is not found now. Look at it before you confirm\n",
			"ump: --yes does not accept the keep-data plan for web-01: it goes on after an installation that failed and keeps less than the first plan did; run the command without --yes and read the plan\n"},
	} {
		e.keepCase = c.fixture
		_, errOut, code := run(c.args...)
		if code != exitAborted || !strings.Contains(errOut, c.shown) || !strings.HasSuffix(errOut, c.want) {
			t.Errorf("%s: code %d, %q", c.fixture, code, errOut)
		}
		if runs := e.keepRuns(); len(runs) != 1 || strings.Contains(runs[0], "--confirm") || e.installed {
			t.Errorf("%s: installer runs %q", c.fixture, runs)
		}
		if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
			t.Errorf("%s: state %s", c.fixture, st)
		}
	}
	// a second attempt that keeps what the first plan kept is --yes's to take
	e.keepCase = "resume"
	_, errOut, code := run("--yes")
	if code != 0 || len(e.keepRuns()) != 2 || !strings.Contains(errOut, "on /dev/mapper/vg0-root, after an interrupted installation; UEFI firmware") {
		t.Errorf("resume: code %d, runs %q, %q", code, e.keepRuns(), errOut)
	}

	// what the old crypttab makes anew at every boot: shown as the
	// installer prints it, and in the result with the crypttab's entries,
	// the image and the labels of what is formatted
	e.keepCase = "crypttab-swap-labelled"
	out, errOut, code := run("--dry-run", "--yes", "-o", "json")
	if code != 0 || !strings.Contains(errOut, "==> MADE ANEW AT EVERY BOOT (crypttab: what is on them is overwritten each time the new system starts, as the old one did)\n==>   /dev/vda4             swap  none  (cswap: LABEL=cryptswap, swap,offset=2048,cipher=aes-xts-plain64,size=256)\n") ||
		!strings.Contains(errOut, "==> the plan is made: stopping here, no disk was touched; --confirm with its hash installs it\nDry run done; nothing was written to the disks of web-01") {
		t.Fatalf("made anew at every boot: code %d, %q", code, errOut)
	}
	var res struct {
		KeepData struct {
			Image struct {
				Version string `json:"version"`
				SHA256  string `json:"sha256"`
			} `json:"image"`
			Root struct {
				UUID  string `json:"uuid"`
				Known *bool  `json:"known"`
			} `json:"root"`
			Resumed  *bool `json:"resumed"`
			Shrunk   *bool `json:"shrunk"`
			Volatile []struct {
				Device  string `json:"device"`
				Name    string `json:"name"`
				Source  string `json:"source"`
				Options string `json:"options"`
				How     string `json:"how"`
			} `json:"volatile"`
			Crypttab []struct {
				Name   string `json:"name"`
				Source string `json:"source"`
			} `json:"crypttab"`
			Format []struct {
				Device string  `json:"device"`
				Label  *string `json:"label"`
			} `json:"format"`
		} `json:"keep_data"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("json: %v\n%s", err, out)
	}
	k := res.KeepData
	if k.Image.Version != "2026.10.12" || len(k.Image.SHA256) != 64 || k.Root.UUID == "" || k.Root.Known != nil || k.Resumed != nil || k.Shrunk != nil ||
		len(k.Volatile) != 1 || k.Volatile[0].Device != "/dev/vda4" || k.Volatile[0].Name != "cswap" || k.Volatile[0].Source != "LABEL=cryptswap" || k.Volatile[0].How != "swap" || !strings.HasPrefix(k.Volatile[0].Options, "swap,offset=2048") ||
		len(k.Crypttab) != 1 || k.Crypttab[0].Name != "cswap" || k.Crypttab[0].Source != "LABEL=cryptswap" ||
		len(k.Format) != 2 || k.Format[0].Label == nil || k.Format[1].Label == nil {
		t.Errorf("the result: %+v\n%s", k, out)
	}
	// and what a plan says of itself comes through as it is
	e.keepCase = "resume-shrunk"
	if out, _, code := run("--dry-run", "--yes", "-o", "yaml"); code != 0 || !strings.Contains(out, "\n  resumed: true\n") || !strings.Contains(out, "\n  shrunk: true\n") {
		t.Errorf("resumed and shrunk, as YAML: code %d\n%s", code, out)
	}
	e.keepCase = "no-system-root"
	if out, _, code := run("--root", "/dev/sda2", "--dry-run", "--yes", "-o", "yaml"); code != 0 || !strings.Contains(out, "\n    known: false\n") {
		t.Errorf("root.known, as YAML: code %d\n%s", code, out)
	}

	// another image on the storage between the plan and the installation:
	// the installer's plan has another hash, and it writes nothing
	e.keepCase, e.keepDrift = "lvm", "lvm-other-image"
	_, errOut, code = run("--yes")
	e.keepDrift = ""
	if code != exitAborted || keepHash(t, "lvm") == keepHash(t, "lvm-other-image") ||
		!strings.Contains(errOut, "install.sh: keep-data: refused: the plan (above, "+keepHash(t, "lvm-other-image")+") is not the one that was confirmed ("+keepHash(t, "lvm")+"): the disks, the options or the image on the storage changed since") ||
		!strings.Contains(errOut, "ump: the installer stopped before it wrote anything (exit status 2, see above). Nothing was written to the disks of web-01") {
		t.Errorf("another image: code %d, %q", code, errOut)
	}
}

// TestKeepDataPlanFile: the plan ump asks about is the one the
// installer printed: a plan file that is a refusal, has no hash, or has
// another hash than the one that was shown ends the run with nothing
// written (exit status 1: something failed).
func TestKeepDataPlanFile(t *testing.T) {
	e := keepEnv(t)
	other := keepHash(t, "lvm-format")
	unwritten := " Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n"
	for _, c := range []struct {
		file, want string
	}{
		{`{"version": 1, "refused": "the old root is in use"}`, "ump: the installer's plan for web-01 (/tmp/ump-keep-data.json) is a refusal, though the installer ended as after a plan: the old root is in use." + unwritten},
		{keepPlanWith(t, "lvm", map[string]any{"hash": nil}), "ump: the installer's plan for web-01 (/tmp/ump-keep-data.json) has no hash to confirm." + unwritten},
		{keepPlanWith(t, "lvm", map[string]any{"hash": "8911da13"}), "ump: the installer's plan for web-01 (/tmp/ump-keep-data.json) has no hash to confirm." + unwritten},
		{keepPlanWith(t, "lvm", map[string]any{"hash": other}), "ump: the plan read back from the rescue system of web-01 (/tmp/ump-keep-data.json, plan " + other[:12] + ") is not the plan the installer printed above, and ump asks about no plan it did not show." + unwritten},
		{"not JSON", "ump: the installer's plan for web-01 could not be read back from the rescue system (/tmp/ump-keep-data.json): invalid character 'o' in literal null (expecting 'u')." + unwritten},
	} {
		for _, args := range [][]string{{"--yes"}, {"--dry-run", "--yes"}, nil} {
			resetFakePower()
			e.installs, e.keepPlanFile = nil, c.file
			out, errOut, code := e.run("y\ny\n", args == nil, append([]string{"server", "reinstall", "web-01", "--keep-data", "--no-provision", "--account", "one", "-o", "json"}, args...)...)
			if code != exitGeneric || out != "" || !strings.HasSuffix(errOut, c.want) || strings.Contains(errOut, "as the plan above says") {
				t.Errorf("%q %v: code %d, %q\n%s", c.file, args, code, errOut, out)
			}
			if runs := e.keepRuns(); len(runs) != 1 || e.installed || e.reboots != 0 {
				t.Errorf("%q %v: installer runs %q", c.file, args, runs)
			}
			if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
				t.Errorf("%q %v: state %s", c.file, args, st)
			}
			noLocks(t, "a plan file that is not the plan")
		}
	}
}

// TestKeepDataEnds: how the runs of the installer may end besides well,
// and what ump makes of each: the exit status, whether the private
// address stays the server's (the disks were written, or may have
// been), and what the message says of the disks.
func TestKeepDataEnds(t *testing.T) {
	e := keepEnv(t)
	unwritten := "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n"
	again := "run the same command again: the installer finds what it saved of the old system's fstab on the new root, plans anew, and ump asks again\n"
	for _, c := range []struct {
		name    string
		set     func()
		code    int
		runs    int
		written bool
		want    string
	}{
		{"the connection ends in the plan run", func() { e.keepPlanExit = 255 }, exitUnreachable, 1, false,
			"ump: the connection to 203.0.113.10 ended while the installer planned, which writes nothing; the server is in the UMP rescue system, look there (ump ssh web-01 --staging); ump server unrescue web-01 --reboot starts the old system again\n"},
		{"the plan run fails", func() { e.keepPlanExit = 1 }, exitGeneric, 1, false,
			"ump: the installer's plan for web-01 failed (exit status 1, see above). " + unwritten},
		{"the plan run is killed", func() { e.keepPlanExit = 137 }, exitGeneric, 1, false,
			"ump: the installer's plan for web-01 failed (exit status 137, see above). " + unwritten},
		{"the connection ends after the installer began to write", func() { e.keepExit = 255 }, exitUnreachable, 2, true,
			"ump: the connection to 203.0.113.10 ended during the installation, after the installer began to write: the old root is gone or moved aside, the volumes the plan keeps hold their data. The server is in the UMP rescue system, look there (ump ssh web-01 --staging); when the installer is not running any more, " + again},
		{"the connection ends before the installer says that it writes", func() { e.keepExit, e.keepBefore = 255, true }, exitUnreachable, 2, true,
			"ump: the connection to 203.0.113.10 ended during the installation, before the installer said that it writes: nothing was written, unless that line went with the connection. The server is in the UMP rescue system, look there (ump ssh web-01 --staging); when the installer is not running any more, run the same command again: it plans anew, and ump asks again\n"},
		{"the connection ends before the installer runs", func() { e.keepExit, e.keepQuiet = 255, true }, exitUnreachable, 2, false,
			"ump: the connection to 203.0.113.10 ended before the installer ran. " + unwritten},
		{"the installer fails before it writes", func() { e.keepExit, e.keepBefore = 1, true }, exitGeneric, 2, false,
			"ump: the installation on web-01 failed before the installer began to write (exit status 1, see above). " + unwritten},
		{"the installer is killed before it writes", func() { e.keepExit, e.keepBefore = 143, true }, exitGeneric, 2, false,
			"ump: the installation on web-01 failed before the installer began to write (exit status 143, see above). " + unwritten},
		{"the specification cannot be put there", func() { e.keepExit, e.keepQuiet = 1, true }, exitGeneric, 2, false,
			"ump: the installation on web-01 failed before the installer began to write (exit status 1, see above). " + unwritten},
		{"the installer is killed after it began to write", func() { e.keepExit = 137 }, exitGeneric, 2, true,
			"ump: the installation on web-01 failed after the installer began to write (exit status 137, see above): the old root is gone or moved aside, the volumes the plan keeps hold their data. The server stays in the UMP rescue system (ump ssh web-01 --staging); " + again},
	} {
		e = keepEnv(t)
		c.set()
		_, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
		if code != c.code || !strings.HasSuffix(errOut, c.want) {
			t.Errorf("%s: code %d, %q", c.name, code, errOut)
		}
		if strings.Contains(errOut, "the old root is gone") != (c.written && !e.keepBefore) {
			t.Errorf("%s: what the message says of the old root: %q", c.name, errOut[strings.LastIndex(errOut, "ump: "):])
		}
		if runs := e.keepRuns(); len(runs) != c.runs || e.reboots != 0 {
			t.Errorf("%s: installer runs %q, reboots %d", c.name, runs, e.reboots)
		}
		if st := e.readState(); strings.Contains(st, "10.0.0.1") != c.written || strings.Contains(st, "installed") {
			t.Errorf("%s: state %s", c.name, st)
		}
		noLocks(t, c.name)
	}
}

// TestKeepDataInterrupted: Ctrl-C at the second question. Nothing was
// written; the line that says how the old system starts again asks the
// provider although the run is over, and names both commands where the
// provider does not say.
func TestKeepDataInterrupted(t *testing.T) {
	e := keepEnv(t)
	e.holdStdin = true
	// the interrupt comes once the plan was read back: at the question
	e.remoteGate = func(addr string, argv []string) {
		if argv[len(argv)-1] == "cat /tmp/ump-keep-data.json" {
			e.interrupt()
		}
	}
	_, errOut, code := e.run("y\n", true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != exitAborted || !strings.HasSuffix(errOut, "What it lists under FORMAT and LOST is lost. [y/N]: \nNothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server unrescue web-01 --reboot starts the old system again.\nump: interrupted\n") {
		t.Errorf("interrupted: code %d, %q", code, errOut)
	}
	if runs := e.keepRuns(); len(runs) != 1 || e.installed || e.reboots != 0 || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard" {
		t.Errorf("interrupted: installer runs %q, actions %v", runs, fakePower.actions)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.1") || strings.Contains(st, "installed") {
		t.Errorf("interrupted: state %s", st)
	}
	noLocks(t, "interrupted")

	// the provider does not say whether its rescue system is still the
	// boot option
	e = keepEnv(t)
	fakeRescueErr = &provider.APIError{Provider: "fake", Status: 503, Message: "try later"}
	defer func() { fakeRescueErr = nil }()
	_, errOut, code = e.run("y\nn\n", true, "server", "reinstall", "web-01", "--keep-data", "--account", "one")
	if code != exitAborted || !strings.HasSuffix(errOut, "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server unrescue web-01 --reboot starts the old system again where the provider's rescue system is still the boot option, ump server reboot web-01 --hard where it is not (the provider did not say which: fake: try later (HTTP 503)).\nump: aborted\n") {
		t.Errorf("the provider does not say: code %d, %q", code, errOut)
	}
}

// bareProvider is a provider that can neither switch the power nor boot
// a rescue system; rescueOnly one that has a rescue system and no power
// control, which notes whether the question about it came with a time
// to answer in.
type bareProvider struct{ provider.Provider }

type rescueOnly struct {
	provider.Provider
	on      bool
	err     error
	bounded bool
}

func (r *rescueOnly) RescueEnabled(ctx context.Context, m provider.Machine) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	_, r.bounded = ctx.Deadline()
	return r.on, r.err
}

func (r *rescueOnly) EnableRescue(ctx context.Context, m provider.Machine, o provider.RescueOptions) (*provider.RescueInfo, error) {
	return nil, errors.New("not here")
}

func (r *rescueOnly) DisableRescue(ctx context.Context, m provider.Machine) error { return nil }

// TestKeepDataWayBack: how the old system starts again, by what the
// provider can do and says, and by how the run reached the rescue
// system.
func TestKeepDataWayBack(t *testing.T) {
	a := &app{}
	m := provider.Machine{Provider: "fake", ID: "7", Name: "lab-07"}
	own := &installEntry{imageEntry: &imageEntry{addr: "203.0.113.7"}, system: "203.0.113.7", staging: "ump ssh lab-07 --staging"}
	session := &installEntry{imageEntry: &imageEntry{addr: "10.9.0.23"}, system: "192.0.2.7", staging: "ssh -J pxe1 -i /home/op/.config/ump/id_ed25519 root@10.9.0.23"}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, c := range []struct {
		name  string
		prov  provider.Provider
		entry *installEntry
		ctx   context.Context
		want  string
	}{
		{"power and a rescue system that is the boot option", &fakeProvider{}, own, context.Background(), "ump server unrescue lab-07 --reboot starts the old system again"},
		{"the same after an interrupt", &fakeProvider{}, own, cancelled, "ump server unrescue lab-07 --reboot starts the old system again"},
		{"power alone", &noRescueProvider{}, own, context.Background(), "ump server reboot lab-07 --hard starts the old system again"},
		{"neither", bareProvider{&fakeProvider{}}, own, context.Background(), "A reboot starts the old system again (ump ssh lab-07 --staging -- systemctl reboot)"},
		{"neither, the rescue system at an address of its own", bareProvider{&fakeProvider{}}, session, cancelled, "A reboot starts the old system again (ssh -J pxe1 -i /home/op/.config/ump/id_ed25519 root@10.9.0.23 systemctl reboot)"},
		{"neither, no entry", bareProvider{&fakeProvider{}}, nil, context.Background(), "A reboot starts the old system again (ump ssh lab-07 --staging -- systemctl reboot)"},
		{"a rescue system that is not the boot option, no power", &rescueOnly{Provider: &fakeProvider{}}, session, cancelled, "A reboot starts the old system again (ssh -J pxe1 -i /home/op/.config/ump/id_ed25519 root@10.9.0.23 systemctl reboot)"},
		{"a rescue system that is the boot option, no power", &rescueOnly{Provider: &fakeProvider{}, on: true}, session, cancelled, "ump server unrescue lab-07 --reboot starts the old system again"},
		{"a rescue system that does not say, no power", &rescueOnly{Provider: &fakeProvider{}, err: errors.New("no answer")}, session, context.Background(),
			"ump server unrescue lab-07 --reboot starts the old system again where the provider's rescue system is still the boot option, ssh -J pxe1 -i /home/op/.config/ump/id_ed25519 root@10.9.0.23 systemctl reboot where it is not (the provider did not say which: no answer)"},
	} {
		resetFakePower()
		fakePower.rescue["fake/7"] = true
		p := &installPlan{ref: "lab-07", m: m, prov: c.prov, keep: &keepData{entry: c.entry}}
		if got := a.oldSystemBack(c.ctx, p); got != c.want {
			t.Errorf("%s: %s", c.name, got)
		}
		// the question to the provider has a time to answer in
		if r, ok := c.prov.(*rescueOnly); ok && !r.bounded {
			t.Errorf("%s: the provider was asked without a time to answer in", c.name)
		}
	}
	resetFakePower()
	if got := lowerFirst("A reboot starts it"); got != "a reboot starts it" {
		t.Errorf("lowerFirst: %s", got)
	}
	for _, s := range []string{"ump server reboot x --hard starts it", "ssh root@10.9.0.23 systemctl reboot starts it", ""} {
		if got := lowerFirst(s); got != s {
			t.Errorf("lowerFirst: %s", got)
		}
	}
}

// TestKeepDataNoPower: a server whose provider has neither a rescue
// system nor power control (one added by hand) keeps its data through
// its running system, and a reboot from within the rescue system is
// the way back.
func TestKeepDataNoPower(t *testing.T) {
	e := keepEnv(t)
	if _, errOut, code := e.run("", false, "server", "add", "rack-07", "--ipv4", "203.0.113.10"); code != 0 {
		t.Fatalf("add: code %d, %q", code, errOut)
	}
	e.directRoot = true
	_, errOut, code := e.run("y\nn\n", true, "server", "reinstall", "rack-07", "--keep-data", "--no-provision", "--account", "manual")
	if code != exitAborted || !strings.HasSuffix(errOut, "Nothing was written to the disks of rack-07, which stays in the UMP rescue system (ump ssh rack-07 --staging). A reboot starts the old system again (ump ssh rack-07 --staging -- systemctl reboot).\nump: aborted\n") {
		t.Errorf("declined: code %d, %q", code, errOut)
	}
	if len(fakePower.actions) != 0 || len(e.halts) != 0 || !e.kexeced {
		t.Errorf("declined: actions %v, halts %v, kexec %v", fakePower.actions, e.halts, e.kexeced)
	}
	// the installer refuses: the same way back, in the error
	e.keepCase = "luks-root"
	_, errOut, code = e.run("", false, "server", "reinstall", "rack-07", "--keep-data", "--yes", "--no-provision", "--account", "manual")
	if code != exitAborted || !strings.HasSuffix(errOut, "Nothing was written to the disks of rack-07, which stays in the UMP rescue system (ump ssh rack-07 --staging); a reboot starts the old system again (ump ssh rack-07 --staging -- systemctl reboot)\n") {
		t.Errorf("refused: code %d, %q", code, errOut)
	}
	// and it installs
	e.keepCase = ""
	if _, errOut, code := e.run("", false, "server", "reinstall", "rack-07", "--keep-data", "--yes", "--no-provision", "--account", "manual"); code != 0 || !strings.Contains(errOut, "rack-07 is up:") || !strings.Contains(e.readState(), "layout: keep-data\n") {
		t.Errorf("installed: code %d, %q", code, errOut)
	}
}

// TestKeepDataShutsDownFirst: on the provider's way, which is a reset,
// the running system of a keep-data installation is shut down over SSH
// first where it lets UMP in, and ump waits until it is down; where it
// cannot, or the system does not go down in time, the reset comes as
// before and a line says so. An installation that wipes the disks
// resets as it always did.
func TestKeepDataShutsDownFirst(t *testing.T) {
	const halt = "systemctl halt || shutdown -H now"
	run := func(e *testEnv, args ...string) (string, int) {
		resetFakePower()
		_, errOut, code := e.run("", false, append([]string{"server", "reinstall", "web-01", "--yes", "--no-provision", "--account", "one"}, args...)...)
		return errOut, code
	}
	// root gets in: halted, down, then the provider's rescue system and
	// its reset
	e := keepEnv(t)
	e.directRoot = true
	e.refusals = 3
	keepDownWait = time.Minute
	errOut, code := run(e, "--keep-data", "--via", "provider")
	if code != 0 || !regexp.MustCompile(`Checking root access to 203\.0\.113\.10 over SSH, to shut the running system down first \.\.\. yes; shutting it down as root \(up to 1m\) \.\.\. down after \ds\.\nRescue system enabled for fake/1 \(web-01, 203\.0\.113\.10\); rebooting into it\.\n`).MatchString(errOut) {
		t.Fatalf("shut down first: code %d, %q", code, errOut)
	}
	if len(e.halts) != 1 || e.halts[0] != "root: "+halt || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" || len(e.keepRuns()) != 2 {
		t.Errorf("shut down first: halts %q, actions %v, runs %q", e.halts, fakePower.actions, e.keepRuns())
	}
	// it waited while the machine still refused connections: its kernel ran
	if e.refusals >= 0 {
		t.Errorf("the provider reset a machine that still refused connections (%d refusals left)", e.refusals)
	}
	// the system was down when the provider was asked: no wait for it to go down
	if strings.Contains(errOut, "to go down ... still up") && strings.Index(errOut, "to go down") < strings.Index(errOut, "Keep-data plan for") {
		t.Errorf("waited for a system that was down to go down: %q", errOut)
	}

	// a sudo user gets in
	e = keepEnv(t)
	e.directSudo = true
	keepDownWait = time.Minute
	errOut, code = run(e, "--keep-data", "--via", "provider")
	if code != 0 || !strings.Contains(errOut, "to shut the running system down first ... yes; shutting it down as ump (up to 1m) ... down after ") || len(e.halts) != 1 || e.halts[0] != "ump: sudo -n sh -c '"+halt+"'" {
		t.Errorf("as a sudo user: code %d, halts %q, %q", code, e.halts, errOut)
	}

	// the fallbacks: each says why, and the installation goes on
	for _, c := range []struct {
		name  string
		set   func(e *testEnv)
		halts int
		want  string
	}{
		{"no access", func(e *testEnv) {}, 0,
			"Checking root access to 203.0.113.10 over SSH, to shut the running system down first ... no (tried root, ump).\nThe running system does not let UMP in to shut it down first: the provider resets web-01 hard, as it runs.\nRescue system enabled for"},
		{"the shutdown fails", func(e *testEnv) { e.directRoot, e.haltExit = true, 1 }, 1,
			"to shut the running system down first ... yes, as root, but the shutdown failed (exit status 1).\nThe running system was not shut down: the provider resets web-01 hard, as it runs.\nRescue system enabled for"},
		{"the system stays up", func(e *testEnv) { e.directRoot, e.haltHangs = true, true }, 1,
			"yes; shutting it down as root (up to 100ms) ... not down yet.\nThe running system is not down after 100ms: the provider resets web-01 hard, as it runs.\nRescue system enabled for"},
		{"the system ends its SSH server and goes on running", func(e *testEnv) { e.directRoot, e.refusals = true, 1<<30 }, 1,
			"yes; shutting it down as root (up to 100ms) ... not down yet.\nThe running system is not down after 100ms: the provider resets web-01 hard, as it runs.\nRescue system enabled for"},
	} {
		e = keepEnv(t)
		c.set(e)
		errOut, code = run(e, "--keep-data", "--via", "provider")
		if code != 0 || !strings.Contains(errOut, c.want) || len(e.halts) != c.halts || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" {
			t.Errorf("%s: code %d, halts %q, actions %v, %q", c.name, code, e.halts, fakePower.actions, errOut)
		}
	}

	// the UMP rescue system runs there already: no system to shut down
	e = keepEnv(t)
	e.inImage = true
	errOut, code = run(e, "--keep-data", "--via", "provider")
	if code != 0 || !strings.Contains(errOut, "to shut the running system down first ... web-01 runs the UMP rescue system 2026.10.12: no system to shut down.\n") || len(e.halts) != 0 {
		t.Errorf("in the rescue system: code %d, halts %q, %q", code, e.halts, errOut)
	}

	// the provider cannot arm its rescue system after the shutdown: the
	// error says how the system starts again
	e = keepEnv(t)
	e.directRoot = true
	keepDownWait = time.Minute
	fakeRescueFails = true
	errOut, code = run(e, "--keep-data", "--via", "provider")
	fakeRescueFails = false
	if code == 0 || !strings.HasSuffix(errOut, "ump: account one: fake: no rescue system today; the running system of web-01 was shut down for it: ump server reboot web-01 --hard starts it again\n") || len(e.halts) != 1 || len(e.keepRuns()) != 0 {
		t.Errorf("no rescue system after the shutdown: code %d, halts %q, %q", code, e.halts, errOut)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.1") {
		t.Errorf("no rescue system after the shutdown: state %s", st)
	}

	// an installation that wipes the disks: the reset alone, as ever
	e = keepEnv(t)
	e.directRoot = true
	errOut, code = run(e, "--via", "provider")
	if code != 0 || len(e.halts) != 0 || strings.Contains(errOut, "shut") || strings.Join(fakePower.actions, ",") != "rescue,reboot:hard,unrescue" {
		t.Errorf("an ordinary reinstall: code %d, halts %q, actions %v, %q", code, e.halts, fakePower.actions, errOut)
	}
	// and the direct way of a keep-data installation is a kexec
	e = keepEnv(t)
	e.directRoot = true
	errOut, code = run(e, "--keep-data")
	if code != 0 || len(e.halts) != 0 || len(fakePower.actions) != 0 || !strings.Contains(errOut, "Loading the UMP rescue image from https://ump.zhurko.de/dl/rescue on web-01, without the provider's rescue system.\nThe running system of web-01 ends with the jump: its file systems are synced and remounted read-only before it, its services are not stopped.\n") {
		t.Errorf("the direct way: code %d, halts %q, actions %v, %q", code, e.halts, fakePower.actions, errOut)
	}
	// which says nothing of the kind where every disk is erased anyway
	e = keepEnv(t)
	e.directRoot = true
	if errOut, code = run(e); code != 0 || !strings.Contains(errOut, "Loading the UMP rescue image") || strings.Contains(errOut, "ends with the jump") {
		t.Errorf("the direct way of an ordinary reinstall: code %d, %q", code, errOut)
	}
}

// TestSSHRefuses: a machine that is up without an SSH server refuses,
// one with an SSH server does not.
func TestSSHRefuses(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	old := sshPort
	sshPort = l.Addr().(*net.TCPAddr).Port
	defer func() { sshPort = old }()
	if sshRefuses(context.Background(), "127.0.0.1") || !sshAnswers(context.Background(), "127.0.0.1") {
		t.Errorf("a port that listens refuses, or does not answer")
	}
	l.Close()
	if !sshRefuses(context.Background(), "127.0.0.1") || sshAnswers(context.Background(), "127.0.0.1") {
		t.Errorf("a port nothing listens on does not refuse, or answers")
	}
	if sshRefuses(context.Background(), "") {
		t.Errorf("no address refuses")
	}
	// through a jump host a refusal is not told from silence
	a := &app{refuses: func(ctx context.Context, addr string) bool { return true }}
	if !a.refused(context.Background(), "192.0.2.7") {
		t.Errorf("refused: no")
	}
	a.setRoute("192.0.2.7", sshRoute{jump: "pxe1"})
	if a.refused(context.Background(), "192.0.2.7") || (&app{}).refused(context.Background(), "192.0.2.7") {
		t.Errorf("refused through a jump host, or without a probe")
	}
}

// TestKeepDataYAMLNumbers: the plan in -o yaml has its sizes as the
// installer gave them, a volume of a terabyte and more too.
func TestKeepDataYAMLNumbers(t *testing.T) {
	plan, err := parseKeepPlan([]byte(`{"hash": "` + strings.Repeat("a", 64) + `", "version": 1, "root": {"device": "/dev/sda2", "size_mb": 1907729, "by": "detection"}, "lost": [{"path": "/srv", "mb": 1500000}, {"path": "/opt", "mb": 12}], "ratio": 0.5, "big": 36893488147419103232}`))
	if err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := printEncoded(&b, installResult{KeepData: plan.doc}, formatYAML); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"size_mb: 1907729\n", "mb: 1500000\n", "mb: 12\n", "version: 1\n", "ratio: 0.5\n", "big: 3.6893488147419103e+19\n"} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("yaml lacks %q:\n%s", want, b.String())
		}
	}
	b.Reset()
	if err := printEncoded(&b, installResult{KeepData: plan.doc}, formatJSON); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"size_mb": 1907729`, `"mb": 1500000`, `"ratio": 0.5`} {
		if !strings.Contains(b.String(), want) {
			t.Errorf("json lacks %q:\n%s", want, b.String())
		}
	}
	// through the command: sizes of a million MB and more
	e := keepEnv(t)
	e.keepPlanFile = keepPlanWith(t, "lvm", map[string]any{"root.size_mb": 1907729, "lost": []any{map[string]any{"path": "/srv", "mb": 2500000}}})
	out, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--keep-data", "--dry-run", "--yes", "--account", "one", "-o", "yaml")
	if code != 0 || !strings.Contains(out, "size_mb: 1907729\n") || !strings.Contains(out, "mb: 2500000\n") || strings.Contains(out, "e+0") {
		t.Errorf("-o yaml: code %d, %q\n%s", code, errOut, out)
	}
}

// TestKeepDataLayoutName: no layout is named as a keep-data
// installation is recorded.
func TestKeepDataLayoutName(t *testing.T) {
	e := keepEnv(t)
	if _, errOut, code := e.run("", false, "config", "disk-layout", "create", "keep-data", "--from", "lvm-linear"); code != exitUsage || !strings.Contains(errOut, "layout name: keep-data is taken: it is what an installation that kept the data is recorded with (ump server reinstall --keep-data), which applies no layout") {
		t.Errorf("create: code %d, %q", code, errOut)
	}
	// one put into the file by hand is no layout either
	path := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ump", "profiles", "layouts.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(data), "\n  lvm-linear:\n", "\n  keep-data:\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, _, code := e.run("", false, "config", "disk-layout", "list")
	if rows := fields(out); code != 0 || !slices.ContainsFunc(rows, func(r []string) bool { return r[0] == "keep-data" && slices.Contains(r, "invalid") }) {
		t.Errorf("list: code %d\n%s", code, out)
	}
	if _, errOut, code := e.run("", false, "server", "reinstall", "web-01", "--yes", "--account", "one", "--layout", "keep-data"); code != exitGeneric || !strings.Contains(errOut, "layout keep-data: the name is taken") || e.touched() {
		t.Errorf("--layout keep-data: code %d, %q", code, errOut)
	}
}

// TestKeepDataMultiinstallYes: in a multiinstall, --yes takes the plans
// reinstall's --yes takes; one it does not take ends the run before
// anything is written anywhere.
func TestKeepDataMultiinstallYes(t *testing.T) {
	e := multiEnv(t)
	// db-01's plan is made by an option nobody gave
	e.keepCaseAt = map[string]string{"203.0.113.11": "two-systems-root"}
	out, errOut, code := e.run("", false, "server", "multiinstall", "web-01", "db-01", "--keep-data", "--yes", "--no-provision", "--account", "one")
	if code != exitAborted || out != "" || !strings.HasSuffix(errOut, "Nothing was written to the disks of web-01, which stays in the UMP rescue system (ump ssh web-01 --staging). ump server reboot web-01 --hard starts the old system again.\nNothing was written to the disks of db-01, which stays in the UMP rescue system (ump ssh db-01 --staging). ump server reboot db-01 --hard starts the old system again.\nump: --yes accepts a keep-data plan whose old system the installer found by itself, or that --root names; the plan for db-01 has its root on /dev/sda3 by option; run the command without --yes and read the plan\n") {
		t.Errorf("a plan --yes does not take: code %d, %q\n%s", code, errOut, out)
	}
	if runs := e.keepRuns(); len(runs) != 2 || strings.Contains(strings.Join(runs, " "), "--confirm") {
		t.Errorf("a plan --yes does not take: installer runs %q", runs)
	}
	if st := e.readState(); strings.Contains(st, "10.0.0.") || strings.Contains(st, "installed") {
		t.Errorf("a plan --yes does not take: state %s", st)
	}
	noLocks(t, "a plan --yes does not take")

	// with --verbose, each installation is the copy that has its plan
	e.keepCaseAt, e.installs = nil, nil
	hash := keepHash(t, "lvm")
	out, errOut, code = e.run("", false, "server", "multiinstall", "web-01", "db-01", "--keep-data", "--yes", "--no-provision", "--account", "one", "-v")
	if code != 0 {
		t.Fatalf("--verbose: code %d, %q\n%s", code, errOut, out)
	}
	confirmed := 0
	for _, r := range e.keepRuns() {
		if strings.HasSuffix(r, "--confirm "+hash) {
			confirmed++
		}
	}
	if len(e.keepRuns()) != 4 || confirmed != 2 || strings.Count(e.readState(), "layout: keep-data\n") != 2 {
		t.Errorf("--verbose: installer runs %q, state %s", e.keepRuns(), e.readState())
	}
}

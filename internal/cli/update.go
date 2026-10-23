package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/ofzhur/ump/internal/config"
)

// The scripts, images and profiles ump uses come from the storage at
// every use, so publishing them needs no new binary. The binary itself
// is what goes stale: once a day, after a successful command, ump reads
// bin/latest/VERSION from the storage and says when a newer release is
// there. The local profile copies are refreshed by ump config os update.

// updateCheckInterval is how often the storage is asked.
const updateCheckInterval = 24 * time.Hour

// updateState is what the last check found, kept in the local cache
// directory: it belongs to this machine's binary, so it stays out of a
// home shared through a backend, where every machine would rewrite it.
type updateState struct {
	Checked time.Time `json:"checked"`
	Latest  string    `json:"latest"`
}

func updateStatePath() string { return filepath.Join(config.CacheDir(), "update-check.json") }

// oldUpdateStatePath is where releases up to v0.4.4 kept it: in the home,
// so a backend shared it.
func oldUpdateStatePath() string { return filepath.Join(config.UserConfigDir(), "update-check.json") }

// updateHint runs after a successful command: at most once a day, and
// never for the commands that do not touch anything, with the check
// off, or for a development build.
func (a *app) updateHint(cmd *cobra.Command) {
	switch cmd.Name() {
	case "version", "completion", "help", cobra.ShellCompRequestCmd, cobra.ShellCompNoDescRequestCmd:
		return
	}
	if Version == "dev" || os.Getenv("UMP_NO_UPDATE_CHECK") != "" {
		return
	}
	cfg, _, err := a.loadConfig()
	if err != nil || (cfg.Updates != nil && !*cfg.Updates) {
		return
	}
	// the home's copy goes; with a backend, the next push removes it there
	os.Remove(oldUpdateStatePath())
	var state updateState
	if data, err := os.ReadFile(updateStatePath()); err == nil {
		json.Unmarshal(data, &state)
	}
	if a.now().Sub(state.Checked) < updateCheckInterval {
		return
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 2*time.Second)
	defer cancel()
	client := a.httpClient
	if client == nil {
		client = &http.Client{Timeout: 2 * time.Second}
	}
	latest, err := fetchVersion(ctx, client, cfg.StorageURL()+"/bin/latest/VERSION")
	// a failed check counts too: offline, or with a storage without
	// binaries, the next try is a day later, not at the next command
	state.Checked = a.now()
	if err == nil {
		state.Latest = latest
	}
	if data, err := json.Marshal(state); err == nil {
		os.MkdirAll(filepath.Dir(updateStatePath()), 0o700)
		os.WriteFile(updateStatePath(), data, 0o600)
	}
	if err != nil {
		return // no noise
	}
	if newerVersion(latest, Version) {
		fmt.Fprintf(a.stderr, "ump %s is available (this is %s): curl -fsSL %s/get.sh | sh\n", latest, Version, cfg.StorageURL())
	}
}

func fetchVersion(ctx context.Context, client *http.Client, url string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, resp.StatusCode)
	}
	var b [64]byte
	n, _ := resp.Body.Read(b[:])
	v := strings.TrimSpace(string(b[:n]))
	if !strings.HasPrefix(v, "v") || strings.ContainsAny(v, " \n\t<") {
		return "", fmt.Errorf("%s: not a version: %q", url, v)
	}
	return v, nil
}

// newerVersion reports whether latest is a later release than current:
// vMAJOR.MINOR.PATCH compared as numbers, a pre-release suffix sorting
// before the release it precedes; two versions that do not parse are
// compared as strings.
func newerVersion(latest, current string) bool {
	if latest == current {
		return false
	}
	ln, lpre, lok := parseVersion(latest)
	cn, cpre, cok := parseVersion(current)
	if !lok || !cok {
		return latest > current
	}
	for i := range ln {
		if ln[i] != cn[i] {
			return ln[i] > cn[i]
		}
	}
	// same numbers: a release is newer than its pre-releases
	return lpre == "" && cpre != "" || (lpre != "" && cpre != "" && lpre > cpre)
}

func parseVersion(v string) (n [3]int, pre string, ok bool) {
	v = strings.TrimPrefix(v, "v")
	v, pre, _ = strings.Cut(v, "-")
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return n, "", false
	}
	for i, p := range parts {
		x, err := strconv.Atoi(p)
		if err != nil || x < 0 {
			return n, "", false
		}
		n[i] = x
	}
	return n, pre, true
}

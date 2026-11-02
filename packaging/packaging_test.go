package packaging

import (
	"os"
	"slices"
	"strings"
	"testing"
)

// sections are a unit's lines by section, without comments.
func sections(unit string) map[string][]string {
	out := map[string][]string{}
	section := ""
	for _, line := range strings.Split(unit, "\n") {
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
		case strings.HasPrefix(line, "["):
			section = line
		default:
			out[section] = append(out[section], line)
		}
	}
	return out
}

func TestUnits(t *testing.T) {
	s, c := sections(Service), sections(Check)
	if got := s["[Service]"]; !slices.Contains(got, "ExecStart="+Binary+" pxe serve --site %i") || !slices.Contains(got, "Type=notify") {
		t.Errorf("the service: %q", got)
	}
	if got := s["[Install]"]; !slices.Equal(got, []string{"WantedBy=multi-user.target"}) {
		t.Errorf("the service's [Install]: %q", got)
	}
	// the check: the service's settings, user and sandbox, run once with
	// --check, in directories of its own, never enabled
	differ := map[string]bool{"Type": true, "ExecStart": true, "StateDirectory": true, "CacheDirectory": true, "Restart": true, "RestartSec": true}
	same := func(lines []string) []string {
		var out []string
		for _, l := range lines {
			if k, _, _ := strings.Cut(l, "="); !differ[k] {
				out = append(out, l)
			}
		}
		return out
	}
	if a, b := same(s["[Service]"]), same(c["[Service]"]); !slices.Equal(a, b) {
		t.Errorf("the check's [Service] differs from the service's:\n%q\n%q", b, a)
	}
	for _, want := range []string{"Type=oneshot", "ExecStart=" + Binary + " pxe serve --site %i --check", "StateDirectory=ump-pxe-check/%i", "CacheDirectory=ump-pxe-check/%i"} {
		if !slices.Contains(c["[Service]"], want) {
			t.Errorf("the check lacks %s", want)
		}
	}
	for _, l := range c["[Service]"] {
		if strings.HasPrefix(l, "Restart") {
			t.Errorf("the check has %s", l)
		}
	}
	if _, ok := c["[Install]"]; ok || !slices.Equal(s["[Unit]"][1:], c["[Unit]"][1:]) {
		t.Errorf("the check's [Unit] or [Install]: %q", c)
	}
}

func TestWithBinary(t *testing.T) {
	got := WithBinary(Check, "/usr/local/bin/ump")
	if want := strings.Replace(Check, "ExecStart=/usr/bin/ump pxe", "ExecStart=/usr/local/bin/ump pxe", 1); got != want || got == Check {
		t.Errorf("WithBinary:\n%s", got)
	}
	if strings.Count(Service, "/usr/bin/ump") != 1 || strings.Count(Check, "/usr/bin/ump") != 1 {
		t.Error("the units name ump elsewhere than in ExecStart")
	}
}

// TestDocs: docs/pxe.md shows the service as it is.
func TestDocs(t *testing.T) {
	docs, err := os.ReadFile("../docs/pxe.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(docs), "```ini\n"+Service+"```\n") {
		t.Error("docs/pxe.md does not show ump-pxe@.service as packaging/ump-pxe@.service has it")
	}
}

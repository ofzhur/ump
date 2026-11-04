package plan

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decode(t *testing.T, text string) (Values, error) {
	t.Helper()
	var v Values
	dec := yaml.NewDecoder(strings.NewReader(text))
	dec.KnownFields(true)
	err := dec.Decode(&v)
	return v, err
}

func TestList(t *testing.T) {
	for _, c := range []struct {
		text string
		want []string
	}{
		{"group: web\n", []string{"web"}},
		{"group: [web, db]\n", []string{"web", "db"}},
		{"group:\n  - web\n  - db\n", []string{"web", "db"}},
		{"group: []\n", []string{}},
	} {
		v, err := decode(t, c.text)
		if err != nil || v.Group == nil || strings.Join(v.Group, ",") != strings.Join(c.want, ",") {
			t.Errorf("%q: %#v %v", c.text, v.Group, err)
		}
	}
	for _, c := range []struct{ text, err string }{
		{"group: [[web]]\n", "line 1: a list of values, one per line or in [ ]"},
		{"group:\n  - web\n  - {db: 1}\n", "line 3: a list of values, one per line or in [ ]"},
		{"group: {web: 1}\n", "line 1: a value, or a list of values"},
	} {
		if _, err := decode(t, c.text); err == nil || err.Error() != c.err {
			t.Errorf("%q: %v, want %s", c.text, err, c.err)
		}
	}
}

func TestOver(t *testing.T) {
	defaults, err := decode(t, "profile: web\nlayout: lvm-raid1\ngroup: [web]\nipv6: true\nprivate-network: [default]\n")
	if err != nil {
		t.Fatal(err)
	}
	server, err := decode(t, "profile: db\ngroup: []\nipv6: false\nhostname: db-01.example.net\n")
	if err != nil {
		t.Fatal(err)
	}
	v := defaults.Over(server)
	switch {
	case *v.Profile != "db", *v.Hostname != "db-01.example.net", *v.IPv6:
		t.Errorf("the server's values: %+v", v)
	case *v.Layout != "lvm-raid1", strings.Join(v.PrivateNet, ",") != "default":
		t.Errorf("the defaults' values: %+v", v)
	case v.Group == nil || len(v.Group) != 0:
		t.Errorf("an empty list replaces the defaults' as a whole: %#v", v.Group)
	case v.OS != nil || v.Via != nil || v.Disks != nil:
		t.Errorf("given by neither: %+v", v)
	}
	if *defaults.Profile != "web" || !*defaults.IPv6 || len(defaults.Group) != 1 || defaults.Hostname != nil {
		t.Errorf("the defaults changed: %+v", defaults)
	}
	if v := (Values{}).Over(Values{}); v.Profile != nil || v.Group != nil {
		t.Errorf("nothing over nothing: %+v", v)
	}
}

func TestKeys(t *testing.T) {
	want := "profile layout firewall os hostname private-network private-ip timezone disks ssh-key set user userdata group ipv6 mitigations via storage"
	if got := strings.Join(Keys(), " "); got != want {
		t.Errorf("keys %s, want %s", got, want)
	}
}

// TestInline: a file with keys of its own besides a server's values
// (pxe.yaml's machines) takes them inline, strictly.
func TestInline(t *testing.T) {
	type machine struct {
		MAC    string `yaml:"mac"`
		Values `yaml:",inline"`
	}
	var m machine
	dec := yaml.NewDecoder(strings.NewReader("mac: 0c:c4:7a:aa:bb:01\nprofile: web\ngroup: web\n"))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil || m.MAC != "0c:c4:7a:aa:bb:01" || *m.Profile != "web" || strings.Join(m.Group, ",") != "web" {
		t.Errorf("inline: %+v %v", m, err)
	}
	dec = yaml.NewDecoder(strings.NewReader("mac: 0c:c4:7a:aa:bb:01\nprofil: web\n"))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err == nil || !strings.Contains(err.Error(), "field profil not found") {
		t.Errorf("an unknown key: %v", err)
	}
}

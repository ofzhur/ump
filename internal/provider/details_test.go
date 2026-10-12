package provider

import (
	"encoding/json"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestDetailsMarshal(t *testing.T) {
	d := Details{Machine: Machine{Provider: "p", Account: "a", ID: "1", Name: "n", Kind: KindCloud, Status: "running"}}
	s := Section{Name: "Power"}
	s.Add("state", "running").Add("reset_types", []string{"sw", "hw"}).Add("locked", false).Add("disk", Bytes(40<<30)).Add("iso", nil)
	d.Sections = append(d.Sections, s)

	j, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"provider":"p","account":"a","id":"1","name":"n","kind":"cloud","status":"running","details":[{"name":"Power","fields":{"state":"running","reset_types":["sw","hw"],"locked":false,"disk":42949672960,"iso":null}}]}`
	if string(j) != want {
		t.Errorf("json:\n got %s\nwant %s", j, want)
	}

	y, err := yaml.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"provider: p\n", "details:\n", "- name: Power\n", "  fields:\n", "    state: running\n", "    reset_types:\n", "    - sw\n", "    locked: false\n", "    disk: 42949672960\n", "    iso: null\n"} {
		if !strings.Contains(string(y), w) {
			t.Errorf("yaml lacks %q:\n%s", w, y)
		}
	}
	if !strings.HasPrefix(string(y), "provider: p\naccount: a\n") {
		t.Errorf("machine fields not inlined:\n%s", y)
	}
}

func TestDisplayLabel(t *testing.T) {
	cases := map[string]string{
		"paid_until": "Paid until", "ipv6_network": "IPv6 network", "state": "State", "dns_ptr": "DNS PTR",
		"os_version": "OS version", "ram": "RAM", "link_speed_mbps": "Link speed Mbps", "cpanel": "cPanel",
	}
	for key, want := range cases {
		if got := (Item{Key: key}).DisplayLabel(); got != want {
			t.Errorf("%s: got %q, want %q", key, got, want)
		}
	}
	if got := (Item{Key: "x", Label: "Explicit"}).DisplayLabel(); got != "Explicit" {
		t.Errorf("explicit label: %q", got)
	}
}

func TestBytesString(t *testing.T) {
	cases := map[Bytes]string{
		0: "0 B", 512: "512 B", 1024: "1 KB", 1536: "1.5 KB", 4 << 30: "4 GB", 40 << 30: "40 GB",
		21990232555520: "20 TB", 32768 << 20: "32 GB", 8000 << 20: "7.8 GB",
	}
	for b, want := range cases {
		if got := b.String(); got != want {
			t.Errorf("%d: got %q, want %q", int64(b), got, want)
		}
	}
}

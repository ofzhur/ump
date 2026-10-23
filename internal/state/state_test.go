package state

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ofzhur/ump/internal/backend"
	"github.com/ofzhur/ump/internal/provider"
)

func TestStoreUpdateMerges(t *testing.T) {
	// two writers about different servers: the second's write conflicts
	// and is retried on the fresh document, so both changes land
	dir := t.TempDir()
	s := &Store{Backend: &backend.Local{Dir: dir}}
	ctx := context.Background()
	var once sync.Once
	err := s.Update(ctx, func(d *Document) error {
		d.Servers["fake/1"] = Server{Profile: "web"}
		once.Do(func() {
			if err := s.Update(ctx, func(d *Document) error {
				d.Servers["fake/2"] = Server{Price: &provider.Price{Amount: 5, Currency: "EUR", Period: "month"}}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	d, _, err := s.Read(ctx)
	if err != nil || d.Servers["fake/1"].Profile != "web" || d.Servers["fake/2"].Price == nil {
		t.Fatalf("merged state: %+v %v", d, err)
	}
	data, _ := os.ReadFile(filepath.Join(dir, File))
	if !strings.HasPrefix(string(data), "# UMP state") || !strings.Contains(string(data), "version: 1\n") {
		t.Errorf("file:\n%s", data)
	}
	if err := s.Update(ctx, func(d *Document) error { d.Servers["fake/1"] = Server{}; return nil }); err != nil {
		t.Fatal(err)
	}
	d, _, _ = s.Read(ctx)
	if _, ok := d.Servers["fake/1"]; ok {
		t.Error("empty record kept")
	}
	unlock, err := s.Lock(ctx, "fake/1", "reinstall")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Lock(ctx, "fake/1", "rescue"); err == nil || !strings.Contains(err.Error(), "fake/1 is locked by") || !strings.Contains(err.Error(), "ump config state unlock fake/1 forces it") {
		t.Errorf("second lock: %v", err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
}

func TestParse(t *testing.T) {
	if _, err := Parse([]byte("version: 7\n")); err == nil || !strings.Contains(err.Error(), "version 7") {
		t.Errorf("newer version: %v", err)
	}
	if _, err := Parse([]byte("servers:\n  fake/1:\n    price: {amount: 1}\n")); err == nil || !strings.Contains(err.Error(), "servers.fake/1.price") {
		t.Errorf("bad price: %v", err)
	}
	if _, err := Parse([]byte("colour: blue\n")); err == nil {
		t.Error("unknown field accepted")
	}
	if d, err := Parse(nil); err != nil || d.Version != 1 || d.Servers == nil {
		t.Errorf("empty: %+v %v", d, err)
	}
}

func TestParseRolesBecomeGroups(t *testing.T) {
	// an older ump wrote roles; they load as groups and are written as such
	d, err := Parse([]byte("servers:\n  fake/1:\n    roles: [web-1, base.x, web_1, common, localhost, 2fa]\n    groups: [db]\n"))
	if err != nil {
		t.Fatal(err)
	}
	s := d.Servers["fake/1"]
	if strings.Join(s.Groups, ",") != "db,web_1,base_x,_2fa" || s.Roles != nil {
		t.Errorf("groups %v, roles %v", s.Groups, s.Roles)
	}
	data, err := d.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "    groups:\n      - db\n      - web_1\n      - base_x\n      - _2fa\n") || strings.Contains(string(data), "roles") {
		t.Errorf("written:\n%s", data)
	}
}

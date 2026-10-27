package provider

import (
	"errors"
	"fmt"
	"testing"
)

func TestMachineMatches(t *testing.T) {
	m := Machine{Provider: "hetzner-robot", ID: "123456", Name: "web-01", IPv4: "203.0.113.10", IPv6: "2001:db8:1:2::/64"}
	for _, ref := range []string{"hetzner-robot/123456", "123456", "web-01", "WEB-01", "203.0.113.10", "2001:db8:1:2::2", "2001:db8:1:2::"} {
		if !m.Matches(ref) {
			t.Errorf("Matches(%q) = false, want true", ref)
		}
	}
	for _, ref := range []string{"", "hetzner-cloud/123456", "web-02", "203.0.113.11", "2001:db8:1:3::1", "12345"} {
		if m.Matches(ref) {
			t.Errorf("Matches(%q) = true, want false", ref)
		}
	}
	single := Machine{ID: "x", IPv6: "2001:db8::1"}
	if !single.Matches("2001:db8::1") || single.Matches("2001:db8::2") {
		t.Error("single IPv6 address should match exactly")
	}
}

func TestSortMachines(t *testing.T) {
	ms := []Machine{
		{Provider: "ovh", Account: "a", Name: "z"},
		{Provider: "hetzner-robot", Account: "b", Name: "b"},
		{Provider: "hetzner-robot", Account: "a", Name: "m"},
		{Provider: "hetzner-robot", Account: "a", Name: "a", ID: "2"},
		{Provider: "hetzner-robot", Account: "a", Name: "a", ID: "1"},
	}
	SortMachines(ms)
	want := []string{"hetzner-robot/1", "hetzner-robot/2", "hetzner-robot/", "hetzner-robot/", "ovh/"}
	for i, m := range ms {
		if m.Ref() != want[i] {
			t.Fatalf("position %d: got %s (%s/%s), want %s", i, m.Ref(), m.Account, m.Name, want[i])
		}
	}
	if ms[2].Name != "m" || ms[3].Account != "b" || ms[4].Name != "z" {
		t.Errorf("unexpected order: %+v", ms)
	}
}

func TestTypeValidate(t *testing.T) {
	typ := Type{Name: "t", Fields: []Field{
		{Key: "login", Required: true},
		{Key: "endpoint", Choices: []string{"eu", "us"}},
		{Key: "url"},
	}, New: func(string, map[string]string, Options) (Provider, error) { return nil, nil }}

	if errs := typ.Validate(map[string]string{"login": "x", "endpoint": "eu"}); len(errs) != 0 {
		t.Errorf("valid settings rejected: %v", errs)
	}
	if errs := typ.Validate(map[string]string{"login": "x", "endpoint": "https://example.test/1.0"}); len(errs) != 0 {
		t.Errorf("URL should be accepted for a choice field: %v", errs)
	}
	errs := typ.Validate(map[string]string{"login": " ", "endpoint": "asia", "bogus": "1"})
	if len(errs) != 3 {
		t.Fatalf("got %d errors, want 3: %v", len(errs), errs)
	}
	var fe *FieldError
	if !errors.As(errs[0], &fe) {
		t.Fatalf("expected FieldError, got %T", errs[0])
	}
	msgs := map[string]bool{}
	for _, e := range errs {
		msgs[e.Error()] = true
	}
	for _, want := range []string{"t: login is required", "t: endpoint must be one of eu, us", "t: bogus is not a setting of this provider type"} {
		if !msgs[want] {
			t.Errorf("missing error %q in %v", want, errs)
		}
	}
}

func TestAPIErrorString(t *testing.T) {
	cases := map[string]*APIError{
		"x: boom":                              {Provider: "x", Message: "boom"},
		"x: boom (HTTP 401)":                   {Provider: "x", Message: "boom", Status: 401},
		"x: boom (UNAUTHORIZED, HTTP 401)":     {Provider: "x", Message: "boom", Status: 401, Code: "UNAUTHORIZED"},
		"x: API request failed (RATE_LIMITED)": {Provider: "x", Code: "RATE_LIMITED"},
	}
	for want, e := range cases {
		if got := e.Error(); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestIsNotFound(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{&APIError{Provider: "x", Status: 404, Code: "not_found"}, true},
		{fmt.Errorf("account a: %w", &APIError{Provider: "x", Status: 404}), true},
		{&APIError{Provider: "x", Status: 403, Code: "NOT_GRANTED_CALL"}, false},
		{&APIError{Provider: "x", Message: "connection refused"}, false},
		{errors.New("404"), false},
		{nil, false},
	} {
		if got := IsNotFound(c.err); got != c.want {
			t.Errorf("IsNotFound(%v) = %v", c.err, got)
		}
	}
}

func TestRegistry(t *testing.T) {
	typ := Type{Name: "registry-test", New: func(account string, s map[string]string, _ Options) (Provider, error) {
		return nil, errors.New("built " + account + " with " + s["k"])
	}, Fields: []Field{{Key: "k", Required: true}}}
	Register(typ)
	if _, ok := Lookup("registry-test"); !ok {
		t.Fatal("type not found after Register")
	}
	if _, err := Build("registry-test", "acc", map[string]string{"k": "v"}, Options{}); err == nil || err.Error() != "built acc with v" {
		t.Errorf("Build did not call New: %v", err)
	}
	if _, err := Build("registry-test", "acc", map[string]string{}, Options{}); err == nil {
		t.Error("Build should validate required fields")
	}
	var ute *UnknownTypeError
	if _, err := Build("nope", "acc", nil, Options{}); !errors.As(err, &ute) {
		t.Errorf("expected UnknownTypeError, got %v", err)
	}
	defer func() {
		if recover() == nil {
			t.Error("duplicate Register should panic")
		}
	}()
	Register(typ)
}

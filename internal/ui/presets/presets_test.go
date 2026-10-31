package presets

import (
	"os"
	"path/filepath"
	"testing"
)

func TestBuiltInsValid(t *testing.T) {
	all, err := NewStore(t.TempDir()).List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("want 3 built-ins, got %d", len(all))
	}
	for _, p := range all {
		if !p.BuiltIn {
			t.Errorf("%s: not marked built-in", p.Name)
		}
		if err := Validate(p); err != nil {
			t.Errorf("built-in %s invalid: %v", p.Name, err)
		}
	}
}

func TestSaveGetDelete(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "presets")
	s := NewStore(dir)
	p := Preset{Name: "My lab/../x", Faults: map[string]float64{"malformed": 0.5}, Profiles: Profiles{Seed: 7}}
	if err := s.Save(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-lab-x.json")); err != nil {
		t.Fatalf("file name not slugged inside the dir: %v", err)
	}
	got, ok := s.Get("My lab/../x")
	if !ok || got.Faults["malformed"] != 0.5 || got.Profiles.Seed != 7 || got.Profiles.Rules == nil || got.BuiltIn {
		t.Fatalf("Get: %+v %v", got, ok)
	}
	all, _ := s.List()
	if len(all) != 4 || all[3].Name != "My lab/../x" {
		t.Fatalf("List: %+v", all)
	}
	if err := s.Delete("My lab/../x"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Get("My lab/../x"); ok {
		t.Fatal("still there after Delete")
	}
}

func TestSaveRefusals(t *testing.T) {
	s := NewStore(t.TempDir())
	for _, p := range []Preset{
		{Name: "auth STORM"},
		{Name: ""},
		{Name: "x", Faults: map[string]float64{"bogus": 1}},
		{Name: "x", Faults: map[string]float64{"auth_fail": 2}},
		{Name: "x", Profiles: Profiles{Rules: []string{"refuse@60%", "hang:1s@60%"}}},
		{Name: "x", Profiles: Profiles{Rules: []string{"refuse@1%,hang:1s@1%"}}},
	} {
		if err := s.Save(p); err == nil {
			t.Errorf("%+v: want an error", p)
		}
	}
	if err := s.Delete("Flaky WAN"); err == nil {
		t.Error("deleting a built-in must fail")
	}
}

func TestListSkipsBadFiles(t *testing.T) {
	dir := t.TempDir()
	_ = os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "invalid.json"), []byte(`{"name":"i","faults":{"auth_fail":9}}`), 0o600)
	all, err := NewStore(dir).List()
	if err == nil || len(all) != 3 {
		t.Fatalf("want built-ins only and an error, got %d presets, err %v", len(all), err)
	}
}

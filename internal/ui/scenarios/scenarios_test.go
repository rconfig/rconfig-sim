package scenarios

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/rcfg-sim/rcfg-sim/internal/ui/catalogue"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/jobs"
	"github.com/rcfg-sim/rcfg-sim/internal/ui/uitest"
	shipped "github.com/rcfg-sim/rcfg-sim/scenarios"
)

func cat(t testing.TB) *catalogue.Catalogue {
	t.Helper()
	c, err := catalogue.Decode(uitest.CatalogueJSON(t))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

const goodJSON = `{"id":"lab","name":"Lab","description":"Two routers.","version":2,"drivers":["cisco_ios"],
	"auth":{"mode":"any_password"},"ssh_auth":"password",
	"topology":{"nodes":[{"id":"r1","tier":0},{"id":"r2","tier":1}],"links":[["r1","r2"]]}}`

const goodManifest = "hostname,ip,port,vendor,template,username,password,enable_password,config_file,size_bucket\n" +
	"r1,10.0.0.1,100,Cisco,cisco_ios,admin,pw,en,configs/r1.cfg,sm\n" +
	"r2,10.0.0.1,101,Cisco,cisco_ios,admin,pw,en,configs/r2.cfg,sm\n"

// pkg is a valid two-device package, changed by edit.
func pkg(edit func(fstest.MapFS)) fstest.MapFS {
	m := fstest.MapFS{
		"scenario.json":                {Data: []byte(goodJSON)},
		"manifest.csv":                 {Data: []byte(goodManifest)},
		"configs/r1.cfg":               {Data: []byte("hostname r1\n")},
		"configs/r2.cfg":               {Data: []byte("hostname r2\n")},
		"commands/r1/show_version.txt": {Data: []byte("r1 version\n")},
		"README.md":                    {Data: []byte("# Lab\n")},
	}
	if edit != nil {
		edit(m)
	}
	return m
}

func TestLoadValid(t *testing.T) {
	s := Load(pkg(nil), "lab", cat(t))
	if len(s.Problems) > 0 {
		t.Fatalf("problems: %v", s.Problems)
	}
	if s.Name != "Lab" || s.Version != 2 || len(s.Devices) != 2 || s.Devices[1].ConfigFile != "configs/r2.cfg" ||
		len(s.Commands) != 1 || s.README != "# Lab\n" || s.SSHAuthMode() != "password" {
		t.Fatalf("loaded %+v", s)
	}
	if pw, en := s.Credentials(); pw != "" || en != "" {
		t.Fatalf("any_password credentials %q %q", pw, en)
	}
	s.Auth.Mode = AuthManifest
	if pw, en := s.Credentials(); pw != "pw" || en != "en" {
		t.Fatalf("manifest credentials %q %q", pw, en)
	}
}

// TestLoadProblems: each validation failure is reported with its reason.
func TestLoadProblems(t *testing.T) {
	set := func(name, data string) func(fstest.MapFS) {
		return func(m fstest.MapFS) {
			if data == "" {
				delete(m, name)
			} else {
				m[name] = &fstest.MapFile{Data: []byte(data)}
			}
		}
	}
	json := func(old, new string) func(fstest.MapFS) {
		return set("scenario.json", strings.Replace(goodJSON, old, new, 1))
	}
	for desc, c := range map[string]struct {
		edit func(fstest.MapFS)
		want string
	}{
		"no scenario.json":  {set("scenario.json", ""), "scenario.json"},
		"unknown field":     {json(`"version":2`, `"version":2,"bogus":1`), `unknown field "bogus"`},
		"id not the folder": {json(`"id":"lab"`, `"id":"other"`), `does not match its folder`},
		"version":           {json(`"version":2`, `"version":0`), "version must be 1 or more"},
		"auth mode":         {json(`any_password`, `secret`), `auth.mode "secret"`},
		"ssh auth":          {json(`"ssh_auth":"password"`, `"ssh_auth":"maybe"`), `ssh_auth "maybe"`},
		"no manifest":       {set("manifest.csv", ""), "manifest.csv"},
		"bad header":        {set("manifest.csv", "host,ip\nr1,1\n"), "header must start"},
		"no config":         {set("configs/r2.cfg", ""), "r2 has no config file (configs/r2.cfg)"},
		"config outside":    {set("manifest.csv", strings.Replace(goodManifest, "configs/r2.cfg", "../r2.cfg", 1)), "r2 has no config file"},
		"absolute config":   {set("manifest.csv", strings.Replace(goodManifest, "configs/r2.cfg", "/etc/passwd", 1)), "r2 has no config file"},
		"stray commands":    {set("commands/r9/show_version.txt", "x"), "commands/r9 matches no manifest hostname"},
		"unknown driver":    {json(`"drivers":["cisco_ios"]`, `"drivers":["cisco_ios","nxos"]`), `driver "nxos" is not registered`},
		"manifest driver":   {set("manifest.csv", strings.Replace(goodManifest, "Cisco,cisco_ios,admin,pw,en,configs/r2", "Cisco,eos,admin,pw,en,configs/r2", 1)), `driver "eos" is not registered`},
		"node not a host":   {json(`{"id":"r2","tier":1}`, `{"id":"r3","tier":1}`), "topology node r3 is not a manifest hostname"},
		"link not a node":   {json(`["r1","r2"]`, `["r1","r9"]`), "topology link r1 to r9: r9 is not a node"},
		"duplicate host":    {set("manifest.csv", strings.Replace(goodManifest, "r2,10.0.0.1", "r1,10.0.0.1", 1)), "hostname r1 appears twice"},
		"manifest creds": {func(m fstest.MapFS) {
			json(`any_password`, `manifest`)(m)
			set("manifest.csv", strings.Replace(goodManifest, "admin,pw,en,configs/r2", "admin,other,en,configs/r2", 1))(m)
		}, "same password and enable password on every row"},
	} {
		s := Load(pkg(c.edit), "lab", cat(t))
		if !strings.Contains(s.Problem(), c.want) {
			t.Errorf("%s: problems %q, want %q", desc, s.Problem(), c.want)
		}
	}
}

// TestShippedValidate: every shipped package validates against the real
// catalogue, and the library lists only folders with scenario.json.
func TestShippedValidate(t *testing.T) {
	dir := t.TempDir()
	// A user package, a folder that is not one, and a shipped id clash.
	writeTree(t, filepath.Join(dir, "lab"), pkg(nil))
	if err := os.MkdirAll(filepath.Join(dir, "example", "commands"), 0o755); err != nil {
		t.Fatal(err)
	}
	clash := pkg(func(m fstest.MapFS) {
		m["scenario.json"] = &fstest.MapFile{Data: []byte(strings.Replace(goodJSON, `"id":"lab"`, `"id":"mesh-campus"`, 1))}
	})
	writeTree(t, filepath.Join(dir, "mesh-campus"), clash)

	lib := NewLibrary(shipped.Shipped, dir, catalogue.Static(cat(t)))
	all, err := lib.List()
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, s := range all {
		ids = append(ids, s.ID)
		if s.Shipped && len(s.Problems) > 0 {
			t.Errorf("shipped %s: %v", s.ID, s.Problems)
		}
	}
	if strings.Join(ids, ",") != "mesh-campus,lab,mesh-campus" {
		t.Fatalf("library: %v", ids)
	}
	if !strings.Contains(all[2].Problem(), "already used by a shipped scenario") {
		t.Fatalf("clash: %q", all[2].Problem())
	}
	mc, err := lib.Get("mesh-campus")
	if err != nil || !mc.Shipped || len(mc.Devices) != 3 || len(mc.Commands) != 3 || mc.Auth.Mode != AuthAnyPassword {
		t.Fatalf("mesh-campus: %+v %v", mc, err)
	}
	if _, err := lib.Get("example"); err == nil {
		t.Fatal("a folder without scenario.json is listed")
	}
}

func writeTree(t *testing.T, dir string, m fstest.MapFS) {
	t.Helper()
	for name, f := range m {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, f.Data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func layout(ips, per int) jobs.Layout {
	l := jobs.Layout{IPBase: "127.0.0.1", IPCount: ips, PortStart: 12000, DevicesPerIP: per}
	for i := 1; i <= ips; i++ {
		l.Instances = append(l.Instances, "127.0.0."+string(rune('0'+i)))
	}
	return l
}

// TestInstall: the devices move to the host's instances (first instance
// first, ports from PORT_START), hostnames kept, config paths point into
// the build, and the files are copied.
func TestInstall(t *testing.T) {
	s := Load(pkg(func(m fstest.MapFS) {
		m["manifest.csv"] = &fstest.MapFile{Data: []byte(goodManifest + "r3,10.0.0.9,7,Cisco,cisco_ios,admin,pw,en,configs/r1.cfg,md\n")}
	}), "lab", cat(t))
	if s.Problem() != "" {
		t.Fatal(s.Problem())
	}
	l := layout(3, 2)
	if Fit(s, layout(1, 2)) == "" || Fit(s, l) != "" || Instances(s, l) != 2 {
		t.Fatalf("fit: %q %q %d", Fit(s, layout(1, 2)), Fit(s, l), Instances(s, l))
	}
	dir := t.TempDir()
	if err := Install(s, dir, l); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(filepath.Join(dir, "manifest.csv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{"r1", "127.0.0.1", "12000", filepath.Join(dir, "configs", "r1.cfg"), "pw"},
		{"r2", "127.0.0.1", "12001", filepath.Join(dir, "configs", "r2.cfg"), "pw"},
		{"r3", "127.0.0.2", "12000", filepath.Join(dir, "configs", "r1.cfg"), "pw"},
	}
	if len(rows) != 4 || strings.Join(rows[0], ",") != strings.Join(manifestHeader, ",") {
		t.Fatalf("manifest: %v", rows)
	}
	for i, w := range want {
		r := rows[i+1]
		if got := []string{r[0], r[1], r[2], r[8], r[6]}; strings.Join(got, "|") != strings.Join(w, "|") {
			t.Errorf("row %d: %v, want %v", i+1, got, w)
		}
	}
	for _, p := range []string{"configs/r1.cfg", "configs/r2.cfg", "commands/r1/show_version.txt", "scenario.json", "README.md"} {
		if _, err := os.Stat(filepath.Join(dir, p)); err != nil {
			t.Errorf("not copied: %s", p)
		}
	}
	if err := Install(Load(pkg(func(m fstest.MapFS) { delete(m, "configs/r1.cfg") }), "lab", cat(t)), t.TempDir(), l); err == nil {
		t.Fatal("installed an invalid package")
	}
}

// TestHash: the same package on the same layout hashes the same; a
// changed file or layout does not.
func TestHash(t *testing.T) {
	l := layout(2, 3)
	h := func(m fstest.MapFS, l jobs.Layout) string {
		x, err := Hash(Load(m, "lab", cat(t)), l)
		if err != nil {
			t.Fatal(err)
		}
		return x
	}
	a := h(pkg(nil), l)
	if a != h(pkg(nil), l) {
		t.Fatal("hash not stable")
	}
	if a == h(pkg(func(m fstest.MapFS) { m["configs/r1.cfg"] = &fstest.MapFile{Data: []byte("hostname r1-changed\n")} }), l) {
		t.Fatal("a changed config kept the hash")
	}
	moved := l
	moved.PortStart = 13000
	if a == h(pkg(nil), moved) {
		t.Fatal("a changed layout kept the hash")
	}
	if name := BuildName(Load(pkg(nil), "lab", cat(t)), a); name != "lab-v2-"+a[:12] {
		t.Fatalf("build name %s", name)
	}
}

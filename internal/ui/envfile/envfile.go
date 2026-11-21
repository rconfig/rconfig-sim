// Package envfile reads and updates rcfg-sim instance env files
// (/etc/rcfg-sim/<IP>.env), the files systemd hands to rcfg-sim@<IP>.service
// through EnvironmentFile=.
//
// Updates rewrite only the keys the controller owns (see Owned). Comments,
// blank lines, ordering and every other line are kept byte for byte. Writes
// are atomic: a temp file in the same directory, with the original file's
// mode and group, fsynced and renamed over the original.
package envfile

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// Keys the controller owns and may rewrite.
const (
	FaultRates            = "FAULT_RATES"
	DeviceProfiles        = "DEVICE_PROFILES"
	ProfileSeed           = "PROFILE_SEED"
	ResponseDelayMSMin    = "RESPONSE_DELAY_MS_MIN"
	ResponseDelayMSMax    = "RESPONSE_DELAY_MS_MAX"
	MaxConcurrentSessions = "MAX_CONCURRENT_SESSIONS"
	ActiveDevices         = "ACTIVE_DEVICES"
	// SSHAuth is the instance's --ssh-auth (Phase 8). Changing it needs a
	// restart: the admin API cannot.
	SSHAuth = "SSH_AUTH"
	// CommandsRoot, Password and EnablePassword are owned while a scenario
	// testbench runs and restored when a generated one starts (Phase 9).
	// They also need a restart.
	CommandsRoot   = "COMMANDS_ROOT"
	Password       = "PASSWORD"
	EnablePassword = "ENABLE_PASSWORD"
	// AllowEmpty is set true on every instance the controller starts a
	// testbench on, so an instance left without devices idles rather than
	// failing (Phase 9 item 5).
	AllowEmpty = "ALLOW_EMPTY"
)

// Legacy fault keys, cleared whenever FAULT_RATES is written: rcfg-sim
// refuses to start with both forms set.
const (
	FaultRate  = "FAULT_RATE"
	FaultTypes = "FAULT_TYPES"
)

// Owned lists every key Update may write.
var Owned = []string{
	FaultRates, DeviceProfiles, ProfileSeed, ResponseDelayMSMin,
	ResponseDelayMSMax, MaxConcurrentSessions, ActiveDevices, SSHAuth,
	CommandsRoot, Password, EnablePassword, AllowEmpty,
}

// clearedLegacy is what the legacy fault keys are set to when FAULT_RATES is
// written. "0" rather than empty: the unit passes --fault-rate=${FAULT_RATE}
// and an empty float flag does not parse.
var clearedLegacy = map[string]string{FaultRate: "0", FaultTypes: ""}

// File is a parsed env file. Lines are kept verbatim; Values holds the
// parsed KEY=VALUE pairs (the last assignment wins, as in systemd).
type File struct {
	Path   string
	lines  []string
	Values map[string]string
}

// Read parses the env file at path.
func Read(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(path, b), nil
}

// Parse parses env file content. path is recorded for Write only.
func Parse(path string, b []byte) *File {
	f := &File{Path: path, Values: map[string]string{}}
	text := string(b)
	text = strings.TrimSuffix(text, "\n")
	if text != "" {
		f.lines = strings.Split(text, "\n")
	}
	for _, ln := range f.lines {
		if k, v, ok := parseLine(ln); ok {
			f.Values[k] = v
		}
	}
	return f
}

// Get returns the value of key and whether the file sets it.
func (f *File) Get(key string) (string, bool) {
	v, ok := f.Values[key]
	return v, ok
}

// parseLine returns the key and unquoted value of an assignment line.
// Comments (# or ;) and blank lines are not assignments.
func parseLine(ln string) (key, val string, ok bool) {
	t := strings.TrimSpace(ln)
	if t == "" || t[0] == '#' || t[0] == ';' {
		return "", "", false
	}
	k, v, found := strings.Cut(t, "=")
	if !found {
		return "", "", false
	}
	k = strings.TrimSpace(k)
	if k == "" || strings.ContainsAny(k, " \t") {
		return "", "", false
	}
	return k, unquote(strings.TrimSpace(v)), true
}

func unquote(v string) string {
	if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
		inner := v[1 : len(v)-1]
		if v[0] == '"' {
			r := strings.NewReplacer(`\"`, `"`, `\\`, `\`)
			inner = r.Replace(inner)
		}
		return inner
	}
	return v
}

// quote returns v as systemd will read it back: bare when it is safe,
// otherwise double-quoted with " and \ escaped.
func quote(v string) string {
	if v == "" || !strings.ContainsAny(v, " \t\"'\\#;$`") {
		return v
	}
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`)
	return `"` + r.Replace(v) + `"`
}

// Set changes key to val: every existing assignment of key is rewritten in
// place (keeping its leading indentation), or, if there is none, a new line
// is appended. Set does not check ownership; Update does.
func (f *File) Set(key, val string) {
	line := key + "=" + quote(val)
	found := false
	for i, ln := range f.lines {
		if k, _, ok := parseLine(ln); ok && k == key {
			indent := ln[:len(ln)-len(strings.TrimLeft(ln, " \t"))]
			f.lines[i] = indent + line
			found = true
		}
	}
	if !found {
		f.lines = append(f.lines, line)
	}
	f.Values[key] = val
}

// Delete removes every assignment of key. Like Set, it does not check
// ownership.
func (f *File) Delete(key string) {
	f.lines = slices.DeleteFunc(f.lines, func(ln string) bool {
		k, _, ok := parseLine(ln)
		return ok && k == key
	})
	delete(f.Values, key)
}

// Restore sets or removes owned keys in the env file at path and writes it
// back atomically: a nil value removes the key (so the unit default
// applies again), any other sets it. Keys outside Owned are refused before
// anything is written.
func Restore(path string, keys map[string]*string) error {
	for k := range keys {
		if !slices.Contains(Owned, k) {
			return fmt.Errorf("%s: %w", k, ErrNotOwned)
		}
	}
	f, err := Read(path)
	if err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(keys)) {
		if v := keys[k]; v == nil {
			f.Delete(k)
		} else {
			f.Set(k, *v)
		}
	}
	return WriteAtomic(path, f.Bytes())
}

// Bytes renders the file.
func (f *File) Bytes() []byte {
	var b bytes.Buffer
	for _, ln := range f.lines {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ErrNotOwned is returned by Update for a key the controller does not own.
var ErrNotOwned = errors.New("key is not owned by the controller")

// Update sets the given owned keys in the env file at path and writes it
// back atomically. Writing FAULT_RATES also clears FAULT_RATE and
// FAULT_TYPES (only when the file sets them). No other line changes. Keys
// outside Owned are refused before anything is written.
func Update(path string, set map[string]string) error {
	for k := range set {
		if !slices.Contains(Owned, k) {
			return fmt.Errorf("%s: %w", k, ErrNotOwned)
		}
	}
	f, err := Read(path)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		f.Set(k, set[k])
	}
	if _, ok := set[FaultRates]; ok {
		for _, k := range []string{FaultRate, FaultTypes} {
			if _, present := f.Values[k]; present {
				f.Set(k, clearedLegacy[k])
			}
		}
	}
	return WriteAtomic(path, f.Bytes())
}

// WriteAtomic replaces path with data: a temp file in the same directory
// takes the original's mode and group, is fsynced, and is renamed over the
// original, then the directory is fsynced. path must already exist.
func WriteAtomic(path string, data []byte) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		if !ok {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		return err
	}
	if err := keepGroup(tmp, info); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// keepGroup gives tmp the group of the original file. A no-op when the
// group already matches (the setgid /etc/rcfg-sim makes it so).
func keepGroup(tmp *os.File, orig fs.FileInfo) error {
	want, ok := orig.Sys().(*syscall.Stat_t)
	if !ok {
		return nil
	}
	cur, err := tmp.Stat()
	if err != nil {
		return err
	}
	if have, ok := cur.Sys().(*syscall.Stat_t); ok && have.Gid == want.Gid {
		return nil
	}
	if err := tmp.Chown(-1, int(want.Gid)); err != nil {
		return fmt.Errorf("keep group %d on %s: %w", want.Gid, orig.Name(), err)
	}
	return nil
}

package sshsrv

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// cmdFiles serves operator-supplied command output from
// <root>/<hostname>/<slug>.txt (--commands-root). It is opt-in: a nil
// *cmdFiles, a missing root, or a missing hostname folder all mean "no file",
// and the driver falls through to its built-in behaviour unchanged.
//
// Each device folder is listed once, on the device's first command, and a
// file's contents are read on its first hit; both are cached for the life of
// the process. Files added, removed or edited after that are not seen until
// restart. Both caches are bounded by the files on disk, never by what a
// client types, and a miss costs a map lookup rather than a stat.
type cmdFiles struct {
	root string

	mu    sync.RWMutex
	dirs  map[string]map[string]bool // hostname -> "<slug>.txt" names present; nil = no folder
	files map[string][]byte          // file path -> CRLF-normalised contents
}

func newCmdFiles(root string) *cmdFiles {
	if root == "" {
		return nil
	}
	return &cmdFiles{root: root, dirs: map[string]map[string]bool{}, files: map[string][]byte{}}
}

// commandSlug maps a typed command to its file name stem: trimmed, whitespace
// collapsed, lowercased, a trailing "| no-more" removed, spaces replaced by "_".
//
//	"show lldp neighbors detail" -> "show_lldp_neighbors_detail"
func commandSlug(line string) string {
	s := strings.Join(strings.Fields(strings.ToLower(line)), " ")
	if strings.HasSuffix(s, "| no-more") {
		s = strings.TrimSpace(strings.TrimSuffix(s, "| no-more"))
	}
	return strings.ReplaceAll(s, " ", "_")
}

// lookup returns the file contents for line on hostname, or ok=false.
func (c *cmdFiles) lookup(hostname, line string) ([]byte, bool) {
	if c == nil {
		return nil, false
	}
	slug := commandSlug(line)
	// A slug with a path separator could escape the hostname folder.
	if slug == "" || strings.ContainsAny(slug, `/\`) || strings.HasPrefix(slug, ".") {
		return nil, false
	}
	name := slug + ".txt"
	if !c.hostFiles(hostname)[name] {
		return nil, false
	}

	path := filepath.Join(c.root, hostname, name)
	c.mu.RLock()
	b, ok := c.files[path]
	c.mu.RUnlock()
	if ok {
		return b, true
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}
	b = toCRLF(raw)
	c.mu.Lock()
	c.files[path] = b
	c.mu.Unlock()
	return b, true
}

// hostFiles returns (and caches) the non-directory entries in the device's folder
// under root, listed once. A missing folder yields a nil set, so every lookup
// for that device misses.
func (c *cmdFiles) hostFiles(hostname string) map[string]bool {
	c.mu.RLock()
	names, seen := c.dirs[hostname]
	c.mu.RUnlock()
	if seen {
		return names
	}

	entries, err := os.ReadDir(filepath.Join(c.root, hostname))
	if err == nil {
		names = make(map[string]bool, len(entries))
		for _, e := range entries {
			if !e.IsDir() {
				names[e.Name()] = true
			}
		}
	}

	c.mu.Lock()
	c.dirs[hostname] = names
	c.mu.Unlock()
	return names
}

// toCRLF normalises line endings to CRLF, matching the rest of the simulator's
// output, and guarantees a trailing CRLF so the next prompt starts a fresh line.
func toCRLF(b []byte) []byte {
	b = bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n"))
	b = bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
	if !bytes.HasSuffix(b, []byte("\r\n")) {
		b = append(b, '\r', '\n')
	}
	return b
}

// serveCommandFile emits the command file for line, if one exists, through the
// shared delay/emit path under the "file" metric label. handled=false means no
// file matched and the driver should dispatch normally; closed=true means the
// session must end.
func (ctx *sessionCtx) serveCommandFile(line string) (handled, closed bool) {
	out, ok := ctx.cmdFiles.lookup(ctx.dev.Hostname, line)
	if !ok {
		return false, false
	}
	cmdStart := time.Now()
	ctx.applyResponseDelay()
	return true, ctx.emit(CmdFile, cmdStart, Response{Output: out})
}

package tui

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/perlsaiyan/zorp/internal/compose"
)

// Choosing files to attach, from the review screen: files under the home
// directory, most recently changed first (what you just saved or
// downloaded is usually what you want), filtered by typing.

// Limits on the walk, so a huge home directory can't stall it.
const (
	maxPickFiles = 50000
	maxPickDepth = 8
)

type filesMsg struct {
	files []pickedFile
	err   error
}

type pickedFile struct {
	path string
	size int64
	mod  time.Time
}

func listFilesCmd() tea.Cmd {
	return func() tea.Msg {
		home, err := os.UserHomeDir()
		if err != nil {
			return filesMsg{err: err}
		}
		return filesMsg{files: walkFiles(home)}
	}
}

// walkFiles lists regular files under root, skipping hidden and bulky
// directories, newest first.
func walkFiles(root string) []pickedFile {
	var out []pickedFile
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		name := d.Name()
		if d.IsDir() {
			rel, _ := filepath.Rel(root, path)
			depth := strings.Count(rel, string(filepath.Separator))
			if path != root && (strings.HasPrefix(name, ".") || skipDirs[name] || (depth == 0 && skipTopDirs[name]) || depth >= maxPickDepth) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() || strings.HasPrefix(name, ".") {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		out = append(out, pickedFile{path: path, size: info.Size(), mod: info.ModTime()})
		if len(out) >= maxPickFiles {
			return filepath.SkipAll
		}
		return nil
	})
	slices.SortFunc(out, func(a, b pickedFile) int { return b.mod.Compare(a.mod) })
	return out
}

// Directories not worth listing: anywhere, and directly under home.
var (
	skipDirs    = map[string]bool{"node_modules": true, "__pycache__": true}
	skipTopDirs = map[string]bool{"go": true, "snap": true}
)

func (m *Model) openFilePicker(r filesMsg) {
	m.notice = ""
	if m.composing == nil {
		return
	}
	if r.err != nil {
		m.errText = r.err.Error()
		return
	}
	home, _ := os.UserHomeDir()
	p := &picker{kind: pickFile, title: "Attach a file"}
	now := time.Now()
	for _, f := range r.files {
		label := f.path
		if rel, err := filepath.Rel(home, f.path); err == nil && !strings.HasPrefix(rel, "..") {
			label = "~/" + rel
		}
		p.all = append(p.all, choice{label: clean(label), detail: humanSize(f.size) + ", " + shortDate(f.mod, now), path: f.path})
	}
	m.picker = p
	m.filterPicker()
}

// attachFile adds a file to the message being written and shows the
// review screen again.
func (m *Model) attachFile(path string) tea.Cmd {
	c := m.composing
	if c == nil {
		return nil
	}
	data, err := os.ReadFile(c.path)
	if err != nil {
		m.errText = err.Error()
		return nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
			path = "~/" + rel
		}
	}
	if err := os.WriteFile(c.path, []byte(compose.AddAttach(string(data), path)), 0o600); err != nil {
		m.errText = err.Error()
		return nil
	}
	return m.editorDone(nil)
}

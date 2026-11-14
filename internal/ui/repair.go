package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/xjtuwxg/arborist/internal/refs"
)

// Reference repair: after entries move, rewrite the absolute paths that
// pointed at them, in the files the scan found. Every edited file is copied
// into the journal first, so undo can put the exact bytes back.
//
// Log files are left alone: they record what happened under the old path.
// Paths that the scan did not see (files over the size limit, paths built at
// run time, anything outside the workspace) are not repaired.

// Edit is one repaired file or symlink.
type Edit struct {
	File   string `json:"file"`   // relative to the root, where the file is after the moves
	Kind   string `json:"kind"`   // "text" or "symlink"
	Count  int    `json:"count"`  // paths rewritten
	Backup string `json:"backup"` // text: name of the copy in the journal; symlink: the old target
	After  string `json:"after"`  // text: sha256 of the edited content; symlink: the new target
	Undone bool   `json:"undone,omitempty"`
}

const maxRepairSize = 8 << 20

func repairable(r refs.Ref) bool { return !r.Dangling && r.Category != "log" }

// remapper rewrites paths for a set of moves, longest source first so that
// nested moves resolve to the innermost one.
type remapper struct {
	root  string
	moves []Move
}

func newRemapper(root string, moves []Move) *remapper {
	sorted := append([]Move{}, moves...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i].From) > len(sorted[j].From) })
	return &remapper{root: root, moves: sorted}
}

// rel maps a path relative to the root; ok is false when no move covers it.
func (m *remapper) rel(p string) (string, bool) {
	for _, mv := range m.moves {
		if under(p, mv.From) {
			return mv.To + strings.TrimPrefix(p, mv.From), true
		}
	}
	return p, false
}

// text rewrites every absolute path into a moved entry. A match must end at
// a path boundary, so /ws/proj never matches inside /ws/project2.
func (m *remapper) text(data []byte) ([]byte, int) {
	count := 0
	for _, mv := range m.moves {
		old, repl := []byte(m.root+"/"+mv.From), []byte(m.root+"/"+mv.To)
		var out []byte
		pos := 0
		for {
			i := bytes.Index(data[pos:], old)
			if i < 0 {
				break
			}
			i += pos
			end := i + len(old)
			if end < len(data) && data[end] != '/' && !refs.IsPathEnd(data[end]) {
				out = append(out, data[pos:end]...)
				pos = end
				continue
			}
			out = append(append(out, data[pos:i]...), repl...)
			pos = end
			count++
		}
		if out != nil {
			data = append(out, data[pos:]...)
		}
	}
	return data, count
}

func sum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// replaceFile writes data over path through a temporary file in the same
// folder, keeping the permission bits.
func replaceFile(path string, data []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".arborist-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	_, err = tmp.Write(data)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(name, mode.Perm())
	}
	if err == nil {
		err = os.Rename(name, path)
	}
	if err != nil {
		os.Remove(name) // our own temporary file
	}
	return err
}

func replaceLink(path, target string) error {
	tmp := filepath.Join(filepath.Dir(path), ".arborist-link-"+filepath.Base(path))
	os.Remove(tmp) // a leftover of ours from an interrupted run
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// repair rewrites references after tx's moves are done. It returns the
// problems it met; a problem never undoes the moves. Callers hold applyMu.
func (s *Server) repair(tx *Tx) []string {
	m := newRemapper(s.Root, tx.Moves)
	s.mu.Lock()
	files := map[string]string{} // path after the moves -> kind
	for _, r := range s.refs {
		if !repairable(r) {
			continue
		}
		if _, moved := m.rel(r.Target); !moved {
			continue
		}
		now, _ := m.rel(r.File)
		files[now] = r.Kind
	}
	s.mu.Unlock()

	names := make([]string, 0, len(files))
	for f := range files {
		names = append(names, f)
	}
	sort.Strings(names)
	backups := filepath.Join(s.cacheDir, "journal", tx.ID+".files")
	var problems []string
	fixed := map[string]bool{}
	for _, f := range names {
		abs := filepath.Join(s.Root, f)
		st, err := os.Lstat(abs)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if st.Mode()&os.ModeSymlink != 0 {
			old, err := os.Readlink(abs)
			if err != nil || !strings.HasPrefix(old, s.Root+"/") {
				continue
			}
			to, moved := m.rel(strings.TrimPrefix(old, s.Root+"/"))
			if !moved {
				continue
			}
			target := s.Root + "/" + to
			if err := replaceLink(abs, target); err != nil {
				problems = append(problems, fmt.Sprintf("%s: %v", f, err))
				continue
			}
			tx.Edits = append(tx.Edits, Edit{File: f, Kind: "symlink", Count: 1, Backup: old, After: target})
			fixed[f] = true
			s.saveTx(tx)
			continue
		}
		if !st.Mode().IsRegular() || st.Size() > maxRepairSize {
			problems = append(problems, f+": not a regular file of repairable size")
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		edited, n := m.text(data)
		if n == 0 {
			continue // the file changed since the scan
		}
		// Back up first; without a backup the file is not touched.
		name := fmt.Sprintf("%04d", len(tx.Edits))
		if err := os.MkdirAll(backups, 0o700); err == nil {
			err = os.WriteFile(filepath.Join(backups, name), data, 0o600)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("%s: could not back up, left unchanged: %v", f, err))
			continue
		}
		if err := replaceFile(abs, edited, st.Mode()); err != nil {
			problems = append(problems, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		tx.Edits = append(tx.Edits, Edit{File: f, Kind: "text", Count: n, Backup: name, After: sum(edited)})
		fixed[f] = true
		s.saveTx(tx)
	}

	// Point the repaired references at the new paths in the scan data.
	s.mu.Lock()
	for i := range s.refs {
		r := &s.refs[i]
		if !repairable(*r) {
			continue
		}
		to, moved := m.rel(r.Target)
		now, _ := m.rel(r.File)
		if moved && fixed[now] {
			r.Target = to
		}
	}
	s.mu.Unlock()
	return problems
}

// unrepair puts back the files edited by tx. A file that changed after the
// repair is left as it is and reported.
func (s *Server) unrepair(tx *Tx) []string {
	var skipped []string
	backups := filepath.Join(s.cacheDir, "journal", tx.ID+".files")
	for i := len(tx.Edits) - 1; i >= 0; i-- {
		e := &tx.Edits[i]
		if e.Undone {
			continue
		}
		abs := filepath.Join(s.Root, e.File)
		if e.Kind == "symlink" {
			if cur, err := os.Readlink(abs); err != nil || cur != e.After {
				skipped = append(skipped, e.File+" (link changed since)")
				continue
			}
			if err := replaceLink(abs, e.Backup); err != nil {
				skipped = append(skipped, fmt.Sprintf("%s (%v)", e.File, err))
				continue
			}
			e.Undone = true
			continue
		}
		st, err := os.Lstat(abs)
		cur, rerr := os.ReadFile(abs)
		if err != nil || rerr != nil || sum(cur) != e.After {
			skipped = append(skipped, e.File+" (edited since; its paths still point at the new location)")
			continue
		}
		orig, err := os.ReadFile(filepath.Join(backups, e.Backup))
		if err == nil {
			err = replaceFile(abs, orig, st.Mode())
		}
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", e.File, err))
			continue
		}
		e.Undone = true
	}
	return skipped
}

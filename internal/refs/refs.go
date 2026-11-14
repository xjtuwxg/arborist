// Package refs finds absolute path references that point into a workspace.
//
// These are the things a plain `mv` breaks: a file that mentions
// /root/projA/data by absolute path stops working when projA (or the file's
// own project) moves. The scan is read-only.
package refs

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// Ref is one absolute path reference found inside the workspace.
type Ref struct {
	File     string `json:"file"`     // relative to root
	Line     int    `json:"line"`     // 0 for symlinks
	Target   string `json:"target"`   // relative to root
	Kind     string `json:"kind"`     // "symlink" or "text"
	Category string `json:"category"` // where the referring file lives
	Cross    bool   `json:"cross"`    // target is in another top-level project
	Dangling bool   `json:"dangling"` // target does not exist right now
}

// Options controls a scan.
type Options struct {
	MaxFileSize int64 // text files larger than this are skipped
	Workers     int
}

// Stats counts what the scan looked at.
type Stats struct {
	Files      int64 `json:"files"`
	Scanned    int64 `json:"scanned"`
	SkippedBig int64 `json:"skipped_big"`
	SkippedBin int64 `json:"skipped_binary"`
	Unreadable int64 `json:"unreadable"`
}

// Scan walks root and returns every absolute reference into root.
func Scan(root string, opt Options) ([]Ref, Stats, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, Stats{}, err
	}
	if opt.MaxFileSize <= 0 {
		opt.MaxFileSize = 1 << 20
	}
	if opt.Workers <= 0 {
		opt.Workers = runtime.NumCPU()
	}
	needle := []byte(root + "/")

	var (
		mu    sync.Mutex
		out   []Ref
		stats Stats
		wg    sync.WaitGroup
	)
	add := func(r Ref) {
		r.Category = categorize(r.File)
		r.Cross = topLevel(r.File) != topLevel(r.Target)
		_, err := os.Lstat(filepath.Join(root, r.Target))
		r.Dangling = err != nil
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
	}

	paths := make(chan string, 256)
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for p := range paths {
				rel, _ := filepath.Rel(root, p)
				hits, status := scanFile(p, needle, opt.MaxFileSize)
				mu.Lock()
				switch status {
				case statusBinary:
					stats.SkippedBin++
				case statusUnreadable:
					stats.Unreadable++
				default:
					stats.Scanned++
				}
				mu.Unlock()
				for _, h := range hits {
					add(Ref{File: rel, Line: h.line, Target: h.target, Kind: "text"})
				}
			}
		}()
	}

	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			mu.Lock()
			stats.Unreadable++
			mu.Unlock()
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			// Object stores hold no readable paths and dominate file counts.
			if d.Name() == "objects" && filepath.Base(filepath.Dir(p)) == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		stats.Files++
		rel, _ := filepath.Rel(root, p)
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			t, err := os.Readlink(p)
			if err == nil && strings.HasPrefix(t, root+"/") {
				add(Ref{File: rel, Target: strings.TrimPrefix(t, root+"/"), Kind: "symlink"})
			}
		case d.Type().IsRegular():
			info, err := d.Info()
			if err != nil {
				mu.Lock()
				stats.Unreadable++
				mu.Unlock()
				return nil
			}
			if info.Size() > opt.MaxFileSize {
				stats.SkippedBig++
				return nil
			}
			if info.Size() >= int64(len(needle)) {
				paths <- p
			}
		}
		return nil
	})
	close(paths)
	wg.Wait()
	return out, stats, walkErr
}

type hit struct {
	line   int
	target string
}

const (
	statusOK = iota
	statusBinary
	statusUnreadable
)

func scanFile(p string, needle []byte, max int64) ([]hit, int) {
	f, err := os.Open(p)
	if err != nil {
		return nil, statusUnreadable
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, max))
	if err != nil {
		return nil, statusUnreadable
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, statusBinary
	}
	var hits []hit
	line, pos := 1, 0
	for {
		i := bytes.Index(data[pos:], needle)
		if i < 0 {
			break
		}
		i += pos
		line += bytes.Count(data[pos:i], []byte{'\n'})
		start := i + len(needle)
		end := start
		for end < len(data) && !isPathEnd(data[end]) {
			end++
		}
		target := strings.TrimRight(string(data[start:end]), "./")
		if target != "" {
			hits = append(hits, hit{line: line, target: target})
		}
		pos = end
	}
	return hits, statusOK
}

func isPathEnd(c byte) bool {
	if c <= ' ' || c >= 0x7f {
		return true
	}
	return strings.IndexByte("\"'`<>|;,()[]{}:=*?\\$&#!", c) >= 0
}

func topLevel(rel string) string {
	if i := strings.IndexByte(rel, '/'); i >= 0 {
		return rel[:i]
	}
	return rel
}

// categorize says what kind of place the referring file lives in, because
// that decides how a broken reference gets repaired: regenerated state can be
// rebuilt, hand-written files have to be rewritten.
func categorize(rel string) string {
	base := filepath.Base(rel)
	for _, part := range strings.Split(filepath.Dir(rel), "/") {
		switch part {
		case ".git":
			return "git"
		case "node_modules":
			return "node"
		case "venv", ".venv", "env", ".env", "site-packages", ".conda", "conda-meta", "__pycache__":
			return "python-env"
		case "build", "target", "dist", "out", "CMakeFiles", ".cache", "_build", ".ccache":
			return "build"
		case ".claude", ".codex", ".cursor":
			return "agent"
		}
	}
	switch {
	case base == "CMakeCache.txt" || base == "compile_commands.json" || strings.HasSuffix(base, ".d") || strings.HasSuffix(base, ".cmake"):
		return "build"
	case base == "CLAUDE.md" || base == "AGENTS.md":
		return "agent"
	case strings.HasSuffix(base, ".log") || strings.HasSuffix(base, ".out"):
		return "log"
	}
	return "authored"
}

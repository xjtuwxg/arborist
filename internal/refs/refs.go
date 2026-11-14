// Package refs finds absolute path references that point into a workspace.
//
// These are the things a plain `mv` breaks: a file that mentions
// /root/projA/data by absolute path stops working when projA (or the file's
// own project) moves. The scan is read-only.
package refs

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path"
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
	Category string `json:"category"` // what kind of file refers: authored, build, python-env, node, git, agent, log, data
	Cross    bool   `json:"cross"`    // target is in another top-level project
	Dangling bool   `json:"dangling"` // target does not exist right now
}

// Options controls a scan.
type Options struct {
	MaxFileSize int64                       // text files larger than this are skipped
	Sub         string                      // only walk this subfolder of root (references are still relative to root)
	Workers     int                         // files read in parallel
	Ignore      []string                    // patterns of folders and files to leave out, see Ignored
	Cache       string                      // file that remembers earlier scans; empty means scan everything
	Progress    func(files, expected int64) // called now and then with the files seen so far and the number the last scan saw (0 when unknown)
}

// Stats counts what the scan looked at.
type Stats struct {
	Files      int64 `json:"files"`
	Scanned    int64 `json:"scanned"`        // files opened and read
	Reused     int64 `json:"reused"`         // files in folders unchanged since the last scan
	Ignored    int64 `json:"ignored"`        // folders and files left out by ignore patterns
	SkippedBig int64 `json:"skipped_big"`    // larger than MaxFileSize
	SkippedBin int64 `json:"skipped_binary"` // binary by extension or content
	Unreadable int64 `json:"unreadable"`
}

// IgnoreFile is the name of the per-workspace ignore list, one pattern per
// line, kept in the workspace root.
const IgnoreFile = ".arboristignore"

// LoadIgnore reads the ignore patterns of a workspace. A missing file means
// no patterns.
func LoadIgnore(root string) []string {
	b, err := os.ReadFile(filepath.Join(root, IgnoreFile))
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// Anchored returns the root-relative path of a pattern that names one place
// ("/data" or "data/trees"), or "" for a pattern that matches a name
// anywhere ("node_modules", "*.ckpt").
func Anchored(pattern string) string {
	p := strings.Trim(pattern, "/")
	if strings.HasPrefix(pattern, "/") || strings.Contains(p, "/") {
		return p
	}
	return ""
}

// Ignored reports whether the entry at rel (relative to the root) matches a
// pattern. Both kinds of pattern may use shell wildcards.
func Ignored(patterns []string, rel string) bool {
	name := path.Base(rel)
	for _, p := range patterns {
		if a := Anchored(p); a != "" {
			if ok, _ := path.Match(a, rel); ok || a == rel {
				return true
			}
		} else if ok, _ := path.Match(strings.Trim(p, "/"), name); ok {
			return true
		}
	}
	return false
}

// IgnoredPath reports whether rel or any folder above it is ignored.
func IgnoredPath(patterns []string, rel string) bool {
	for p := rel; p != "" && p != "."; p = path.Dir(p) {
		if Ignored(patterns, p) {
			return true
		}
	}
	return false
}

// CacheDir is where Arborist keeps what it knows about a workspace root.
func CacheDir(root string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	h := sha1.Sum([]byte(root))
	return filepath.Join(cache, "arborist", hex.EncodeToString(h[:6])), nil
}

// The scan cache works per folder. A folder's fingerprint covers the name,
// size and modification time of each regular file directly in it. When the
// fingerprint is unchanged, the references found in those files last time
// are reused and none of the files is opened.
type cachedRef struct {
	Name   string // file name within the folder
	Line   int
	Target string
}

type cachedDir struct {
	Fp   uint64
	Refs []cachedRef
	N    int32 // files and symlinks directly in the folder; lets the next scan say how far along it is
}

type scanCache struct {
	Version     int
	Root        string
	MaxFileSize int64
	Dirs        map[string]*cachedDir // keyed by folder path relative to the root
	Counted     bool                  // every folder has its N (caches written by older versions do not)
}

const cacheVersion = 1

func loadCache(file, root string, max int64) (map[string]*cachedDir, bool) {
	f, err := os.Open(file)
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var c scanCache
	if gob.NewDecoder(bufio.NewReader(f)).Decode(&c) != nil || c.Version != cacheVersion || c.Root != root || c.MaxFileSize != max {
		return nil, false
	}
	return c.Dirs, c.Counted
}

func saveCache(file, root string, max int64, dirs map[string]*cachedDir, counted bool) error {
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	err = gob.NewEncoder(w).Encode(scanCache{Version: cacheVersion, Root: root, MaxFileSize: max, Dirs: dirs, Counted: counted})
	if err == nil {
		err = w.Flush()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp) // our own temporary file
		return err
	}
	return os.Rename(tmp, file)
}

func underDir(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
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
	sub := strings.Trim(filepath.ToSlash(filepath.Clean("/"+opt.Sub)), "/")
	if _, err := os.Stat(filepath.Join(root, sub)); err != nil {
		return nil, Stats{}, err
	}
	needle := []byte(root + "/")

	var old map[string]*cachedDir
	var counted bool
	var expect int64 // files the last scan saw here; 0 when unknown
	if opt.Cache != "" {
		old, counted = loadCache(opt.Cache, root, opt.MaxFileSize)
		if counted {
			for dir, c := range old {
				if underDir(dir, sub) {
					expect += int64(c.N)
				}
			}
		}
	}
	fresh := map[string]*cachedDir{} // every folder visited by this scan
	rescanned := map[string]bool{}   // of those, the ones whose files were read

	var (
		mu    sync.Mutex
		out   []Ref
		stats Stats
		wg    sync.WaitGroup
	)
	add := func(r Ref) {
		r.Category = Categorize(r.File)
		r.Cross = topLevel(r.File) != topLevel(r.Target)
		_, err := os.Lstat(filepath.Join(root, r.Target))
		r.Dangling = err != nil
		mu.Lock()
		out = append(out, r)
		mu.Unlock()
	}
	count := func(n *int64, by int64) {
		mu.Lock()
		*n += by
		mu.Unlock()
	}

	type job struct{ abs, rel string }
	jobs := make(chan job, 256)
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				hits, status := scanFile(j.abs, needle, opt.MaxFileSize)
				switch status {
				case statusBinary:
					count(&stats.SkippedBin, 1)
				case statusUnreadable:
					count(&stats.Unreadable, 1)
				default:
					count(&stats.Scanned, 1)
				}
				for _, h := range hits {
					add(Ref{File: j.rel, Line: h.line, Target: h.target, Kind: "text"})
				}
			}
		}()
	}

	var seen int64
	var walk func(rel string)
	walk = func(rel string) {
		abs := filepath.Join(root, rel)
		des, err := os.ReadDir(abs)
		if err != nil {
			count(&stats.Unreadable, 1)
			return
		}
		type file struct {
			name string
			size int64
		}
		var files []file
		var subdirs []string
		var here int32
		fp := fnv.New64a()
		var num [16]byte
		for _, d := range des {
			childRel := path.Join(rel, d.Name())
			if Ignored(opt.Ignore, childRel) {
				count(&stats.Ignored, 1)
				continue
			}
			switch {
			case d.IsDir():
				// Object stores hold no readable paths and dominate file counts.
				if d.Name() == "objects" && path.Base(rel) == ".git" {
					continue
				}
				subdirs = append(subdirs, childRel)
				continue
			case d.Type()&fs.ModeSymlink != 0:
				if t, err := os.Readlink(filepath.Join(abs, d.Name())); err == nil && strings.HasPrefix(t, root+"/") {
					add(Ref{File: childRel, Target: strings.TrimPrefix(t, root+"/"), Kind: "symlink"})
				}
			case d.Type().IsRegular():
				info, err := d.Info()
				if err != nil {
					count(&stats.Unreadable, 1)
					continue
				}
				files = append(files, file{d.Name(), info.Size()})
				fp.Write([]byte(d.Name()))
				binary.LittleEndian.PutUint64(num[:8], uint64(info.Size()))
				binary.LittleEndian.PutUint64(num[8:], uint64(info.ModTime().UnixNano()))
				fp.Write(num[:])
				fp.Write([]byte{0})
			default:
				continue
			}
			seen++
			here++
			if opt.Progress != nil && seen%20000 == 0 {
				opt.Progress(seen, expect)
			}
		}
		sum := fp.Sum64()
		if c := old[rel]; c != nil && c.Fp == sum {
			fresh[rel] = c
			count(&stats.Reused, int64(len(files)))
			for _, r := range c.Refs {
				add(Ref{File: path.Join(rel, r.Name), Line: r.Line, Target: r.Target, Kind: "text"})
			}
		} else {
			fresh[rel] = &cachedDir{Fp: sum}
			rescanned[rel] = true
			for _, f := range files {
				switch {
				case f.size > opt.MaxFileSize:
					count(&stats.SkippedBig, 1)
				case binaryExt[strings.ToLower(path.Ext(f.name))]:
					count(&stats.SkippedBin, 1)
				case f.size >= int64(len(needle)):
					jobs <- job{filepath.Join(abs, f.name), path.Join(rel, f.name)}
				}
			}
		}
		fresh[rel].N = here
		for _, d := range subdirs {
			walk(d)
		}
	}
	if opt.Progress != nil {
		opt.Progress(0, expect)
	}
	walk(sub)
	close(jobs)
	wg.Wait()
	stats.Files = seen

	if opt.Cache != "" {
		for _, r := range out {
			if dir := path.Dir(r.File); r.Kind == "text" {
				if dir == "." {
					dir = ""
				}
				if rescanned[dir] {
					fresh[dir].Refs = append(fresh[dir].Refs, cachedRef{Name: path.Base(r.File), Line: r.Line, Target: r.Target})
				}
			}
		}
		// Keep what earlier scans learned about folders outside this one.
		for dir, c := range old {
			if !underDir(dir, sub) {
				fresh[dir] = c
			}
		}
		if err := saveCache(opt.Cache, root, opt.MaxFileSize, fresh, counted || sub == "" || old == nil); err != nil {
			return out, stats, err
		}
	}
	return out, stats, nil
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
	// Look at the first block before reading the rest: most binaries are
	// recognizable from it, and reading them whole is the bulk of the I/O.
	head := make([]byte, 8192)
	n, err := io.ReadFull(f, head)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, statusUnreadable
	}
	head = head[:n]
	if bytes.IndexByte(head, 0) >= 0 {
		return nil, statusBinary
	}
	data := head
	if n == cap(head) {
		rest, err := io.ReadAll(io.LimitReader(f, max-int64(n)))
		if err != nil {
			return nil, statusUnreadable
		}
		data = append(data, rest...)
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

// IsPathEnd reports whether c cannot be part of a path written in text.
func IsPathEnd(c byte) bool { return isPathEnd(c) }

// binaryExt lists file types that never hold a readable path. They are
// skipped without being opened.
var binaryExt = map[string]bool{}

func init() {
	for _, e := range strings.Fields(`.o .a .so .ko .obj .lo .la .dll .dylib .exe .class .jar .pyc .pyo .wasm .rlib .rmeta
		.png .jpg .jpeg .gif .bmp .ico .webp .tiff .pdf .ps .eps
		.gz .tgz .bz2 .xz .zst .zip .7z .rar .tar .whl .deb .rpm .iso .img .qcow2
		.mp3 .mp4 .mkv .avi .mov .wav .flac .ttf .otf .woff .woff2
		.pack .idx .db .sqlite .sqlite3 .npy .npz .pt .pth .ckpt .safetensors .onnx .gguf .parquet .pkl .bin`) {
		binaryExt[e] = true
	}
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

// Categorize says what kind of file a reference lives in, because that
// decides what a move has to do about it: authored files need an edit, build
// output and environments can be regenerated, logs and recorded data describe
// the past. It looks only at the path, so stored scans can be reclassified
// without reading the disk again.
func Categorize(rel string) string {
	base := filepath.Base(rel)
	kind := ""
	for _, part := range strings.Split(filepath.Dir(rel), "/") {
		switch part {
		case ".git":
			return "git"
		case "node_modules":
			return "node"
		case "venv", ".venv", "env", ".env", "site-packages", ".conda", "conda-meta", "__pycache__":
			return "python-env"
		case "build", "target", "dist", "out", "CMakeFiles", ".cache", "_build", ".ccache", ".deps", "autom4te.cache":
			return "build"
		case ".claude", ".codex", ".cursor":
			return "agent"
		case "meson-info", "meson-private", "cache":
			return "build"
		case "logs", "log":
			kind = "log"
		case "data", "dataset", "datasets", "results", "runs", "output", "outputs", "sessions", "traces":
			if kind == "" {
				kind = "data"
			}
		}
		if strings.HasPrefix(part, "build-") || strings.HasPrefix(part, "cmake-build-") {
			return "build"
		}
	}
	ext := strings.ToLower(filepath.Ext(base))
	switch {
	case base == "CMakeCache.txt" || base == "compile_commands.json" || base == ".depend" || base == "config.status" ||
		base == "build.ninja" || strings.HasPrefix(base, ".ninja_") ||
		base == "libtool" || base == "Makefile.objects" || base == "Makefile.fragments" ||
		ext == ".d" || ext == ".dep" || ext == ".po" || ext == ".plo" || ext == ".cmake" || ext == ".la" || ext == ".lo" || ext == ".pc" ||
		(strings.HasPrefix(base, ".") && ext == ".cmd"): // kernel build: .foo.o.cmd
		return "build"
	case base == "CLAUDE.md" || base == "AGENTS.md":
		return "agent"
	case ext == ".log" || ext == ".out" || base == "config.log":
		return "log"
	case ext == ".csv" || ext == ".tsv" || ext == ".jsonl" || ext == ".ndjson":
		return "data"
	}
	if kind != "" {
		return kind
	}
	return "authored"
}

// Recategorize brings the categories of a stored scan up to date.
func Recategorize(found []Ref) {
	for i := range found {
		found[i].Category = Categorize(found[i].File)
	}
}

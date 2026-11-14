// Package ui serves the Arborist web interface: a file browser over one
// workspace root, with reference scanning and AI analysis per folder.
//
// Everything here is read-only with respect to the workspace. The server
// writes only to its cache directory.
package ui

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/xjtuwxg/arborist/internal/providers"
	"github.com/xjtuwxg/arborist/internal/refs"
)

//go:embed web
var webFS embed.FS

// Job is a background scan or analysis.
type Job struct {
	ID      int       `json:"id"`
	Kind    string    `json:"kind"` // "scan" or "analyze"
	Path    string    `json:"path"`
	Status  string    `json:"status"` // "running", "done", "failed"
	Error   string    `json:"error,omitempty"`
	Detail  string    `json:"detail,omitempty"`
	Started time.Time `json:"started"`
	Ended   time.Time `json:"ended,omitzero"`
}

// Server holds the state behind the web interface.
type Server struct {
	Root  string
	Token string

	cacheDir string

	applyMu sync.Mutex // one apply, undo or trash action at a time

	mu        sync.Mutex
	txs       []*Tx
	refs      []refs.Ref
	scanned   []string // subfolders (relative, "" for the root) with reference data
	analyses  map[string]*Analysis
	providers []providers.Provider
	jobs      []*Job
}

// New prepares a server for root. The access token is kept in the cache
// directory so the URL stays the same across restarts.
func New(root string) (*Server, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", root)
	}
	dir, err := cacheFor(root)
	if err != nil {
		return nil, err
	}
	s := &Server{Root: root, cacheDir: dir, analyses: map[string]*Analysis{}}
	if err := os.MkdirAll(s.cacheDir, 0o700); err != nil {
		return nil, err
	}
	tokFile := filepath.Join(s.cacheDir, "token")
	if b, err := os.ReadFile(tokFile); err == nil && len(b) >= 32 {
		s.Token = strings.TrimSpace(string(b))
	} else {
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return nil, err
		}
		s.Token = hex.EncodeToString(raw)
		if err := os.WriteFile(tokFile, []byte(s.Token), 0o600); err != nil {
			return nil, err
		}
	}
	s.loadJSON("refs.json", &struct {
		Refs    *[]refs.Ref `json:"refs"`
		Scanned *[]string   `json:"scanned"`
	}{&s.refs, &s.scanned})
	s.loadJSON("analyses.json", &s.analyses)
	s.loadJournal()
	s.providers = providers.Detect(context.Background())
	return s, nil
}

// cacheFor returns the cache directory of a workspace root.
func cacheFor(root string) (string, error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	sum := sha1.Sum([]byte(root))
	return filepath.Join(cache, "arborist", hex.EncodeToString(sum[:6])), nil
}

// LoadAnalyses returns the AI analyses stored for root, keyed by folder.
func LoadAnalyses(root string) (map[string]*Analysis, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if root, err = filepath.EvalSymlinks(root); err != nil {
		return nil, err
	}
	dir, err := cacheFor(root)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, "analyses.json"))
	if err != nil {
		return nil, err
	}
	out := map[string]*Analysis{}
	return out, json.Unmarshal(b, &out)
}

// LoadRefs replaces the reference data with a full scan made by `arb refs --json`.
func (s *Server) LoadRefs(file string) error {
	b, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	var doc struct {
		Refs []refs.Ref `json:"refs"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	s.mu.Lock()
	s.refs, s.scanned = doc.Refs, []string{""}
	s.mu.Unlock()
	s.saveRefs()
	return nil
}

func (s *Server) loadJSON(name string, v any) {
	if b, err := os.ReadFile(filepath.Join(s.cacheDir, name)); err == nil {
		json.Unmarshal(b, v)
	}
}

func (s *Server) saveJSON(name string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	tmp := filepath.Join(s.cacheDir, name+".tmp")
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, filepath.Join(s.cacheDir, name))
	}
}

func (s *Server) saveRefs() {
	s.mu.Lock()
	doc := map[string]any{"refs": s.refs, "scanned": s.scanned}
	s.mu.Unlock()
	s.saveJSON("refs.json", doc)
}

// Handler returns the HTTP handler, with the token check in front.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServerFS(web))
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/ls", s.handleLs)
	mux.HandleFunc("GET /api/preview", s.handlePreview)
	mux.HandleFunc("GET /api/refs", s.handleRefs)
	mux.HandleFunc("GET /api/jobs", s.handleJobs)
	mux.HandleFunc("POST /api/scan", s.handleScan)
	mux.HandleFunc("POST /api/analyze", s.handleAnalyze)
	mux.HandleFunc("POST /api/providers/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/plan", s.handlePlan)
	mux.HandleFunc("POST /api/apply", s.handleApply)
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/undo", s.handleUndo)
	mux.HandleFunc("GET /api/trash", s.handleTrash)
	mux.HandleFunc("POST /api/trash/restore", s.handleTrashRestore)
	mux.HandleFunc("POST /api/trash/delete", s.handleTrashDelete)
	return s.auth(mux)
}

const cookieName = "arborist_token"

func (s *Server) auth(next http.Handler) http.Handler {
	ok := func(v string) bool { return subtle.ConstantTimeCompare([]byte(v), []byte(s.Token)) == 1 }
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if t := r.URL.Query().Get("token"); t != "" && r.URL.Path == "/" {
			if !ok(t) {
				http.Error(w, "wrong token", http.StatusForbidden)
				return
			}
			http.SetCookie(w, &http.Cookie{Name: cookieName, Value: t, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 90 * 86400})
			http.Redirect(w, r, "/", http.StatusSeeOther)
			return
		}
		c, err := r.Cookie(cookieName)
		if err != nil || !ok(c.Value) {
			http.Error(w, "Open Arborist with the link printed by `arb ui` (it carries the access token).", http.StatusForbidden)
			return
		}
		// State-changing requests must come from our own page.
		if r.Method != http.MethodGet && r.Header.Get("X-Arborist") != "1" {
			http.Error(w, "missing X-Arborist header", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// resolve turns a client path into (relative, absolute) inside the root.
// Symlinks that lead outside the root are refused.
func (s *Server) resolve(rel string) (string, string, error) {
	rel = filepath.Clean("/" + rel)[1:]
	abs := filepath.Join(s.Root, rel)
	real, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", "", err
	}
	if real != s.Root && !strings.HasPrefix(real, s.Root+string(filepath.Separator)) {
		return "", "", errors.New("path leaves the workspace")
	}
	return rel, abs, nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func under(p, dir string) bool {
	return dir == "" || p == dir || strings.HasPrefix(p, dir+"/")
}

func (s *Server) isScanned(rel string) bool {
	for _, sc := range s.scanned {
		if under(rel, sc) {
			return true
		}
	}
	return false
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	host, _ := os.Hostname()
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, map[string]any{"root": s.Root, "host": host, "providers": s.providers, "refs": len(s.refs), "scanned": s.scanned, "trash": len(s.trashItems())})
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	found := providers.Detect(r.Context())
	s.mu.Lock()
	s.providers = found
	s.mu.Unlock()
	writeJSON(w, found)
}

type entry struct {
	Name    string    `json:"name"`
	Dir     bool      `json:"dir"`
	Link    string    `json:"link,omitempty"`
	Size    int64     `json:"size"`
	ModTime time.Time `json:"mtime"`
	RefsOut int       `json:"refsOut"`
	RefsIn  int       `json:"refsIn"`
}

func (s *Server) handleLs(w http.ResponseWriter, r *http.Request) {
	rel, abs, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	entries := make([]entry, 0, len(des))
	index := map[string]int{}
	for _, d := range des {
		e := entry{Name: d.Name(), Dir: d.IsDir()}
		if info, err := d.Info(); err == nil {
			e.Size, e.ModTime = info.Size(), info.ModTime()
		}
		if d.Type()&fs.ModeSymlink != 0 {
			e.Link, _ = os.Readlink(filepath.Join(abs, d.Name()))
			if st, err := os.Stat(filepath.Join(abs, d.Name())); err == nil {
				e.Dir = st.IsDir()
			}
		}
		index[e.Name] = len(entries)
		entries = append(entries, e)
	}
	// child returns the entry of this folder that p falls under, if any.
	child := func(p string) (int, bool) {
		if !under(p, rel) || p == rel {
			return 0, false
		}
		rest := strings.TrimPrefix(strings.TrimPrefix(p, rel), "/")
		name, _, _ := strings.Cut(rest, "/")
		i, ok := index[name]
		return i, ok
	}
	s.mu.Lock()
	for _, ref := range s.refs {
		fi, fok := child(ref.File)
		ti, tok := child(ref.Target)
		if fok {
			entries[fi].RefsOut++
		}
		if tok && !(fok && fi == ti) {
			entries[ti].RefsIn++
		}
	}
	resp := map[string]any{"path": rel, "entries": entries, "scanned": s.isScanned(rel), "analysis": s.withImpact(rel)}
	s.mu.Unlock()
	sort.SliceStable(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, resp)
}

func (s *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	_, abs, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	st, err := os.Stat(abs)
	if err != nil || st.IsDir() {
		fail(w, http.StatusBadRequest, errors.New("not a file"))
		return
	}
	text, binary := readHead(abs, 64<<10)
	writeJSON(w, map[string]any{"size": st.Size(), "binary": binary, "text": text, "truncated": st.Size() > 64<<10})
}

// readHead returns up to max bytes of a file as text, or binary=true.
func readHead(path string, max int64) (string, bool) {
	f, err := os.Open(path)
	if err != nil {
		return "", false
	}
	defer f.Close()
	b, _ := io.ReadAll(io.LimitReader(f, max))
	if strings.IndexByte(string(b), 0) >= 0 {
		return "", true
	}
	for len(b) > 0 && !utf8.Valid(b) && len(b) > int(max)-4 {
		b = b[:len(b)-1] // a multi-byte character cut by the limit
	}
	return strings.ToValidUTF8(string(b), "�"), false
}

func (s *Server) handleRefs(w http.ResponseWriter, r *http.Request) {
	rel, _, err := s.resolve(r.URL.Query().Get("path"))
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	const limit = 300
	var out, in []refs.Ref
	var nOut, nIn int
	s.mu.Lock()
	for _, ref := range s.refs {
		from, to := under(ref.File, rel), under(ref.Target, rel)
		if from {
			if nOut++; len(out) < limit {
				out = append(out, ref)
			}
		}
		if to && !from {
			if nIn++; len(in) < limit {
				in = append(in, ref)
			}
		}
	}
	scanned := s.isScanned(rel)
	s.mu.Unlock()
	writeJSON(w, map[string]any{"path": rel, "scanned": scanned, "out": out, "in": in, "outTotal": nOut, "inTotal": nIn})
}

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	jobs := append([]*Job{}, s.jobs...)
	if len(jobs) > 20 {
		jobs = jobs[len(jobs)-20:]
	}
	writeJSON(w, jobs)
}

// start registers a job and runs fn in the background. Only one job of a
// kind may run on a path at a time.
func (s *Server) start(kind, path string, fn func(*Job) error) (*Job, error) {
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.Status == "running" && j.Kind == kind && j.Path == path {
			s.mu.Unlock()
			return nil, fmt.Errorf("already running a %s on this folder", kind)
		}
	}
	j := &Job{ID: len(s.jobs) + 1, Kind: kind, Path: path, Status: "running", Started: time.Now()}
	s.jobs = append(s.jobs, j)
	s.mu.Unlock()
	go func() {
		err := fn(j)
		s.mu.Lock()
		j.Ended = time.Now()
		if err != nil {
			j.Status, j.Error = "failed", err.Error()
		} else {
			j.Status = "done"
		}
		s.mu.Unlock()
	}()
	return j, nil
}

func readBody(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(v)
}

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	var req struct{ Path string }
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rel, _, err := s.resolve(req.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	j, err := s.start("scan", rel, func(j *Job) error {
		found, stats, err := refs.Scan(s.Root, refs.Options{Sub: rel, Progress: func(n int64) {
			s.mu.Lock()
			j.Detail = fmt.Sprintf("%d files so far", n)
			s.mu.Unlock()
		}})
		if err != nil {
			return err
		}
		s.mu.Lock()
		kept := s.refs[:0:0]
		for _, ref := range s.refs {
			if !under(ref.File, rel) {
				kept = append(kept, ref)
			}
		}
		s.refs = append(kept, found...)
		roots := []string{rel}
		for _, sc := range s.scanned {
			if !under(sc, rel) && !under(rel, sc) {
				roots = append(roots, sc)
			} else if under(rel, sc) {
				roots[0] = sc // an ancestor already covers this folder
			}
		}
		s.scanned = roots
		j.Detail = fmt.Sprintf("%d references in %d files scanned", len(found), stats.Scanned)
		s.mu.Unlock()
		s.saveRefs()
		return nil
	})
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, j)
}

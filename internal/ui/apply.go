package ui

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xjtuwxg/arborist/internal/refs"
)

// Applying a plan only ever renames. Nothing is overwritten and nothing is
// deleted: "archive" moves into _archive next to the entry, "trash" moves
// into _trash at the workspace root, where entries stay until the user
// deletes them one by one with a second confirmation.
const (
	archiveName = "_archive"
	trashName   = "_trash"
)

// Move is one rename, with paths relative to the workspace root.
type Move struct {
	Op       string `json:"op"`
	From     string `json:"from"`
	To       string `json:"to"`
	Reason   string `json:"reason,omitempty"`
	Proposal int    `json:"proposal"`
	Breaks   int    `json:"breaks"` // live references that stop resolving
	Done     bool   `json:"done,omitempty"`
	Restored bool   `json:"restored,omitempty"` // trash only: put back
	Purged   bool   `json:"purged,omitempty"`   // trash only: deleted for good
}

// Node is one entry of the before or after tree shown in a dry run.
type Node struct {
	Name     string  `json:"name"`
	Dir      bool    `json:"dir"`
	State    string  `json:"state"` // same, leaving, moved, new, trash
	Other    string  `json:"other,omitempty"`
	Children []*Node `json:"children,omitempty"`
}

// Plan is the dry run of the selected proposals of one folder.
type Plan struct {
	Path      string   `json:"path"`
	Moves     []Move   `json:"moves"`
	Conflicts []string `json:"conflicts"`
	Before    []*Node  `json:"before"`
	After     []*Node  `json:"after"`
	Breaks    int      `json:"breaks"`    // live references pointing at the moved entries
	Repairs   int      `json:"repairs"`   // of those, how many a repair rewrites
	Files     int      `json:"files"`     // files a repair edits
	Unscanned bool     `json:"unscanned"` // some entries have no reference scan
}

// Tx is a journal entry: one applied plan.
type Tx struct {
	ID        string     `json:"id"`
	At        time.Time  `json:"at"`
	Path      string     `json:"path"`
	Moves     []Move     `json:"moves"`
	Created   []string   `json:"created"` // folders made for the moves
	Proposals []Proposal `json:"proposals"`
	Edits     []Edit     `json:"edits"`              // files rewritten by the reference repair
	Problems  []string   `json:"problems,omitempty"` // references that could not be repaired
	Status    string     `json:"status"`             // applying, applied, undone, failed
	Error     string     `json:"error,omitempty"`
}

func (s *Server) loadJournal() {
	files, _ := filepath.Glob(filepath.Join(s.cacheDir, "journal", "*.json"))
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		tx := &Tx{}
		if json.Unmarshal(b, tx) == nil && tx.ID != "" {
			s.txs = append(s.txs, tx)
		}
	}
}

func (s *Server) saveTx(tx *Tx) error {
	dir := filepath.Join(s.cacheDir, "journal")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(tx, "", " ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, tx.ID+".tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(dir, tx.ID+".json"))
}

// insideRoot reports whether abs, after following the symlinks of its
// deepest existing ancestor, is still in the workspace.
func (s *Server) insideRoot(abs string) bool {
	p := abs
	for {
		real, err := filepath.EvalSymlinks(p)
		if err == nil {
			return real == s.Root || strings.HasPrefix(real, s.Root+string(filepath.Separator))
		}
		if p == s.Root || p == filepath.Dir(p) {
			return false
		}
		p = filepath.Dir(p)
	}
}

func exists(abs string) bool {
	_, err := os.Lstat(abs)
	return err == nil
}

// buildPlan turns the selected proposals of folder rel into moves and the
// two trees. selected == nil means all. txid names the trash folder. Callers
// hold s.mu.
func (s *Server) buildPlan(rel string, selected []int, txid string) (*Plan, []Proposal, error) {
	a := s.analyses[rel]
	if a == nil {
		return nil, nil, errors.New("this folder has no analysis")
	}
	if rel == trashName || strings.HasPrefix(rel, trashName+"/") {
		return nil, nil, errors.New("the trash cannot be reorganized")
	}
	pick := map[int]bool{}
	for _, i := range selected {
		pick[i] = true
	}
	plan := &Plan{Path: rel, Moves: []Move{}, Conflicts: []string{}}
	var used []Proposal
	ignore := refs.LoadIgnore(s.Root)
	seenFrom, seenTo := map[string]bool{}, map[string]bool{}
	editFiles := map[string]bool{}
	conflict := func(format string, args ...any) {
		plan.Conflicts = append(plan.Conflicts, fmt.Sprintf(format, args...))
	}
	for i, p := range a.Proposals {
		if selected != nil && !pick[i] {
			continue
		}
		used = append(used, p)
		for _, name := range p.From {
			if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\") {
				conflict("%q is not a direct child of this folder", name)
				continue
			}
			from := path.Join(rel, name)
			var to string
			switch p.Op {
			case "archive":
				to = path.Join(rel, archiveName, name)
			case "trash":
				to = path.Join(trashName, txid, from)
			default:
				dest := path.Clean("/" + p.To)[1:]
				if dest == "" {
					conflict("%s: no destination given", name)
					continue
				}
				if len(p.From) == 1 && path.Base(dest) == name {
					to = path.Join(rel, dest)
				} else {
					to = path.Join(rel, dest, name)
				}
			}
			switch {
			case name == trashName || name == archiveName:
				conflict("%s is managed by Arborist and stays where it is", name)
			case !exists(filepath.Join(s.Root, from)):
				conflict("%s no longer exists", from)
			case from == to:
				// already in place
			case under(to, from):
				conflict("%s cannot move into itself", from)
			case seenFrom[from]:
				conflict("%s appears in two proposals", from)
			case seenTo[to]:
				conflict("two entries would both become %s", to)
			case exists(filepath.Join(s.Root, to)):
				conflict("%s already exists; nothing is overwritten", to)
			case !s.insideRoot(filepath.Join(s.Root, to)):
				conflict("%s leads outside the workspace", to)
			default:
				seenFrom[from], seenTo[to] = true, true
				m := Move{Op: p.Op, From: from, To: to, Reason: p.Reason, Proposal: i}
				for _, ref := range s.refs {
					if !ref.Dangling && under(ref.Target, from) {
						m.Breaks++
						if repairable(ref) {
							plan.Repairs++
							editFiles[ref.File] = true
						}
					}
				}
				if !s.isScanned(from) {
					plan.Unscanned = true
				}
				// References inside ignored folders were never looked at.
				for _, pat := range ignore {
					if a := refs.Anchored(pat); a != "" && (under(a, from) || under(from, a)) {
						plan.Unscanned = true
					}
				}
				plan.Breaks += m.Breaks
				plan.Moves = append(plan.Moves, m)
			}
		}
	}
	for _, m := range plan.Moves {
		for _, n := range plan.Moves {
			if m.From != n.From && under(m.To, n.From) {
				conflict("%s would move into %s, which is itself being moved", m.From, n.From)
			}
		}
	}
	plan.Files = len(editFiles)
	s.trees(plan)
	return plan, used, nil
}

// trees fills plan.Before and plan.After with one level of the folder plus
// whatever the moves add below it.
func (s *Server) trees(plan *Plan) {
	abs := filepath.Join(s.Root, plan.Path)
	des, _ := os.ReadDir(abs)
	leaving := map[string]Move{}
	for _, m := range plan.Moves {
		leaving[m.From] = m
	}
	shown := func(to string) string {
		if under(to, plan.Path) && plan.Path != "" {
			return strings.TrimPrefix(to, plan.Path+"/")
		}
		return to
	}
	after := &Node{Dir: true}
	for _, d := range des {
		isDir := d.IsDir()
		if st, err := os.Stat(filepath.Join(abs, d.Name())); err == nil {
			isDir = st.IsDir()
		}
		n := &Node{Name: d.Name(), Dir: isDir, State: "same"}
		if m, ok := leaving[path.Join(plan.Path, d.Name())]; ok {
			other := shown(m.To)
			if m.Op == "trash" {
				other = "Trash"
			}
			plan.Before = append(plan.Before, &Node{Name: n.Name, Dir: isDir, State: "leaving", Other: other})
			continue
		}
		plan.Before = append(plan.Before, n)
		after.Children = append(after.Children, &Node{Name: n.Name, Dir: isDir, State: "same"})
	}
	child := func(parent *Node, name string, make func() *Node) *Node {
		for _, c := range parent.Children {
			if c.Name == name {
				return c
			}
		}
		c := make()
		parent.Children = append(parent.Children, c)
		return c
	}
	for _, m := range plan.Moves {
		st, err := os.Stat(filepath.Join(s.Root, m.From))
		isDir := err == nil && st.IsDir()
		leaf := &Node{Name: path.Base(m.To), Dir: isDir, State: "moved", Other: path.Base(m.From)}
		if m.Op == "trash" {
			bin := child(after, "Trash", func() *Node { return &Node{Name: "Trash", Dir: true, State: "trash"} })
			leaf.State = "trash"
			bin.Children = append(bin.Children, leaf)
			continue
		}
		parts := strings.Split(shown(m.To), "/")
		cur, sofar := after, plan.Path
		for _, part := range parts[:len(parts)-1] {
			sofar = path.Join(sofar, part)
			state := "new"
			if exists(filepath.Join(s.Root, sofar)) {
				state = "same"
			}
			cur = child(cur, part, func() *Node { return &Node{Name: part, Dir: true, State: state} })
		}
		cur.Children = append(cur.Children, leaf)
	}
	sortNodes(after.Children)
	sortNodes(plan.Before)
	plan.After = after.Children
}

func sortNodes(ns []*Node) {
	sort.SliceStable(ns, func(i, j int) bool {
		if (ns[i].State == "trash") != (ns[j].State == "trash") {
			return ns[j].State == "trash"
		}
		if ns[i].Dir != ns[j].Dir {
			return ns[i].Dir
		}
		return strings.ToLower(ns[i].Name) < strings.ToLower(ns[j].Name)
	})
	for _, n := range ns {
		sortNodes(n.Children)
	}
}

// rename moves from to to (both relative to the root), creating missing
// parent folders and recording them. It refuses to overwrite.
func (s *Server) rename(from, to string, created *[]string) error {
	absFrom, absTo := filepath.Join(s.Root, from), filepath.Join(s.Root, to)
	if !exists(absFrom) {
		return fmt.Errorf("%s no longer exists", from)
	}
	if exists(absTo) {
		return fmt.Errorf("%s already exists; nothing is overwritten", to)
	}
	if !s.insideRoot(absTo) {
		return fmt.Errorf("%s leads outside the workspace", to)
	}
	var missing []string
	for p := path.Dir(to); p != "." && p != "/" && !exists(filepath.Join(s.Root, p)); p = path.Dir(p) {
		missing = append(missing, p)
	}
	for i := len(missing) - 1; i >= 0; i-- {
		if err := os.Mkdir(filepath.Join(s.Root, missing[i]), 0o755); err != nil {
			return err
		}
		if created != nil {
			*created = append(*created, missing[i])
		}
	}
	return os.Rename(absFrom, absTo)
}

// removeCreated removes folders made for a plan, deepest first, and only if
// they are empty again.
func (s *Server) removeCreated(dirs []string) {
	for i := len(dirs) - 1; i >= 0; i-- {
		os.Remove(filepath.Join(s.Root, dirs[i])) // fails on non-empty folders, which is what we want
	}
}

// removeHolders removes the now-empty folders that held a trashed entry,
// up to but not including the trash itself.
func (s *Server) removeHolders(trashed string) {
	for p := path.Dir(trashed); p != trashName && p != "." && p != "/"; p = path.Dir(p) {
		os.Remove(filepath.Join(s.Root, p)) // empty folders only
	}
}

// remapRefs updates the reference data after entries moved from -> to.
func (s *Server) remapRefs(pairs [][2]string) {
	for i := range s.refs {
		ref := &s.refs[i]
		for _, p := range pairs {
			if under(ref.File, p[0]) {
				ref.File = p[1] + strings.TrimPrefix(ref.File, p[0])
			}
			if under(ref.Target, p[0]) || under(ref.Target, p[1]) {
				ref.Dangling = !exists(filepath.Join(s.Root, ref.Target))
			}
		}
	}
}

func (s *Server) apply(rel string, selected []int, repair bool) (*Tx, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	id := time.Now().Format("20060102-150405")
	s.mu.Lock()
	plan, used, err := s.buildPlan(rel, selected, id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if len(plan.Conflicts) > 0 {
		return nil, fmt.Errorf("the plan has conflicts: %s", strings.Join(plan.Conflicts, "; "))
	}
	if len(plan.Moves) == 0 {
		return nil, errors.New("nothing to move")
	}
	tx := &Tx{ID: id, At: time.Now(), Path: rel, Moves: plan.Moves, Created: []string{}, Proposals: used, Edits: []Edit{}, Status: "applying"}
	if err := s.saveTx(tx); err != nil {
		return nil, fmt.Errorf("could not write the journal, so nothing was moved: %w", err)
	}
	for i := range tx.Moves {
		m := &tx.Moves[i]
		if err := s.rename(m.From, m.To, &tx.Created); err != nil {
			// Put back what was already moved.
			for j := i - 1; j >= 0; j-- {
				if s.rename(tx.Moves[j].To, tx.Moves[j].From, nil) == nil {
					tx.Moves[j].Done = false
				}
			}
			s.removeCreated(tx.Created)
			tx.Status, tx.Error = "failed", err.Error()
			s.saveTx(tx)
			s.mu.Lock()
			s.txs = append(s.txs, tx)
			s.mu.Unlock()
			return nil, fmt.Errorf("%v. Everything was put back", err)
		}
		m.Done = true
		s.saveTx(tx)
	}
	if repair {
		tx.Problems = s.repair(tx)
	}
	tx.Status = "applied"
	s.saveTx(tx)

	s.mu.Lock()
	s.txs = append(s.txs, tx)
	pairs := make([][2]string, len(tx.Moves))
	for i, m := range tx.Moves {
		pairs[i] = [2]string{m.From, m.To}
	}
	s.remapRefs(pairs)
	if a := s.analyses[rel]; a != nil {
		applied := map[int]bool{}
		for _, m := range tx.Moves {
			applied[m.Proposal] = true
		}
		kept := []Proposal{}
		for i, p := range a.Proposals {
			if !applied[i] {
				kept = append(kept, p)
			}
		}
		c := *a
		c.Proposals = kept
		s.analyses[rel] = &c
	}
	snapshot := s.analysesCopy()
	s.mu.Unlock()
	s.saveJSON("analyses.json", snapshot)
	s.saveRefs()
	return tx, nil
}

func (s *Server) analysesCopy() map[string]*Analysis {
	out := make(map[string]*Analysis, len(s.analyses))
	for k, v := range s.analyses {
		out[k] = v
	}
	return out
}

// lastApplied returns the newest transaction that can be undone.
func (s *Server) lastApplied() *Tx {
	for i := len(s.txs) - 1; i >= 0; i-- {
		switch s.txs[i].Status {
		case "applied":
			return s.txs[i]
		case "undone", "failed":
			continue
		}
	}
	return nil
}

func (s *Server) undo(id string) (*Tx, error) {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.Lock()
	tx := s.lastApplied()
	s.mu.Unlock()
	if tx == nil || tx.ID != id {
		return nil, errors.New("only the most recent change can be undone")
	}
	// Files were edited at their new paths, so put them back before the moves.
	var skipped []string
	for _, f := range s.unrepair(tx) {
		skipped = append(skipped, "reference repair kept in "+f)
	}
	var pairs [][2]string
	for i := len(tx.Moves) - 1; i >= 0; i-- {
		m := &tx.Moves[i]
		if !m.Done || m.Restored {
			continue
		}
		if m.Purged {
			skipped = append(skipped, m.From+" was deleted from the trash")
			continue
		}
		if err := s.rename(m.To, m.From, nil); err != nil {
			skipped = append(skipped, err.Error())
			continue
		}
		m.Done = false
		pairs = append(pairs, [2]string{m.To, m.From})
	}
	s.removeCreated(tx.Created)
	s.removeHolders(path.Join(trashName, tx.ID, tx.Path, "x"))
	tx.Status = "undone"
	if len(skipped) > 0 {
		tx.Error = "not fully undone: " + strings.Join(skipped, "; ")
	}
	s.saveTx(tx)
	s.mu.Lock()
	for i := range s.refs {
		for _, p := range pairs {
			if under(s.refs[i].Target, p[0]) {
				s.refs[i].Target = p[1] + strings.TrimPrefix(s.refs[i].Target, p[0])
				break
			}
		}
	}
	s.remapRefs(pairs)
	if a := s.analyses[tx.Path]; a != nil && len(skipped) == 0 {
		c := *a
		c.Proposals = append(append([]Proposal{}, a.Proposals...), tx.Proposals...)
		s.analyses[tx.Path] = &c
	}
	snapshot := s.analysesCopy()
	s.mu.Unlock()
	s.saveJSON("analyses.json", snapshot)
	s.saveRefs()
	if len(skipped) > 0 {
		return tx, errors.New(tx.Error)
	}
	return tx, nil
}

// ---- trash --------------------------------------------------------------

type trashItem struct {
	Tx    string    `json:"tx"`
	Index int       `json:"index"`
	From  string    `json:"from"`
	To    string    `json:"to"`
	At    time.Time `json:"at"`
	Dir   bool      `json:"dir"`
	Why   string    `json:"why,omitempty"`
}

func (s *Server) trashItems() []trashItem {
	items := []trashItem{}
	for i := len(s.txs) - 1; i >= 0; i-- {
		tx := s.txs[i]
		for j, m := range tx.Moves {
			if m.Op != "trash" || !m.Done || m.Restored || m.Purged {
				continue
			}
			st, err := os.Lstat(filepath.Join(s.Root, m.To))
			if err != nil {
				continue
			}
			items = append(items, trashItem{Tx: tx.ID, Index: j, From: m.From, To: m.To, At: tx.At, Dir: st.IsDir(), Why: m.Reason})
		}
	}
	return items
}

func (s *Server) trashMove(id string, index int) (*Tx, *Move, error) {
	for _, tx := range s.txs {
		if tx.ID == id && index >= 0 && index < len(tx.Moves) {
			m := &tx.Moves[index]
			if m.Op == "trash" && m.Done && !m.Restored && !m.Purged && strings.HasPrefix(m.To, trashName+"/") {
				return tx, m, nil
			}
		}
	}
	return nil, nil, errors.New("no such item in the trash")
}

// ---- handlers -----------------------------------------------------------

func (s *Server) handlePlan(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string
		Selected []int
	}
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rel, _, err := s.resolve(req.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.mu.Lock()
	plan, _, err := s.buildPlan(rel, req.Selected, "pending")
	s.mu.Unlock()
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, plan)
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path     string
		Selected []int
		Repair   *bool
	}
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	rel, _, err := s.resolve(req.Path)
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	tx, err := s.apply(rel, req.Selected, req.Repair == nil || *req.Repair)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, tx)
}

func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	txs := []*Tx{}
	for i := len(s.txs) - 1; i >= 0; i-- {
		txs = append(txs, s.txs[i])
	}
	undoable := ""
	if tx := s.lastApplied(); tx != nil {
		undoable = tx.ID
	}
	writeJSON(w, map[string]any{"txs": txs, "undoable": undoable})
}

func (s *Server) handleUndo(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID string }
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	tx, err := s.undo(req.ID)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, tx)
}

func (s *Server) handleTrash(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	writeJSON(w, s.trashItems())
}

func (s *Server) handleTrashRestore(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tx    string
		Index int
	}
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.Lock()
	tx, m, err := s.trashMove(req.Tx, req.Index)
	s.mu.Unlock()
	if err == nil {
		err = s.rename(m.To, m.From, nil)
	}
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	s.removeHolders(m.To)
	s.mu.Lock()
	m.Restored = true
	s.remapRefs([][2]string{{m.To, m.From}})
	s.mu.Unlock()
	s.saveTx(tx)
	s.saveRefs()
	writeJSON(w, m)
}

// handleTrashDelete is the only code path that deletes anything. It removes
// one entry that is already in the trash, and only when the request repeats
// the entry's name.
func (s *Server) handleTrashDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Tx      string
		Index   int
		Confirm string
	}
	if err := readBody(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.Lock()
	tx, m, err := s.trashMove(req.Tx, req.Index)
	s.mu.Unlock()
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	if req.Confirm != path.Base(m.From) {
		fail(w, http.StatusBadRequest, errors.New("type the entry's name to confirm"))
		return
	}
	abs := filepath.Join(s.Root, m.To)
	parent, err := filepath.EvalSymlinks(filepath.Dir(abs))
	bin := filepath.Join(s.Root, trashName)
	if err != nil || !strings.HasPrefix(parent+string(filepath.Separator), bin+string(filepath.Separator)) {
		fail(w, http.StatusConflict, errors.New("that entry is not inside the trash"))
		return
	}
	if err := os.RemoveAll(filepath.Join(parent, filepath.Base(abs))); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.removeHolders(m.To)
	s.mu.Lock()
	m.Purged = true
	s.mu.Unlock()
	s.saveTx(tx)
	writeJSON(w, m)
}

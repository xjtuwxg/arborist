package ui

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xjtuwxg/arborist/internal/refs"
)

func tree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		if d.IsDir() {
			out[rel] = "<dir>"
		} else {
			b, _ := os.ReadFile(p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

func same(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	root, _ := filepath.EvalSymlinks(t.TempDir())
	for name, body := range map[string]string{
		"projA/main.go": "a", "projB/main.go": "b", "old/notes.txt": "old", "junk.tmp": "junk",
		"tools/keep.txt": "keep", "clash/x": "1", "dest/clash/y": "2",
	} {
		p := filepath.Join(root, name)
		os.MkdirAll(filepath.Dir(p), 0o755)
		os.WriteFile(p, []byte(body), 0o644)
	}
	s := &Server{Root: root, cacheDir: t.TempDir(), analyses: map[string]*Analysis{}}
	s.analyses[""] = &Analysis{Proposals: []Proposal{
		{Op: "move", From: []string{"projA", "projB"}, To: "code/go"},
		{Op: "archive", From: []string{"old"}},
		{Op: "trash", From: []string{"junk.tmp"}},
		{Op: "move", From: []string{"clash"}, To: "dest"},
	}}
	s.refs = []refs.Ref{{File: "tools/keep.txt", Target: "projA/main.go"}, {File: "projA/main.go", Target: "tools/keep.txt"}}
	s.scanned = []string{""}
	return s
}

func TestApplyUndo(t *testing.T) {
	s := newTestServer(t)
	before := tree(t, s.Root)

	plan, _, err := s.buildPlan("", nil, "x")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Conflicts) != 1 {
		t.Fatalf("want 1 conflict (dest/clash exists), got %v", plan.Conflicts)
	}
	if _, err := s.apply("", nil); err == nil {
		t.Fatal("apply must refuse a plan with conflicts")
	}
	if !same(before, tree(t, s.Root)) {
		t.Fatal("refused apply changed the workspace")
	}

	tx, err := s.apply("", []int{0, 1, 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"code/go/projA/main.go", "code/go/projB/main.go", "_archive/old/notes.txt", "tools/keep.txt"} {
		if !exists(filepath.Join(s.Root, p)) {
			t.Errorf("missing after apply: %s", p)
		}
	}
	if exists(filepath.Join(s.Root, "junk.tmp")) || len(s.trashItems()) != 1 {
		t.Errorf("junk.tmp should be in the trash: %+v", s.trashItems())
	}
	if b, _ := os.ReadFile(filepath.Join(s.Root, s.trashItems()[0].To)); string(b) != "junk" {
		t.Errorf("trashed content changed: %q", b)
	}
	if !s.refs[0].Dangling || s.refs[1].File != "code/go/projA/main.go" || s.refs[1].Dangling {
		t.Errorf("refs not remapped: %+v", s.refs)
	}
	if len(s.analyses[""].Proposals) != 1 {
		t.Errorf("applied proposals should be gone: %+v", s.analyses[""].Proposals)
	}

	if _, err := s.undo(tx.ID); err != nil {
		t.Fatal(err)
	}
	if after := tree(t, s.Root); !same(before, after) {
		t.Fatalf("undo did not restore the tree:\nbefore %v\nafter  %v", before, after)
	}
	if s.refs[0].Dangling || s.refs[1].File != "projA/main.go" {
		t.Errorf("refs not restored: %+v", s.refs)
	}
	if _, err := s.undo(tx.ID); err == nil {
		t.Error("a second undo must be refused")
	}
}

func TestApplyRefusesStalePlan(t *testing.T) {
	s := newTestServer(t)
	s.analyses[""] = &Analysis{Proposals: []Proposal{{Op: "move", From: []string{"projA", "projB"}, To: "code"}}}
	before := tree(t, s.Root)
	plan, _, _ := s.buildPlan("", nil, "x")
	if len(plan.Moves) != 2 {
		t.Fatalf("moves: %+v", plan.Moves)
	}
	os.Remove(filepath.Join(s.Root, "projB", "main.go"))
	os.Remove(filepath.Join(s.Root, "projB"))
	delete(before, "projB")
	delete(before, "projB/main.go")
	// projB vanished between dry run and apply: the plan now has a conflict.
	if _, err := s.apply("", nil); err == nil {
		t.Fatal("apply should fail")
	}
	if !same(before, tree(t, s.Root)) {
		t.Fatalf("failed apply left changes: %v", tree(t, s.Root))
	}
}

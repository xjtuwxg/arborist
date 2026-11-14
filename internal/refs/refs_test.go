package refs

import (
	"os"
	"path/filepath"
	"testing"
)

func TestScan(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/data.txt", "x")
	write("a/run.sh", "#!/bin/sh\ncat "+root+"/a/data.txt\n")
	write("b/cfg.ini", "input="+root+"/a/data.txt\nold=\""+root+"/gone/file\"\n")
	write("b/venv/bin/activate", "VIRTUAL_ENV="+root+"/b/venv\n")
	write("b/blob.bin", "\x00\x00"+root+"/a/data.txt")
	if err := os.Symlink(filepath.Join(root, "a"), filepath.Join(root, "b", "link")); err != nil {
		t.Fatal(err)
	}

	found, stats, err := Scan(root, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]Ref{}
	for _, r := range found {
		got[r.File+"->"+r.Target] = r
	}
	if len(found) != 5 {
		t.Fatalf("want 5 refs, got %d: %+v", len(found), found)
	}
	if r := got["a/run.sh->a/data.txt"]; r.Line != 2 || r.Cross || r.Dangling || r.Category != "authored" {
		t.Errorf("run.sh: %+v", r)
	}
	if r := got["b/cfg.ini->a/data.txt"]; !r.Cross || r.Line != 1 {
		t.Errorf("cfg.ini cross: %+v", r)
	}
	if r := got["b/cfg.ini->gone/file"]; !r.Dangling || r.Line != 2 {
		t.Errorf("cfg.ini dangling: %+v", r)
	}
	if r := got["b/venv/bin/activate->b/venv"]; r.Category != "python-env" {
		t.Errorf("venv: %+v", r)
	}
	if r := got["b/link->a"]; r.Kind != "symlink" || !r.Cross {
		t.Errorf("symlink: %+v", r)
	}
	if stats.SkippedBin != 1 {
		t.Errorf("binary skipped = %d, want 1", stats.SkippedBin)
	}
}

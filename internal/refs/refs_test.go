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

func TestCacheAndIgnore(t *testing.T) {
	root, _ := filepath.EvalSymlinks(t.TempDir())
	write := func(rel, body string) {
		p := filepath.Join(root, rel)
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a/data.txt", "x")
	write("a/run.sh", "cat "+root+"/a/data.txt\n")
	write("b/cfg.ini", "input="+root+"/a/data.txt\n")
	write("big/trees/t1/Makefile", "SRC="+root+"/a\n")
	write("x/node_modules/pkg/index.js", "// "+root+"/a\n")
	cache := filepath.Join(t.TempDir(), "scan.gob")
	opt := Options{Cache: cache, Ignore: []string{"/big/trees", "node_modules"}}
	keys := func(rs []Ref) map[string]bool {
		m := map[string]bool{}
		for _, r := range rs {
			m[r.File+"->"+r.Target] = true
		}
		return m
	}

	first, st, err := Scan(root, opt)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || st.Ignored != 2 || st.Reused != 0 || st.Scanned != 2 {
		t.Fatalf("first scan: %d refs, stats %+v", len(first), st)
	}

	second, st, _ := Scan(root, opt)
	if st.Scanned != 0 || st.Reused != 3 || len(second) != 2 {
		t.Fatalf("second scan should read nothing: %d refs, stats %+v", len(second), st)
	}

	// One folder changes: only its files are read again, and the result is current.
	write("b/cfg.ini", "input="+root+"/a/run.sh\nother="+root+"/gone\n")
	third, st, _ := Scan(root, opt)
	got := keys(third)
	if st.Scanned != 1 || st.Reused != 2 || !got["b/cfg.ini->a/run.sh"] || !got["b/cfg.ini->gone"] || got["b/cfg.ini->a/data.txt"] || !got["a/run.sh->a/data.txt"] {
		t.Fatalf("third scan: %v, stats %+v", got, st)
	}

	// A target disappears: the cached reference is reported as dangling without a reread.
	os.Remove(filepath.Join(root, "a/data.txt"))
	fourth, _, _ := Scan(root, Options{Cache: cache, Ignore: opt.Ignore, Sub: "a"})
	if len(fourth) != 1 || !fourth[0].Dangling {
		t.Fatalf("fourth scan: %+v", fourth)
	}
	// A scan of one subfolder must not forget the rest.
	fifth, st, _ := Scan(root, opt)
	if len(fifth) != 3 || st.Scanned != 0 || st.Reused != 2 { // the subfolder scan already refreshed a/
		t.Fatalf("fifth scan: %d refs, stats %+v", len(fifth), st)
	}

	if !IgnoredPath(opt.Ignore, "big/trees/t1") || IgnoredPath(opt.Ignore, "big") || !IgnoredPath(opt.Ignore, "x/node_modules/pkg") {
		t.Error("IgnoredPath")
	}
}

func TestCategorize(t *testing.T) {
	for file, want := range map[string]string{
		"p/src/main.c":                     "authored",
		"p/run.sh":                         "authored",
		"p/build-static/meson-info/x.json": "build",
		"p/src/foo.dep":                    "build",
		"p/Makefile":                       "authored",
		"p/src/.depend":                    "build",
		"k/tools/objtool/.check.o.cmd":     "build",
		"p/scripts/deploy.cmd":             "authored",
		"p/eval/all.csv":                   "data",
		"p/dataset/sessions/a.jsonl":       "data",
		"p/runs/r0/result.json":            "data",
		"p/runs/r0/logs/stderr.txt":        "log",
		"p/data/.venv/bin/activate":        "python-env",
		"p/.claude/settings.local.json":    "agent",
		"p/results/CLAUDE.md":              "agent",
	} {
		if got := Categorize(file); got != want {
			t.Errorf("%s: got %s, want %s", file, got, want)
		}
	}
}

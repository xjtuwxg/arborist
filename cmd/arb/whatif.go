package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/xjtuwxg/arborist/internal/refs"
	"github.com/xjtuwxg/arborist/internal/ui"
)

// runWhatIf answers: if the AI's proposals for a workspace were carried out
// with a plain mv, which absolute references would stop resolving?
//
// A reference breaks when its target lies under a moved path. References
// whose file moves but whose target stays put keep working, because they are
// absolute. Already-dangling references are not counted.
func runWhatIf(args []string) {
	fl := flag.NewFlagSet("whatif", flag.ExitOnError)
	refsFile := fl.String("refs", "", "scan made by `arb refs <root> --json` (required)")
	asJSON := fl.Bool("json", false, "print the report as JSON")
	root, rest := splitRoot(args)
	fl.Parse(rest)
	if root == "" || *refsFile == "" {
		fmt.Fprintln(os.Stderr, "usage: arb whatif <root> --refs scan.json [--json]")
		os.Exit(2)
	}
	analyses, err := ui.LoadAnalyses(root)
	if err != nil || len(analyses) == 0 {
		fmt.Fprintln(os.Stderr, "arb whatif: no AI analysis stored for this root; run one in `arb ui` first")
		os.Exit(1)
	}
	f, err := os.Open(*refsFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "arb whatif:", err)
		os.Exit(1)
	}
	var scan struct {
		Refs []refs.Ref `json:"refs"`
	}
	if err := json.NewDecoder(f).Decode(&scan); err != nil {
		fmt.Fprintln(os.Stderr, "arb whatif:", err)
		os.Exit(1)
	}

	refs.Recategorize(scan.Refs)

	type row struct {
		Folder   string         `json:"folder"`
		Op       string         `json:"op"`
		From     []string       `json:"from"`
		To       string         `json:"to"`
		Breaks   int            `json:"breaks"`
		Inbound  int            `json:"inbound"`  // from files outside the moved paths
		Internal int            `json:"internal"` // from files that move along
		Files    int            `json:"files"`
		ByCat    map[string]int `json:"by_category"`
	}
	var rows []*row
	var moved []string
	owner := map[string]*row{}
	for folder, a := range analyses {
		for _, p := range a.Proposals {
			r := &row{Folder: folder, Op: p.Op, From: p.From, To: p.To, ByCat: map[string]int{}}
			rows = append(rows, r)
			for _, from := range p.From {
				full := path.Join(folder, from)
				moved = append(moved, full)
				owner[full] = r
			}
		}
	}
	sort.Slice(moved, func(i, j int) bool { return len(moved[i]) > len(moved[j]) }) // deepest first
	under := func(p string) string {
		for _, m := range moved {
			if p == m || strings.HasPrefix(p, m+"/") {
				return m
			}
		}
		return ""
	}
	files := map[*row]map[string]bool{}
	live, dangling := 0, 0
	total := &row{ByCat: map[string]int{}}
	allFiles := map[string]bool{}
	for _, ref := range scan.Refs {
		if ref.Dangling {
			dangling++
			continue
		}
		live++
		m := under(ref.Target)
		if m == "" {
			continue
		}
		r := owner[m]
		internal := under(ref.File) == m
		for _, x := range []*row{r, total} {
			x.Breaks++
			x.ByCat[ref.Category]++
			if internal {
				x.Internal++
			} else {
				x.Inbound++
			}
		}
		if files[r] == nil {
			files[r] = map[string]bool{}
		}
		files[r][ref.File] = true
		allFiles[ref.File] = true
	}
	breaking := 0
	for _, r := range rows {
		r.Files = len(files[r])
		if r.Breaks > 0 {
			breaking++
		}
	}
	total.Files = len(allFiles)
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].Breaks > rows[j].Breaks })

	if *asJSON {
		json.NewEncoder(os.Stdout).Encode(map[string]any{
			"live_refs": live, "dangling_refs": dangling, "proposals": rows, "total": total,
			"proposals_that_break": breaking, "paths_moved": len(moved),
		})
		return
	}
	fmt.Printf("references in scan: %d live, %d already dangling\n", live, dangling)
	fmt.Printf("plan: %d proposals moving %d paths; %d proposals break at least one reference\n\n", len(rows), len(moved), breaking)
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "op\tfrom\tto\tbreaks\tinbound\tinternal\tfiles\tauthored+agent")
	line := func(r *row, from string) {
		fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\n", r.Op, from, r.To, r.Breaks, r.Inbound, r.Internal, r.Files, r.ByCat["authored"]+r.ByCat["agent"])
	}
	for _, r := range rows {
		from := strings.Join(r.From, ",")
		if len(from) > 48 {
			from = fmt.Sprintf("%s,… (%d paths)", r.From[0], len(r.From))
		}
		line(r, from)
	}
	total.Op = "TOTAL"
	line(total, "")
	w.Flush()
	fmt.Println("\nbroken references by where the referring file lives:")
	cats := make([]string, 0, len(total.ByCat))
	for c := range total.ByCat {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return total.ByCat[cats[i]] > total.ByCat[cats[j]] })
	for _, c := range cats {
		fmt.Printf("  %-12s %d\n", c, total.ByCat[c])
	}
}

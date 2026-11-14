// Command arb is the Arborist CLI.
//
//	arb refs <root> [--json] [--max-size bytes]   report absolute path references pointing into <root>
//	arb ui [root] [--listen addr] [--tailscale]   serve the web interface
//	arb whatif <root> --refs scan.json            what a plain mv of the AI's proposals would break
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"sort"
	"text/tabwriter"

	"github.com/xjtuwxg/arborist/internal/refs"
)

func main() {
	if len(os.Args) >= 2 && os.Args[1] == "ui" {
		runUI(os.Args[2:])
		return
	}
	if len(os.Args) >= 2 && os.Args[1] == "whatif" {
		runWhatIf(os.Args[2:])
		return
	}
	if len(os.Args) < 2 || os.Args[1] != "refs" {
		fmt.Fprintln(os.Stderr, "usage:\n  arb refs <root> [--json] [--max-size bytes]\n  arb ui [root] [--listen addr] [--tailscale] [--refs scan.json]\n  arb whatif <root> --refs scan.json [--json]")
		os.Exit(2)
	}
	fl := flag.NewFlagSet("refs", flag.ExitOnError)
	asJSON := fl.Bool("json", false, "print every reference as JSON")
	maxSize := fl.Int64("max-size", 1<<20, "skip text files larger than this many bytes")
	root, args := splitRoot(os.Args[2:])
	fl.Parse(args)
	if root == "" {
		root = fl.Arg(0)
	}
	if root == "" {
		fmt.Fprintln(os.Stderr, "arb refs: missing <root>")
		os.Exit(2)
	}

	found, stats, err := refs.Scan(root, refs.Options{MaxFileSize: *maxSize, Progress: func(n int64) {
		if n%200000 == 0 {
			fmt.Fprintf(os.Stderr, "%d files seen\n", n)
		}
	}})
	if err != nil {
		fmt.Fprintln(os.Stderr, "arb refs:", err)
		os.Exit(1)
	}
	if *asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.Encode(map[string]any{"stats": stats, "refs": found})
		return
	}
	summarize(found, stats)
}

// splitRoot lets the root come before the flags.
func splitRoot(args []string) (string, []string) {
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		return args[0], args[1:]
	}
	return "", args
}

func summarize(found []refs.Ref, stats refs.Stats) {
	type row struct{ total, cross, dangling, files int }
	byCat := map[string]*row{}
	seen := map[string]bool{}
	projects := map[string]bool{}
	var all row
	for _, r := range found {
		c := byCat[r.Category]
		if c == nil {
			c = &row{}
			byCat[r.Category] = c
		}
		for _, x := range []*row{c, &all} {
			x.total++
			if r.Cross {
				x.cross++
			}
			if r.Dangling {
				x.dangling++
			}
		}
		if !seen[r.File] {
			seen[r.File] = true
			c.files++
			all.files++
		}
		for i := 0; i < len(r.File); i++ {
			if r.File[i] == '/' {
				projects[r.File[:i]] = true
				break
			}
		}
	}
	fmt.Printf("files seen %d, text files scanned %d, skipped: %d large, %d binary, %d unreadable\n",
		stats.Files, stats.Scanned, stats.SkippedBig, stats.SkippedBin, stats.Unreadable)
	fmt.Printf("top-level projects containing references: %d\n\n", len(projects))

	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Slice(cats, func(i, j int) bool { return byCat[cats[i]].total > byCat[cats[j]].total })
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "category\trefs\tfiles\tcross-project\tdangling\t")
	for _, c := range cats {
		r := byCat[c]
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t\n", c, r.total, r.files, r.cross, r.dangling)
	}
	fmt.Fprintf(w, "total\t%d\t%d\t%d\t%d\t\n", all.total, all.files, all.cross, all.dangling)
	w.Flush()
}

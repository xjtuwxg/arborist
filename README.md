# Arborist

**An agentic file manager that can reorganize a workspace without breaking it.**

Personal and development machines rot: projects drift away from the folder they were
created in, duplicates pile up, dead experiments sit next to live ones. LLM
agents are now good at deciding where things *should* go. They are bad at
moving them, because a move is not a classification problem. Paths are
referenced from scripts, configs, virtual environments, build caches, git
worktrees and agent memory, and a plain `mv` breaks all of them silently.

Arborist treats reorganization as **refactoring a file system**: find what
refers to what, move things as a transaction, repair the references, and be
able to undo.

Status: research prototype. See [docs/research-plan.md](docs/research-plan.md).

## What works today

```bash
go build -o arb ./cmd/arb
./arb ui ~/workspace                 # web interface on 127.0.0.1:7878
./arb ui ~/workspace --tailscale     # same, on this machine's Tailscale address
./arb refs ~/workspace               # reference summary by category
./arb refs ~/workspace --json > scan.json   # --ignore a,b  --no-cache
./arb whatif ~/workspace --refs scan.json
```

The workspace changes only when you confirm the preview in the web interface.
Even then Arborist only renames: it never overwrites and never deletes on its
own. Its journal and cache live in `~/.cache/arborist` on Linux.

**Web interface (`arb ui`).** A file browser over one workspace root. It
prints a link that carries an access token; the token is stored so the link
stays the same across restarts. Per folder you can:

- *Scan references*: find absolute paths (symlinks and text files) that point
  into the workspace, see what points into a folder and what its files point
  to, and which references are already dangling.
- *Analyze*: describe every entry, label it by topic and activity, and propose
  `move` / `merge` / `archive` / `trash` steps. Each proposal shows how many
  references the move would break.
- *Tidy up*: see the folder as it is now next to how it would look, untick
  steps you do not want, then apply. A plan with any conflict (a destination
  that already exists, an entry that vanished) is refused as a whole.
- *History*: every applied plan is journaled; the most recent one can be
  undone, which puts each entry back at its old path.
- *Trash*: `trash` steps move entries into `_trash/` at the workspace root.
  They stay there, intact, until you restore them or delete them one at a
  time by typing the entry's name. That is the only code path that deletes.

- *Reference repair*: on apply, absolute paths that pointed at a moved entry
  are rewritten to the new path, in text files and symlinks, using the scan
  data. Each edited file is copied into the journal first and undo puts the
  exact bytes back; a file you changed after the repair is left alone and
  reported. Log files are never rewritten. Repair only knows what the scan
  saw: scan before you reorganize.

**Scans are incremental.** The first scan of a large workspace on a spinning
disk is slow, because every text file is read. Later scans reuse what was
found in any folder whose files have the same names, sizes and modification
times, and open only files in folders that changed. The command line and the
web interface share the cache (`--no-cache` rereads everything).

**Leaving folders out.** Lines in `.arboristignore` in the workspace root name
folders and files that scans skip: `/data/trees` is a path from the root,
`node_modules` or `*.ckpt` match a name anywhere. The web interface has an
"Exclude from scans" button per folder that edits this file. References
inside an excluded folder are unknown, so the dry run says when a move
touches one.

Not repaired yet: paths built at run time, files over the scan's size limit,
references from outside the workspace (shell rc files, crontab, agent state
under `~/.claude` and `~/.codex`).

With `--tailscale` the printed link uses the machine's tailnet address and
works from any device on the tailnet, no SSH tunnel needed.

**AI without setup.** On start Arborist looks for AI command-line tools that
are already installed and signed in (Claude Code, Codex, Gemini CLI, opencode,
Ollama) and uses one of them; there are no API keys to enter. The sidebar
lets you pick the provider and the model (suggestions come from the tool where
it can list them; any model name the tool accepts can be typed). It only checks
that a tool's credential file exists and never reads it. The tool is run in an
empty directory with its own tools disabled or read-only, because folder names
and README text are untrusted input. Analysis sends folder names and the first
lines of README files to the chosen tool.

**Measurement (`arb refs`, `arb whatif`).** `refs` reports every absolute
reference with the kind of file it lives in (`authored`, `build`,
`python-env`, `node`, `git`, `agent`, `log`, `data`). The kind comes from the
path alone, so `arb refs --from scan.json` re-summarizes a stored scan under
the current rules without touching the disk. `whatif` takes the AI proposals
stored by the UI and counts the references that would stop resolving if they
were carried out with a plain `mv`.

## Planned pieces

| Piece | Role | Source |
|---|---|---|
| Reference graph | what breaks if X moves | `internal/refs` (started) |
| Transactional move + undo | apply a plan, journal it, roll back | `internal/ui/apply.go` (renames only); agent-state migration from [AgentReRoot](https://github.com/xjtuwxg/AgentReRoot) to do |
| Reference repair | rewrite or regenerate what a move breaks | `internal/ui/repair.go` (rewrite; regenerating environments to do) |
| Semantic index | per-file and per-folder summaries and embeddings in a sidecar database, no new file system | to do |
| Views before moves | show the proposed layout virtually, materialize after review | to do |
| Agent loop | propose, dry-run, apply, verify | propose only (`internal/providers`) |
| File manager UI | browse, review proposals, undo | `internal/ui` |

AgentReRoot keeps its packages under `internal/`, so they cannot be imported
from this module yet. Either call the `reroot` binary or move its engine to a
public package first.

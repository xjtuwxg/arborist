# Arborist

**An agentic file manager that can reorganize a workspace without breaking it.**

Personal and lab machines rot: projects drift away from the folder they were
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
./arb refs ~/workspace --json > scan.json
./arb whatif ~/workspace --refs scan.json
```

Nothing here changes the workspace. Arborist writes only to its cache
directory (`~/.cache/arborist` on Linux).

**Web interface (`arb ui`).** A file browser over one workspace root. It
prints a link that carries an access token; the token is stored so the link
stays the same across restarts. Per folder you can:

- *Scan references*: find absolute paths (symlinks and text files) that point
  into the workspace, see what points into a folder and what its files point
  to, and which references are already dangling.
- *Analyze with AI*: describe every entry, label it by topic and activity, and
  propose `move` / `merge` / `archive` steps. Each proposal shows how many
  references a plain move would break.

**AI without setup.** On start Arborist looks for AI command-line tools that
are already installed and signed in (Claude Code, Codex, Gemini CLI, opencode,
Ollama) and uses one of them; there are no API keys to enter. It only checks
that a tool's credential file exists and never reads it. The tool is run in an
empty directory with its own tools disabled or read-only, because folder names
and README text are untrusted input. Analysis sends folder names and the first
lines of README files to the chosen tool.

**Measurement (`arb refs`, `arb whatif`).** `refs` reports every absolute
reference with the kind of file it lives in (`authored`, `build`,
`python-env`, `node`, `git`, `agent`, `log`). `whatif` takes the AI proposals
stored by the UI and counts the references that would stop resolving if they
were carried out with a plain `mv`.

## Planned pieces

| Piece | Role | Source |
|---|---|---|
| Reference graph | what breaks if X moves | `internal/refs` (started) |
| Transactional move + undo | apply a plan, journal it, roll back | [AgentReRoot](https://github.com/xjtuwxg/AgentReRoot) |
| Semantic index | per-file and per-folder summaries and embeddings in a sidecar database, no new file system | to do |
| Views before moves | show the proposed layout virtually, materialize after review | to do |
| Agent loop | propose, dry-run, apply, verify | propose only (`internal/providers`) |
| File manager UI | browse, review proposals, undo | `internal/ui` (browse and review; no apply yet) |

AgentReRoot keeps its packages under `internal/`, so they cannot be imported
from this module yet. Either call the `reroot` binary or move its engine to a
public package first.

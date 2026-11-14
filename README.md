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

Status: research prototype, day one. See [docs/research-plan.md](docs/research-plan.md).

## What works today

```bash
go build -o arb ./cmd/arb
./arb refs ~/workspace          # summary by category
./arb refs ~/workspace --json   # every reference
```

`arb refs` is read-only. It reports absolute path references that point into
the workspace (symlinks and text files), where the referring file lives
(`authored`, `build`, `python-env`, `node`, `git`, `agent`, `log`), whether
the reference crosses top-level projects, and whether it is already dangling.

## Planned pieces

| Piece | Role | Source |
|---|---|---|
| Reference graph | what breaks if X moves | `internal/refs` (started) |
| Transactional move + undo | apply a plan, journal it, roll back | [AgentReRoot](https://github.com/xjtuwxg/AgentReRoot) |
| Semantic index | per-file and per-folder summaries and embeddings in a sidecar database, no new file system | to do |
| Views before moves | show the proposed layout virtually, materialize after review | to do |
| Agent loop | propose, dry-run, apply, verify | to do |
| File manager UI | browse, review proposals, undo | to do; start from the reroot web UI |

AgentReRoot keeps its packages under `internal/`, so they cannot be imported
from this module yet. Either call the `reroot` binary or move its engine to a
public package first.

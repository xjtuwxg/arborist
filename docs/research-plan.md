# Research plan

Paper first. The product (file manager UI) follows the system, not the other
way round.

## Claim

Agents that reorganize files today treat a move as free. On developer and lab
machines it is not: workspaces are full of absolute path references, and a
semantically correct reorganization can still break builds, environments,
experiments and agent memory. Reorganization needs the same guarantees as code
refactoring: reference-preserving, atomic, reversible.

## What is not the contribution

- Using an LLM to classify files into folders (LlamaFS, Local-File-Organizer,
  Sparkle, Filer AI, Claude Cowork all do this).
- A semantic file system or vector index over files (LSFS, ICLR 2025).
- Recommending where a new file goes (KondoCloud, UIST 2021; PaperRouter-Agent).

## Research questions

1. **How entangled are real workspaces?** How many absolute references exist,
   where do they live, how many cross project boundaries, how many are already
   dangling? (`arb refs` measures this.)
2. **How much do existing agentic organizers break?** Run them on snapshots of
   real workspaces, then check builds, tests, environments and agent state.
3. **Can moves be made safe?** Reference graph + transactional apply + repair
   (rewrite authored files, regenerate derived state) + undo. What fraction of
   breakage is prevented, at what cost?
4. **Does safety change behavior?** With undo and a dry run, do users accept
   larger reorganizations? (Only needed for an HCI venue.)

## Evaluation sketch

- **Corpus**: snapshots of real workspaces (this lab's machines first; more
  via volunteers, with a scanner that exports counts only, never contents).
- **Breakage oracle**: per project, a check that passed before the move
  (build, test, `python -c import`, `git status`, agent session resume).
- **Baselines**: plain `mv` following an LLM plan; LlamaFS; a coding agent
  told to "tidy up"; AgentReRoot alone (agent state only).
- **Metrics**: checks broken, references repaired, undo fidelity (byte-for-byte
  tree comparison), time and tokens.

## Risks

- The measurement may show breakage is rare outside developer machines. Then
  the scope is "developer and lab workspaces", which is still the target.
- References that are computed at run time (`os.path.join(HOME, ...)`,
  environment variables) are invisible to a static scan. Needs a dynamic
  component (trace opens) or an honest limitation.
- References from outside the workspace into it (shell rc, crontab, systemd
  units, other users on a shared server) are not scanned yet.
- Platform vendors may ship organization features; they are unlikely to ship
  reference repair for developer state.

## Milestones

1. `arb refs` on several real workspaces; first numbers. (in progress)
2. Breakage oracle and the baseline study (RQ2). This decides whether the
   paper exists.
3. Engine: reference-aware plan, apply, repair, undo, on top of AgentReRoot.
4. Semantic index and agent loop.
5. File manager UI.

Venues to aim at, by framing: USENIX ATC / EuroSys / FAST (system and
measurement), CHI / UIST (if RQ4 becomes the center).

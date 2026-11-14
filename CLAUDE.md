@AGENTS.md

# Fork rules (ImIOImI/argo-cd, multi-hub research spike)

These apply on top of AGENTS.md. They live here, not in AGENTS.md, so
upstream rebases don't conflict.

## Recording research

- Findings go in `research/`. Write one dated note per investigation
  (`research/YYYY-MM-DD-<slug>.md`) and list it in `research/README.md`,
  including the scoreboard if the note changes a measured number.
- Raw evidence (controller logs, Application YAML, lab samples) goes in
  `research/data/YYYY-MM-DD-<slug>/`. `hack/multi-hub-lab/collect.sh` saves
  it. Check for credentials before committing; `collect.sh` refuses to leave
  them.
- Every note has a **How it was run** section: each command, the directory
  it ran from, the environment (tool versions), and a link to every script
  used, **pinned to the commit SHA the script was at when it ran**
  (`https://github.com/ImIOImI/argo-cd/blob/<sha>/<path>`), never a branch
  link.
- No unrecorded commands. Anything run by hand gets saved as a script in
  `hack/multi-hub-lab/` and committed *before* the note cites it. The note
  then says the script was saved after the fact and was re-run to confirm it
  reproduces the result.
- Commit the scripts first, then the note that cites their SHA.
- Measure churn from controller events (`hack/multi-hub-lab/syncs.sh`, one
  line per started sync), not by polling the spoke. Reverts are often undone
  between samples.

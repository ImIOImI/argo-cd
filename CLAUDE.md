@AGENTS.md

# Fork rules (ImIOImI/argo-cd, multi-hub research spike)

These apply on top of AGENTS.md. They live here, not in AGENTS.md, so
upstream rebases don't conflict.

## Test-driven development (mandatory)

All new implementation in this fork is written test-first, and every
implementation has a **red test commit** before it. This covers Go code
(e.g. `controller/multihub`, the controller glue, anything else we add or
change) and tooling under `hack/tdd/`. Bug fixes too: reproduce the bug with
a failing test first.

1. **Red commit first:** `test(red): <behaviour>`. It contains the new or
   changed tests plus, where needed, **compiling stubs** (zero values,
   `return nil, nil`, no-op) so the tests fail on **assertions, not build
   errors**. The commit body lists the failing tests and the `go test`
   summary.
2. **Green commit next:** `feat|fix(<area>): …`. It contains the
   implementation only. Don't change tests in the green commit; if a test was
   wrong, go back to a new red commit. The body includes `Red: <sha>`.
3. **Prove the pair:** `hack/tdd/red-green.sh <red> <green> <pkg> [test-regex]`
   must print `PROVEN`. Paste its output in the green commit body, and in
   the research note's "How it was run" section when there is one. It
   rejects a red that passes, a red that only fails to build (unless
   `ALLOW_BUILD_FAILURE=1`, which needs a reason in the commit body), a green
   that fails or runs no tests, and a green that doesn't descend from its
   red.
4. **Refactors:** `refactor: …`. Tests are unchanged and green before and
   after.
5. Push red and green together. A red commit on its own is expected to fail.
6. **Non-Go tooling** (shell under `hack/tdd/`): tests are executable
   `*_test.sh` scripts, and the red commit body records the pass/fail counts.
   When no-script and stub runs each leave some cases passing for the wrong
   reason, record both runs so every behavioural case is shown red once.
   Example: `93dafdac0` → `edef78088`.
7. **Exempt:** lab experiment scripts in `hack/multi-hub-lab/` (instruments
   run against the live lab, covered by "no unrecorded commands" below),
   research notes, and docs.
8. **History:** the C2 operation lease (`aeb859186`) and its telemetry
   (`e028e0111`) predate this rule and were written test-after. Don't
   rewrite them; any further change to that code follows this rule.

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

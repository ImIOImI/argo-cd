# Multi-hub lab

Three kind clusters: `hub-a` and `hub-b` both run **stock** Argo CD and both
manage the same `lab-app` on `spoke`. It measures how two controllers fight
today, which sets the baseline the multi-hub fork has to beat. Design notes:
[docs/proposals/multi-hub-active-active.md](../../docs/proposals/multi-hub-active-active.md).

```bash
./up.sh          # ~5 min: 3 clusters, Argo CD on both hubs, app registered on both
./observe.sh     # second terminal: one line every 2s
./bump.sh        # push one commit that changes the app
./down.sh
```

The hubs poll Git at different intervals (hub-a 30s, hub-b 180s, override with
`HUB_A_RECONCILE` / `HUB_B_RECONCILE`), so after `bump.sh` there is a window
where hub-a knows the new commit and hub-b does not.

## Experiments

| # | Run | Watch for |
|---|-----|-----------|
| E1 | Steady state, no pushes, 10 min | `cm_rv` should not move. If it does, the hubs fight even when they agree. |
| E2 | One `bump.sh` | `cm_gen` flipping between N and N+1, `cm_rv` climbing, until hub-b catches up. Time the window. |
| E3 | Count `hooks` after E2 | More than one new PreSync Job per commit = duplicated hook runs. |
| E4 | Repeat E2 with `selfHeal: false` on both hubs | The hypothesis is that skew churn needs self-heal. |
| E5 | Manual rollback on hub-b to an older revision | Hub-a's self-heal should undo it: the imperative-ops gap. |

`observe.log` keeps every sample (gitignored).

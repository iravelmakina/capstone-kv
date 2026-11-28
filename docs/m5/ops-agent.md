# M5: the ops agent (ADR-0034, task 7.5)

The ops agent is L1: it reads the store through five read-only tools and proposes one
action; a person approves it on the host (`make ops-approve`); kvscale executes it after
checking its guards again. This file holds the defense demo. **The transcript sections are
empty until the author runs them**: every run calls Claude (`claude-opus-5-5`) and is billed
to the author's `ANTHROPIC_API_KEY`.

## What was checked without calling the model

- `cd cmd/kvops && go test ./...`: the loop with a fake model (a tool call then a proposal;
  refusal; no proposal; a malformed proposal retried), the evaluation set loads and grades.
- `make ops-image`: the image is 13.8 MB, distroless, runs as `nonroot:nonroot`, has no
  shell (`docker run --entrypoint ls kvops:local` fails: no `ls` in the image).
- `make -n ops-agent`: the container gets `ANTHROPIC_API_KEY`, the `ops-agent` key (scope
  `status`), `ops/proposals`, `ops/runs` and kvscale's log read-only, nothing else: no
  `~/.aws`, no admin key, no `ops/approvals`. With the status key the agent can read
  `/v1/admin/cluster` and nothing more (`internal/auth`: status is the only scope it has).
- `kvops eval` without a key: every situation fails with "no Anthropic credentials", and no
  request leaves the container.

## Steps for the demo (local cluster)

```bash
make local-up && make local-seed          # two sets, data
make ops-key                              # the ops agent's status-scoped key
make alertmanager                          # optional: proposals reach the chat
make autoscale-local AUTOSCALE=no          # kvscale executes approvals, decides nothing itself
```

1. **A proposal that is approved and executed**: make two leaders share a node, e.g.
   `curl -X POST -H "Authorization: Bearer $(cat secrets/ops.key)" localhost:8080/v1/admin/shards/shard-2/leader -d '{"to":"2b"}'`
   when shard-1's leader is `1b`; wait for `LeadersColocated`; `make ops-agent`; expect
   `transfer_leader`; `make ops-approve ID=p-…`; kvscale's log shows `executed`.
2. **A proposal refused by a guard**: generate load (`KV_API_KEY=$(cat secrets/ops.key) scripts/local-load.sh 64 300`) until
   `ScaleOutDue` fires, `make ops-agent` (expect `scale_up`, blocker `off`), then stop one
   replica (`docker compose stop r1c`) before approving: `make ops-approve` or kvscale
   refuses it because a replica is down.
3. **The evaluation set**: `make ops-eval` prints one line per situation and the pass rate.

## Transcript

_(to be filled by the author: the proposals' summaries, the approval prompt, kvscale's
`executed` / `refused` lines, the evaluation output)_

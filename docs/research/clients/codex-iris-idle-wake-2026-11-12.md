# Real Iris idle wake on Codex 0.167.2

2026-11-12, Linux under WSL 2. Both an isolated default Codex home and an explicit
`CODEX_HOME` completed the real Iris incoming-message-to-idle-turn chain. This measures
[ADR 017](../../adr/017-wake-codex-by-queue.md) with the actual implementation, following the
[earlier stand-in MCP measurement](codex-mcp-queue-2026-11-10.md).

Implementation tested: `34abeff6333bf4f9db819f5718ea363585114ff5`, based on main
`b2164d458e495165e755bf586fda8767e17a9ed0`, in PR 292. The candidate was built with
`CGO_ENABLED=0`, source clean, and binary SHA-256
`efaec3fe7f67ed9b4c73a9341d93ad9c40fab5f1c82bbadce8c2d97bde574506`.
The host reported `Linux 6.6.87.2-microsoft-standard-WSL2 x86_64`. The harness required the
installed CLI to answer exactly `codex-cli 0.167.2` in each isolated environment.

## Setup and boundaries

The native harness reused the owner's earlier local Responses SSE and public hook-trust
protocol. It ran the actual candidate's `serve`, integration, hooks and MCP process. There was
no stand-in MCP server or proxy queueing the observed wake.

Each case had a fresh short home under `/home/klattm/iw3-97554696`, its own repository with an
`example.invalid` origin, loopback Iris hub, generated fixture token, database and loopback
scripted Responses provider. The default case omitted `CODEX_HOME`; the custom case set it.
Child environments were built from scratch, inheriting only `USER`, and did not read the user's
Codex configuration or authentication. Closed HTTP proxies were a routing precaution, not
OS-enforced network isolation. No user service or scheduler was installed.

`iris integrate codex --wake` wrote the real candidate integration. An additional independent
Go hook recorded payloads without changing Iris's hooks. A stdio Codex app-server supplied the
hooks' current hashes through `hooks/list`; `config/batchWrite` recorded those hashes in the
scratch configuration, and another listing verified trust. No trust-bypass flag was used.
This is programmatic trust of the controlled fixture; a person's TUI review was not measured.

The local provider used the model label `gpt-6-luna`, with `requires_openai_auth=false`. Its
responses were scripted and did not contact a hosted model. Warm-up Code Mode called the real
Iris `whoami`; wake Code Mode called `inbox` and `claims`, then completed. Message bodies did
not choose code or commands. Provider token usage fields were synthetic and are not cost data.

## Sequence and result

For each case, the harness started its own daemon, seeded a saved thread with `codex exec`,
and queued one warm-up prompt. It required the warm-up's real `whoami` result to agree with
the hook-registered binding, a completed rollout turn and Stop hook, root/stream observations,
then at least three seconds with no additional turn. The wake journal had to contain no
attempt before the incoming message.

A separate fixture Agent and Execution sent exactly one message through the scratch hub.
The harness did not queue the observed wake. Iris's own MCP child did so. Acceptance required
the reservation to start after the send, a completed queue turn of the same UUID, the exact
persisted prompt in that turn's UserPromptSubmit payload, the delivery id in that turn's inbox
result, and consumption no earlier than reservation. The provider request also had to name
`request_kind=turn`, `turn_trigger=queue` and the same session UUID.

Both cases passed all of those assertions:

| Evidence | Default home | Explicit `CODEX_HOME` |
|---|---|---|
| Thread | `01a1cf32-21e4-7b41-9a88-83a11fa0f1d7` | `01a1cf32-cc25-7cf2-9eb1-991d817191ce` |
| Warm-up turn | `01a1cf32-2315-7922-a1b1-7c1cc9efe301` | `01a1cf32-cd4d-7a72-a974-15a25ac37f76` |
| Wake turn | `01a1cf32-4e2f-7a70-94ca-74da658db157` | `01a1cf32-f864-7af3-a350-d8fe68fe3463` |
| Delivery | `dl_8bc70ef91008b8ab61f77a47a1ab46a9` | `dl_5e6a39699ad24ae1d863729fe44db48b` |
| Warm-up completion observed, UTC | `09:58:52.132` | `09:59:35.710` |
| Incoming send, UTC | `09:58:55.977` | `09:59:39.551` |
| Wake completion observed, UTC | `09:59:03.113` | `09:59:46.685` |
| No-extra-wake observation ended, UTC | `09:59:33.211` | `10:00:16.689` |

The inbox receipt cleared attention to `identity=null`; the sender's status still reported
the delivery as `inflight`. The script sent no ACK or handled action. For thirty more seconds
there were exactly three turns including seed and warm-up, and exactly one wake attempt.
Both cleanup results reported successful daemon stop, an empty descendant scan and no remaining
sockets among the discovered home-linked targets, with no cleanup errors. This harness used
PID snapshots rather than pinned process handles, and collected socket targets at cleanup;
that is narrower than proving stable process ownership or a complete live socket inventory.
Newly created files were preserved. A later per-key inspection of both trust logs confirmed
all ten expected keys retained their current hashes and returned `trustStatus=trusted`.

The roughly seven-second send-to-completion observations are single-run observations, not a
latency guarantee. The scheduler also had an earlier connection candidate: later hints do not
postpone it, so the ten-second settle rule is not a minimum delay after every incoming message.
These runs therefore do not isolate `inbox_changed` handling from the initial connection's
already scheduled authoritative read. That route has separate unit and real-hub coverage;
an additional native measurement must send after initial arming activity has settled to prove
the socket-event trigger independently.

## Preserved evidence

The operational harness source is outside the repository at
`/home/klattm/.local/state/iris/validation/adr017-linux-native-34abeff/harness/`:

- `main.go` SHA-256: `f7c3071fb3e3ef6528399c8de2f02a65fc86d1a5243e3d92034d64727535a3cd`.
- `native-acceptance` executable SHA-256: `233e7281e8c3702202ec7741bb545fdf183c3bed702faa7856f8af2739403230`.

The run is preserved at
`/home/klattm/.local/state/iris/validation/adr017-linux-native-34abeff/runs/run-01/`:

- `results.json` SHA-256: `80b7a39c32545c3de196081844b80dff84a439b18fa02a9104b338deb63b994b`.
- `manifest.json` identifies the binary, scratch paths and isolation limits.
- Per-case command, hook-trust, hook-payload, provider and hub-API logs join the evidence above.
- The scratch homes retain their actual rollouts, Iris observations and wake journals.

The same implementation passed all six local Linux gates: `make test`, `make race`, `make cov`,
`make cov-check`, `make check-all` and `make fixtures`. Strict coverage was 11669/11669 statements.
Those logs and state are at
`/home/klattm/.local/state/iris/validation/codex-wake-34abeff-20261112T093113Z/`;
the profile SHA-256 is `927b534e80474e4d249b5ab505a36a563cb213b2da839dc1e37ba9f037e1bf66`.
Normal Go cache reuse of unchanged packages is recorded separately from the preceding failed
whole `5557d265` race run, whose new test fixture was corrected before this candidate.

## Linux observer preflight failure

A second harness revision added an external HTTP/WebSocket observer and stricter process and
socket ownership checks. Its `run-02` stopped both cases during setup, before seed, warm-up or
an incoming message: immediately after daemon start, the advertised control-socket link did
not yet exist. The initial inventory returned before examining the updater link, and cleanup
then refused removal from an incomplete inventory. The original results retain FAIL and the
cleanup errors; this was a harness readiness assumption, not an observed Iris wake failure.

All managed commands were joined and the descendant scans were empty. A separate recorded
cleanup verified each surviving fixture updater link, its exact path-derived target and lock,
UID, type, device/inode identity, refused connection and unlocked lock before removing those
six specific paths. The scratch data remain intact; the run evidence is at `runs/run-02/`,
including `post-failure-owned-socket-cleanup.json`. A later revision must collect both links,
preserve partial positive identities, and require a complete live inventory after warm-up
before sending its message. This failed preparation supplies no socket-trigger acceptance.

## Linux socket-event acceptance

The corrected external harness completed both cases in `runs/run-03/`, without changing the
Iris candidate. The run finished with exit 0 at 11:01:22 UTC. It retained initial partial
socket inventories, then required both verified live links after warm-up. Before sending,
it required 25 seconds of stable idle/root/stream observations, no wake attempt and a completed
empty attention response from the real runner. Harness API reads went directly to the backend;
only the normal integration's traffic traversed the observer.

Both cases then recorded one `inbox_changed` frame on the existing WebSocket, followed by a
completed pending attention response after the settle interval. Its identity stream/cursor
matched the frame and the wake reservation; its outer and attention high-water values agreed.
There was no reconnect or reset during the measured interval. Independent downstream evidence
joined the reservation to the same UUID's exact prompt, inbox delivery and consumed mark.
The delivery remained inflight without ACK/handled, attention cleared and thirty further
seconds produced no extra wake. Per-key trust checks covered all ten fixture hook keys.
An independent read-only review of the actual local artifacts confirmed both cases, including
the stream/turn joins, per-key trust, result hashes and absence of the captured socket paths.

| Evidence | Default home | Explicit `CODEX_HOME` |
|---|---|---|
| Thread | `01a1cf69-4391-7ae2-974b-b9700edea004` | `01a1cf6a-514c-7b62-9b71-021876d1088d` |
| Wake turn | `01a1cf69-d2a9-7271-8bc3-b7f0e24ffc6b` | `01a1cf6a-df74-7911-b545-ccb08e93f80f` |
| Delivery | `dl_61e9d122e393fb2c1f5a32cbdf8bb196` | `dl_d015260920ddc56f8ad88e73f46aa915` |
| Empty response before send | 15067 ms | 15058 ms |
| Frame to pending read start | 10002 ms | 10001 ms |
| Frame to consumption | 10301 ms | 10061 ms |
| Attention identity cursor / high-water | 8 / 9 | 8 / 9 |

Both cases reported a complete live socket inventory, no missing links, successful daemon
stop, complete final observer JSONL and no remaining owned descendants, captured socket objects
or cleanup errors. Linux pidfds pinned identities for discovered descendants; cleanup joined
managed commands before reaping. Socket removal checked the previously captured path, UID,
type and device/inode identity. This verifies the fixture's owned objects, not absence of every
unrelated process/socket on the host. Direct child commands also use Go's bounded `WaitDelay`,
whose fallback may call `Process.Kill`; discovered descendants have no numeric-PID signal fallback.

The frozen harness source and executable are in `harness-v3/`, alongside `SHA256SUMS` and
`BUILD.txt`. The observer files are `observer-v2-src/main.go` and `observer-v2-src/attention-proxy`:

- `main.go` SHA-256: `f47530ff845b4be6e460a8a6d8d3fa26f65858cf3230c9ebbd369fca2d86256d`.
- Harness executable SHA-256: `505821ff28a3f2fad2f6a20d912ca43f27ec3199d017257a788cc2e2681d8260`.
- Observer source SHA-256: `3d49bf9a000435cbb48e0dd3ae798e3b075e1e059be2d72faa82435df8da1d40`.
- Observer executable SHA-256: `30ee6b1a5c4a107ac15db25fe2b02756df55679436f1b79d3f075af05bc6e186`.

The results SHA-256 is `707822518041512c018012669f718154521dbef93925f6bf3257df7e1c4326a1`.
The default/custom observer log hashes are
`ecd79f43ff06a01543c2b8de2c412c093ee2007fdbad7f58584e6ba05fc351c9` and
`708fcf42677060a64cb7a3f8b0a8e1bcb4fb4ab978f9b066187849f0aa6fe655`.
The compact `coordinator-evidence.json` records the joins without prompts or tokens, SHA-256
`d40a8834f241de791a51b36a85183cad19b48e9e1bfeba0bb3e35cda0af57055`.
All these paths are relative to the operational root named above. The scratch homes are
under `/home/klattm/iw3-1681796353` and remain preserved.

The observer is a lossless external measurement tool, not Iris application code. Its timestamps
describe upstream frame/read completion before forwarding, not client receipt. Synchronous
logging can affect latency. Its Linux derivative corrects the `consumer_id` audit field named
below. The scripted provider and loopback-routing limits of the first run still apply; this
run proves the isolated socket trigger for daemon-loaded threads, not a native TUI interaction.

## macOS native TUI witness reported by its owner

The native macOS owner, `codex-laptop-iris`, reported two completed cases on the same
implementation and Codex 0.167.2, with an ordinary TUI and the default shared daemon. Both
used a scripted local provider. The candidate binary SHA-256 was
`6fc4e5990151a7df857dddd0366bec45d3cd4f1ebe826264e69e0ef2eef7d584`.
An independent read-only artifact review on that host accepted both cases. The WSL owner
received compact result data and hashes, rather than running macOS or reading its raw files.

Unlike the first Linux run above, these cases isolated the socket trigger: a lossless external
observer recorded the runner's completed empty attention response before the incoming send,
fourteen seconds of stable quiet, the actual `inbox_changed` frame on the same connection,
then a pending attention read matching the frame's stream and cursor. The queued turn had the
same UUID and exact prompt, fetched the delivery, consumed the reservation and stayed quiet
for thirty seconds. Twelve fixture hook keys, including the normal Iris hooks, had their
per-key trust checked. Delivery remained inflight without an ACK or handled action. Both
daemon and proxy exited normally, with zero reported PID/socket/cleanup errors.

| Evidence | Default home | Explicit `CODEX_HOME` |
|---|---|---|
| Thread | `01a1cf43-3ebb-7ff0-9ce6-51a5d90215a1` | `01a1cf44-25b5-7bb1-9e84-eb1cd1425527` |
| Wake turn | `01a1cf43-cd5b-71f2-98cb-deb19db39093` | `01a1cf44-b42c-7843-b015-47323c7d415e` |
| Delivery | `dl_71ebe6328440dfd735c5edf44aef45c2` | `dl_f5efbc912528a20cd75045cc16f7ec1b` |
| Empty response before send | 14319 ms | 14263 ms |
| Frame to pending read | 10001 ms | 10003 ms |
| Frame to consumption | 10188 ms | 10174 ms |
| Attention identity cursor / high-water | 4 / 5 | 4 / 5 |

The owner preserved `native-acceptance-summary.json`, SHA-256
`9ac17e9578349bae86a140677db58fa50d38ddf77b051884b13529cbebde35ab`, under
`/Users/klattm/.local/state/iris/validation/adr017-native-macos-34abeff-20261112T094824Z/`.
The default report/audit hashes were
`a510c920d9a8896ed8cf072717dc915670c533befe5d2fcbd1276402ea0fe8cd` and
`c0e96a8702340124d3ef8f4b0669aeebcf0564b6cc8e5948668ab62250782c76`;
the custom report/audit hashes were
`f11228e72a05968772ef4de35a803bc30fce8c6afd7a2d0fe95193c41d36ff9a` and
`7e59a26bdb5dda8320d0c750fc41abfa5967dcb481e38d1916496aec75be571a`.

The observer source SHA-256 was `48d4a2b843c8b9c87240e9b313db4d5cee6d247544e7887ab0868c54d4102035`.
Its EOF and frame timestamps describe upstream observation before forwarding, not direct
client receipt or a runner decision. Independent downstream prompt/inbox evidence supplies
that join. Its audit misspells `consumer_id` as `consumer`, so the audit alone does not prove
the absence of `consumer_id` in requests. Synchronous logging can affect timings, and routing
through a loopback observer is not OS network containment. These qualifications do not turn the local scripted
provider into hosted-model or cost evidence.

## What remains unmeasured

The first Linux measurement has its initial-connection qualification; the later run isolates
the socket trigger in both homes. The separate owner-reported macOS measurement adds native
TUI/socket-event evidence. These do not prove hosted-model response, short/long-thread cost,
Windows, closed/unloaded-session wake, reboot recovery, power-loss durability or long-term
reliability. The default remains off. Windows native acceptance is pending; an unavailable
owner seat and passing Windows CI do not supply that delivery proof.

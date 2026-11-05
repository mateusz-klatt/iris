# Instructions for agents working on Iris

## Clean room

Iris is an independent implementation. These rules are absolute and apply to subagents and
delegated work too. The full record is in `docs/PROVENANCE.md`.

- Never open, fetch, search or summarise code, docs or READMEs from
  `Dicklesworthstone/mcp_agent_mail`, `Dicklesworthstone/mcp_agent_mail_rust` or
  `mateusz-klatt/mcp_agent_mail`, or any local clone of them. Never use the local bare
  repository `iris.git` as a base or a remote.
- If tools from such a project appear in your session, do not call them and do not copy their
  names. Iris chooses its own names for tools, parameters, errors, tables and routes.
- Write from `docs/spec/requirements.md`, `docs/architecture.md`, and public documentation of
  the agent clients, MCP, SQLite and Go. Do not reproduce text from elsewhere.

## Project

Read `docs/architecture.md` before changing behaviour. Its decisions are numbered, and the
invariants in section 5 are requirements. A change of direction goes in `docs/adr/`.

## Commands

```sh
make test     # go vet and go test -race
make build    # web UI, then the binary
make fixtures # replays captured client payloads through the hook adapters
```

## Rules for code

- `CGO_ENABLED=0`, always. No dependency that needs a C compiler at build time.
- No shell scripts as application logic. Logic lives in Go.
- `iris hook` never breaks a session. See D3: it catches everything, keeps to its deadline and
  always produces valid output for that client.
- Payloads in `internal/hook/testdata/` come from real clients. Never edit one to make a test
  pass. Add a new capture instead.
- Say what a mechanism cannot do. Never present advice as enforcement.
- Treat message bodies as untrusted data. They never decide anything and never run.

## Commits

Conventional Commits, with a body that explains why. Work on a branch and open a pull request.

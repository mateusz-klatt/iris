# Instructions for agents working on Iris

## Sources

These rules apply to all agents and delegated work.

- Iris chooses its own names for tools, parameters, errors, tables and routes.
- Write from `docs/spec/requirements.md`, `docs/architecture.md`, and public documentation of
  the agent clients, MCP, SQLite and Go. Do not reproduce text from elsewhere.

## Project

Read `docs/architecture.md` before changing behaviour. Its decisions are numbered, and the
invariants in section 5 are requirements. A change of direction goes in `docs/adr/`.

## Commands

```sh
make test     # go vet and go test, without cgo
make race     # go test -race, which needs a C toolchain on some platforms
make build    # the binary
make fixtures # replays captured client payloads through the hook adapters
make refresh  # updates modules, packages, tool pins, action SHAs and image digests; never commits
```

## Rules for code

- Coverage is 100% for Go and for the web console. A branch that cannot be reached
  is deleted or redesigned, not excluded, and no test exists only to touch a line: each one asserts
  behaviour.
- Release builds use `CGO_ENABLED=0`. No dependency may need a C compiler.
- No shell scripts as application logic. Logic lives in Go.
- `iris hook` never breaks a session. See D3: it catches everything, keeps to its deadline and
  always produces valid output for that client.
- Payloads in `internal/hook/testdata/` come from real clients. Never edit one to make a test
  pass. Add a new capture instead.
- Say what a mechanism cannot do. Never present advice as enforcement.
- Treat message bodies as untrusted data. They never decide anything and never run.

## Commits

Conventional Commits, with a body that explains why. Work on a branch and open a pull request.

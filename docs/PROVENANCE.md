# Provenance

Iris is an independent implementation. This file records how that independence is kept, so
that anyone can check it later.

## Why this file exists

The project owner previously maintained a fork of `mcp_agent_mail`, and there is a separate
Rust project in the same space, `mcp_agent_mail_rust`. Both carry a licence rider that makes
their code unusable here. Changing the programming language does not change that:
translating a program is still making a derivative of it. What is free to everyone are
ideas, functionality and interfaces. What is protected are the code and the text that
express them. Iris therefore rests on process, not on the choice of Go.

This is an engineering record, not legal advice. A review by someone qualified in
intellectual property is planned before the first public release.

## Rules

1. Nobody working on Iris, human or agent, reads, fetches, searches or summarises the
   backend code, documentation or READMEs of the following, including for reference:
   - `github.com/Dicklesworthstone/mcp_agent_mail`
   - `github.com/Dicklesworthstone/mcp_agent_mail_rust`
   - `github.com/mateusz-klatt/mcp_agent_mail`, except as listed under "Imported material"
   - local clones of those repositories, and the local bare repository `iris.git`
2. Iris is written from `docs/spec/requirements.md`, from the ADRs in `docs/adr/`, and from
   public documentation of the agent clients, MCP, SQLite and Go.
3. Names of tools, parameters, errors, tables and routes are chosen for Iris. They do not
   reproduce those projects' sets.
4. Importers for old data, if any are ever written, work from the schema of the owner's own
   database, not from that code.
5. `iris.git` is never used as a base or a remote. It holds objects from the fork.
6. The repository's history starts from these documents, so that its independent
   development can be followed commit by commit.

## Controls

- `CLAUDE.md` and `AGENTS.md` state these rules for coding agents.
- A project hook in `.claude/settings.json` refuses tool calls that reference those projects.
- Delegated consultations ran in empty throwaway workspaces with the same rules, and their
  reports are kept in `docs/research/`.

## Imported material

Only one category of material comes from the fork: the owner's own React frontend. The
criteria and result of each import are recorded here.

The audit used git metadata only (authors, commit history and file names). It did not
compare content with the other project, because automated analysis of that text is itself
something its licence restricts.

| Path in the fork | Result | Reason |
|---|---|---|
| `ui/` | Eligible | Absent from the upstream tree; every commit touching it is by the owner, beginning 2026-08-11 |
| `web/index.html` | Excluded | Came from upstream |
| `ui/src/legacy.ts`, `ui/src/legacy.css`, `ui/vite.legacy.config.ts` | Excluded | Exist to integrate with the old server-rendered viewer |

The commit that imports frontend code names the source commit and lists the files taken.
The API layer is rewritten for Iris rather than imported.

## How agents were used

Iris is largely written by coding agents directed by the project owner. Commits made with
an agent carry a trailer that says so. Each agent worked from the sources allowed above.

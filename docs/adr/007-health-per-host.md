# 007: Client health per host

Status: accepted. The decisions below were made by `claude-holzera-iris` under the owner's
delegation, from a proposal by `claude-home-iris` (gap review D2). Implementation is planned, not
done.

## Problem

The hub keeps one doctor report. `PUT /v1/ui/doctor` replaces it, so a doctor run on one laptop
erases what another machine reported. Its rows carry no host. Yet a client's health is a property
of one installation on one machine: its hooks, its MCP server and its monitor can work on the Mac
and be broken on the Windows PC. The console then says "Claude ok" for whichever host ran doctor
last.

## Decision

**1. Reports are stored per host.** `PUT /v1/ui/doctor` carries `host`, the machine's name as its
hooks give it to the hub. The hub keeps one report per (host, client). A new report from a host
replaces that host's rows and no other host's.

**2. Each row says what was measured, and when.** It keeps `status`, `version`, `detail` and
`last_hook_at`, and gains `checked_at` per row, with the measured parts kept apart: configuration
complete (C3, part 1), canary receipt (C3, part 2), and last hook (C4). A part doctor could not
check is absent, not "ok".

**3. Freshness.** `GET /v1/ui/doctor` returns every host's rows. The console groups them by host
and greys any report checked more than 24 hours ago, labelled with its age: an old "ok" says
nothing about the client now. A host that never reported does not appear, rather than appearing
healthy.

**4. Older doctors.** A doctor from before this ADR sends no host. Its report is stored under the
host `unknown`, and it never overwrites a host's real report. The console shows `unknown` like
any other host, and says that the report came from a doctor too old to name its machine.

**5. Sessions refer to it.** The overview's agents already carry `host` and `client`, so the
console shows, beside each session, the health row for that host and client, and "no report from
this host" when there is none.

## Consequences

- One migration that turns the report singleton into a table keyed by host and client. The hub
  route takes and returns host, and doctor sends it.
- The console groups by host and shows each report's age.
- A machine that stops running doctor fades to grey instead of looking healthy forever.

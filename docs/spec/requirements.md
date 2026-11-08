# Iris requirements

Status: accepted as the starting point, 2026-11-05. This document states what Iris must do
and why. How Iris does it lives in `docs/adr/`.

## The problem

A developer can now run many coding agents at once: several terminals, several vendors,
several machines, several git worktrees of one repository. Those agents do not know about
each other. Two of them edit the same file and one loses its work. One finishes a task
that another depends on, and nothing tells the second. A human who wants to know what is
happening has to read each terminal in turn.

Iris gives those sessions a shared place to see each other, talk to each other and agree
on who is working on what, and gives the human one screen for all of it.

## Who it is for

One developer or a small team, with up to about 50 concurrent sessions across up to about
5 machines. Iris is not built for hundreds of users or for multi-tenant hosting.

## Requirements

### R1. Works across vendors

Iris supports Claude Code, Codex CLI, Google Antigravity CLI, Kimi Code, Grok CLI, Cursor
Agent and GitHub Copilot CLI. These clients expose different capabilities. Iris uses each
one's strongest available mechanism and never pretends that a weaker client has a guarantee
it does not have.

### R2. Works across platforms

Windows (native), WSL2, macOS and Linux on amd64 and arm64, and containers for the server.
A single deployment can mix them. The same repository reached through different paths on
different systems must be recognised as the same repository.

### R3. Installs seamlessly from trusted sources

Installing means a package manager the user already trusts, followed by one command that
wires Iris into the clients found on the machine. No Node, Python or shell scripts at
runtime. Application logic lives in one native binary. Removal is as easy as installation
and leaves client configuration as it was found.

### R4. Durable communication

An agent can send a message to another agent or a group. Messages survive restarts of the
server and of the recipient. The recipient acknowledges what it has handled, so the sender
can tell delivered from acted upon. A delivery may be repeated after a failure, but it is
never silently lost and never processed twice as two different messages.

### R5. Collision avoidance

An agent can claim files or other resources before working on them. A claim is decided
atomically by the server. When a session tries to edit something held by another session,
it finds out before the edit wherever the client allows that, and at commit time at the
latest. A claim held by a dead session must not block others for long.

### R6. Immediate attention

When something needs an agent's attention, it finds out as quickly as its client allows:
at once where a client can be woken from outside, otherwise at its next turn. A human gets
a signal when an agent cannot be reached.

### R7. Human oversight

A web UI shows sessions, messages and claims live, and lets a human read, reply and step
in. It ships inside the binary.

### R8. Never breaks a session

A coordination failure must not stop an agent from working. If the server is down, a
client is too new, or a hook crashes, agents continue and the loss of protection is
visible. Blocking on failure is an explicit opt-in.

### R9. Honest about its limits

Iris coordinates cooperating agents. It says plainly what it cannot see, such as a file
changed through a shell command, and what it cannot enforce. It never presents advice as
enforcement.

### R10. Treats agent input as untrusted

Text written by one agent can reach another agent's model context. It is handled as data
from a named sender, bounded in size, and never as instructions from Iris or from the user.

### R11. Independent implementation

Iris is written from these requirements and the architecture decisions in `docs/adr/`.

## Non-goals

- Email. Messages are records in Iris's own store, and there is no SMTP or IMAP.
- Running or scheduling agents. Iris coordinates sessions that people start themselves.
- A security boundary between agents under one operating system user.
- A distributed database. There is one server per deployment.
- Choosing or routing models.

## First milestone

The first milestone proves the riskiest assumptions before integrations multiply:

1. Two sessions, on Linux and on Windows, register and see each other.
2. One sends a message, the other receives it and acknowledges it, and the sender sees the
   acknowledgement.
3. One claims a file, and the other is refused before it edits that file.
4. A connection drops during a burst of events, and the client recovers every event in
   order with no duplicates applied.
5. Hook latency is measured on Linux, macOS and Windows.

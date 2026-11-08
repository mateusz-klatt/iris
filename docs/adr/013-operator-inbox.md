# 013: Put the operator's conversations first

Status: proposed.

## Problem

On a populated hub, client reports, repository aliases and inactive agents push
the operator's messages far down a single page. Opening a thread renders its
contents below the list, outside the viewport. R7's read, reply and intervention
workflow therefore requires finding the same controls repeatedly.

## Decision

The populated console opens on **My messages**. Its navigation separates that
mailbox from all fleet conversations, agents, claims and client diagnostics. A
new-message action is always reachable. Unread operator mail, active agents and
claims have counts and shortcuts; delivery problems remain visible above mail
as an expandable summary. On an empty hub, D16's client reports and live wait
remain the initial view.

`agent=human` on the existing operator thread endpoint selects threads containing
messages sent or received by any human mailbox. Repository and search filters
still apply. Human mailboxes preserved by a repository repair remain included
by their client type, without depending on their ids. Other agent names and ids
retain their exact-match semantics. Listing does not read or acknowledge mail.

The desktop mailbox pairs a scrollable thread list with its reader. On phones,
the reader replaces the list until Back. Rows show the subject, participants,
timestamp, message count and unread count, with an explicit selected state.
Opening a conversation focuses its heading; returning focuses the originating
row after the list is visible. Retrying a failed load restores focus to the
reader heading. A live refresh retains an edited reply and does not continually
move focus. Only opening unread operator mail invokes the existing read action.

The fleet initially shows active sessions, with a search over their context and
an option to include inactive records. Repository aliases are expandable.
Navigation hides the compose view without unmounting it, preserving drafts and
their existing review and idempotency rules. Recipient selection still uses ids.

## Limits and validation

View selection and filters remain local to the page; reload and browser history
do not restore them. Drafts do not survive reload. Counts reflect the latest hub
snapshot, and claims keep their existing advisory meaning. Client diagnostics
still report each host's last doctor run and cannot run doctor remotely.

The operator filter is tested across repositories, search, sent and received
mail, same-timestamp pagination and database failures. Navigation tests exercise
keyboard focus and draft retention. Browser checks cover WebKit and Chromium at
390, 430 and 1440 pixels, including long delivery and thread rows.

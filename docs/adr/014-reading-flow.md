# 014: Keep the operator's place and unfinished replies

Status: proposed.

## Problem

The operator console introduced in ADR 013 puts conversations first, but its
stacked summary and filters leave the first conversation partly below a phone's
initial viewport. Opening a conversation also retains that navigation above the
reader. Long messages leave the Back action far from the reply form.

The console holds a single reply draft. Reading another conversation replaces
unfinished text. A send completing after the operator edits a draft can also
clear text that was not part of that send.

## Decision

Keep the existing navigation and delivery summary, with compact counters and
spacing. On a phone, agent and reservation counts remain in navigation instead
of appearing a second time above the inbox. Opening a conversation hides the
mailbox heading, counters, delivery summary and filters until Back. Only the Back
action stays visible while scrolling; even a very long subject scrolls away so
it cannot cover the reply editor. Back restores the
originating list or delivery row and its keyboard focus.

Keep reply drafts in page memory, keyed by repository filter and thread. A draft
retains its recipients, subject, body and complete send retry state. Switching
conversations or repository filters does not discard it. A live refresh may
update an untouched suggested reply, but cannot replace edited text. An empty
conversation has no reply form and cannot inherit another thread's recipients.

A successful send clears only the submitted draft with the same retry key. New
text typed while a send is pending remains, including in the new-message form.
Leaving a conversation closes its transient send review. Returning retains the
draft and retry identity, focuses the conversation heading, and requires review
again before sending. The existing one-copy retry window continues to apply.

## Limits and validation

Drafts still do not survive a page reload or closing the browser. This adds no
server-side draft storage, acknowledgement state or inference that reading means
the operator has acted. Filters and browser history retain ADR 013's limits.

Behaviour tests cover switching conversations and repository scopes, empty
threads, ambiguous retries, and edits during a pending send. Browser checks
cover an initially visible conversation, long-message reading and returning to
the same row on phones and desktop, in Chromium and WebKit.

import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  ApiError,
  getDoctor,
  getOverview,
  getThread,
  readThread,
  listThreads,
  revokeClaim,
  sendMessage,
  watchUi,
  type Agent,
  type Claim,
  type DoctorClient,
  type DoctorReport,
  type DoctorReports,
  type DoctorStatus,
  type Overview,
  type ThreadDetail,
  type ThreadMessage,
  type ThreadSummary,
  type WatchFrame,
  type WatchStatus,
} from "./api";
import { DeliveryProblems } from "./DeliveryProblems";
import { ClaimsPanel } from "./ClaimsPanel";
import { DraftBody, DraftReview } from "./DraftReview";
import { RecipientPicker } from "./RecipientPicker";
import { useReview } from "./review";
import { loadLocale, type SupportedLocale } from "./i18n";
import LocalePicker from "./LocalePicker";
import { MessageBody } from "./MessageBody";
import {
  playNotificationTone,
  setSoundEnabled,
  soundEnabled,
} from "./notificationSound";
import { clearUiLocale, isSupportedLocale } from "./preferences";
import { clearAccountSession, ConsoleClosedError, getSession, logout, updateProfile, type AccountSession } from "./accountApi";
import Login from "./Login";
import Account from "./Account";
import AccessPanel from "./access";
import "./app.css";
import "./console.css";

type ConsoleView = "messages" | "fleet" | "claims" | "diagnostics" | "compose" | "account" | "access";

// The hub limits a message by its UTF-8 bytes, not by characters: a draft of 16 KiB of
// characters outside ASCII can be several times that (gap review A11).
const maximumBodyBytes = 16 * 1024;
const maximumSubjectBytes = 1024;
const encoder = new TextEncoder();

function byteLength(text: string): number {
  return encoder.encode(text).length;
}

// SubjectSize says a subject is too long, which is otherwise only a disabled Send button.
function SubjectSize({ subject }: { subject: string }) {
  const { t } = useTranslation();
  const size = byteLength(subject);
  return size > maximumSubjectBytes ? <p role="alert">{t("markdown.byteCount", { count: size, maximum: maximumSubjectBytes })}</p> : null;
}

// sendable is whether the hub can take the draft as it is.
function sendable(draft: Draft): boolean {
  const body = byteLength(draft.body);
  return draft.to.length > 0 && draft.subject.length > 0 && body > 0 && body <= maximumBodyBytes && byteLength(draft.subject) <= maximumSubjectBytes;
}

// A draft carries the retry key of its send. The key is made with the draft and kept until the
// send succeeds, so retrying after an ambiguous failure cannot send twice (gap review A9). An
// edit is a different send, so it gets a new key: the hub refuses a key reused for other content.
interface Draft {
  to: string;
  subject: string;
  body: string;
  replyTo: string;
  key: string;
  /** The repository the key was first sent from. A send from another view is another send. */
  sentFrom?: string;
  /** When this key was first sent, in milliseconds. The hub's one-copy promise runs from then. */
  firstTried?: number;
  /**
   * Whether the console made this reply draft and the operator has not touched it, so a refresh
   * may replace it. An edit makes it the operator's, and a live refresh never discards it (A14).
   */
  offered?: boolean;
}

function newDraft(fields: Partial<Draft> = {}): Draft {
  return { to: "", subject: "", body: "", replyTo: "", ...fields, key: humanKey() };
}

// The console bounds an uncertain retry more tightly than the hub's retained key.
const accountRetryWindow = 10 * 60 * 1000;

// operatorName is the sender name the hub gives the console's own messages.
const operatorName = "human";
const noAgents: Agent[] = [];

// replyDraft starts a reply to a thread. It answers the other party: the latest sender who is not
// the operator, or, in a thread only the operator has written to, whom they wrote to. Answering
// the latest sender would address a follow-up after the operator's own message back to them.
function replyDraft(messages: ThreadMessage[]): Draft | null {
  const last = messages.at(-1);
  if (last === undefined) {
    return null;
  }
  const other = [...messages].reverse().find((message) => message.sender !== operatorName);
  const to =
    other?.sender ??
    last.recipients
      .map((recipient) => recipient.name)
      .filter((name) => name !== operatorName)
      .join(", ");
  return newDraft({ to, subject: last.subject.startsWith("Re:") ? last.subject : `Re: ${last.subject}`, replyTo: last.id, offered: true });
}

// unreadForOperator is whether a thread holds mail to the operator not yet acknowledged.
function unreadForOperator(detail: ThreadDetail): boolean {
  return detail.messages.some((message) =>
    message.recipients.some((recipient) => recipient.name === operatorName && recipient.state !== "acked" && recipient.state !== "handled"),
  );
}

function edited(draft: Draft, change: Partial<Draft>): Draft {
  return { ...draft, ...change, key: humanKey(), firstTried: undefined, offered: false };
}

// threadFilters names the filters a thread list was opened under.
function threadFilters(repositoryId: string, agent: string, query: string): string {
  return JSON.stringify([repositoryId, agent, query]);
}

function isAbort(error: unknown): boolean {
  return error instanceof Error && error.name === "AbortError";
}

function formatWhen(value: string): string {
  if (value.length === 0) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(date);
}

function repositoryName(overview: Overview, id: string): string {
  return overview.repositories.find((repository) => repository.id === id)?.name ?? id;
}

function statusKey(status: DoctorStatus): `operator.${"statusOk" | "statusNotConfigured" | "statusNeedsHuman" | "statusSilent" | "statusSlow"}` {
  switch (status) {
    case "ok":
      return "operator.statusOk";
    case "not_configured":
      return "operator.statusNotConfigured";
    case "needs_human":
      return "operator.statusNeedsHuman";
    case "silent":
      return "operator.statusSilent";
    case "slow":
      return "operator.statusSlow";
  }
}

function recipientsOf(value: string): string[] {
  return value
    .split(",")
    .map((item) => item.trim())
    .filter((item) => item.length > 0);
}

function humanKey(): string {
  return `human:${crypto.randomUUID()}`;
}

// healthOf is what the session's own host last reported for its client (ADR 007): the evidence
// for this session, not for another machine's installation of the same client.
function healthOf(doctor: DoctorReports | null, agent: Agent): DoctorClient | undefined {
  return doctor?.hosts.find((report) => report.host === agent.host)?.clients.find((client) => client.client === agent.client);
}

// staleAfter is how old a report may be before it says little about the client now (ADR 007).
const staleAfter = 24 * 60 * 60 * 1000;

function HostReport({ report }: { report: DoctorReport }) {
  const { t } = useTranslation();
  // An age that cannot be read is no fresher than an old one.
  const stale = report.checked_at === null || !(Date.now() - Date.parse(report.checked_at) <= staleAfter);
  return (
    <section className="host-report" data-host={report.host} data-stale={stale ? "yes" : "no"} aria-label={report.host}>
      <h2>{report.host === "unknown" ? t("operator.doctorUnknownHost") : report.host}</h2>
      {report.checked_at !== null ? <p>{t("operator.checkedAt", { date: formatWhen(report.checked_at) })}</p> : null}
      {stale ? <p className="stale-note">{t("operator.doctorStale")}</p> : null}
      <ul className="message-list">
        {report.clients.map((client) => (
          <li key={client.client}>
            <article className="message-row" data-client={client.client} data-status={client.status}>
              <div>
                <strong>{client.client}</strong>
                <small>
                  {t("operator.version")}: {client.version ? client.version : t("operator.none")}
                </small>
              </div>
              <span className="status">{t(statusKey(client.status))}</span>
              <p>
                {client.status === "silent" || client.status === "slow" ? <span>{t("operator.cause")}: </span> : null}
                {client.detail}
              </p>
            </article>
          </li>
        ))}
      </ul>
    </section>
  );
}

export function App({ connect = watchUi }: { connect?: typeof watchUi } = {}) {
  const { t } = useTranslation();
  const [authentication, setAuthentication] = useState<"loading" | "closed" | "login" | "error">("loading");
  const [session, setSession] = useState<AccountSession | null>(null);
  const [attempt, setAttempt] = useState(0);
  const [passwordChanged, setPasswordChanged] = useState(false);
  const sessionGeneration = useRef(0);
  const signedOut = useCallback((changed = false) => {
    sessionGeneration.current++;
    clearAccountSession();
    setPasswordChanged(changed);
    clearUiLocale();
    delete document.body.dataset.humanUnread;
    void loadLocale("en");
    setSession(null);
    setAuthentication("login");
  }, []);
  useEffect(() => {
    const controller = new AbortController();
    void getSession(controller.signal).then(setSession).catch((error: unknown) => {
      if (controller.signal.aborted || isAbort(error)) return;
      if (error instanceof ConsoleClosedError) { clearAccountSession(); setAuthentication("closed"); }
      else if (error instanceof ApiError && error.status === 401) { clearAccountSession(); setAuthentication("login"); }
      else setAuthentication("error");
    });
    return () => controller.abort();
  }, [attempt, signedOut]);
  useEffect(() => {
    // Session-bound HTTP failures unmount the console here before a view's
    // asynchronous rejection handler can expose stale data or an error panel.
    const expired = () => signedOut();
    window.addEventListener("iris:unauthorized", expired);
    return () => window.removeEventListener("iris:unauthorized", expired);
  }, [signedOut]);
  useEffect(() => () => clearAccountSession(), []);
  const currentGeneration = sessionGeneration.current;
  const updateSession = useCallback((next: AccountSession) => {
    if (currentGeneration === sessionGeneration.current) {
      // A matching generation still has the mounted Console's account.
      setSession((current) => current!.account.id === next.account.id ? next : current);
    }
  }, [currentGeneration]);
  const endSession = useCallback((changed?: boolean) => {
    if (currentGeneration === sessionGeneration.current) signedOut(changed);
  }, [currentGeneration, signedOut]);
  if (session !== null) {
    return <Console key={session.account.id} connect={connect} session={session} onSession={updateSession} onSignedOut={endSession} />;
  }
  if (authentication === "login") return <main>{passwordChanged ? <p role="status">{t("account.passwordSaved")}</p> : null}<Login onLogin={setSession} /></main>;
  return <main className="content"><p role={authentication === "loading" ? "status" : "alert"}>{t(authentication === "error" ? "accountUi.sessionError" : authentication === "closed" ? "accountUi.consoleClosed" : "accountUi.sessionLoading")}</p>
    {authentication !== "loading" ? <button type="button" onClick={() => { setAuthentication("loading"); setAttempt((value) => value + 1); }}>{t("accountUi.retry")}</button> : null}</main>;
}

function Console({ connect, session, onSession, onSignedOut }: { connect: typeof watchUi; session: AccountSession; onSession: (session: AccountSession) => void; onSignedOut: (passwordChanged?: boolean) => void }) {
  const { t } = useTranslation();
  const member = session.account.role === "member";
  const [locale, setLocale] = useState<SupportedLocale>("en");
  const [soundOn, setSoundOn] = useState(false);
  const [phase, setPhase] = useState<"loading" | "ready" | "error">("loading");
  const [overview, setOverview] = useState<Overview | null>(null);
  const [doctor, setDoctor] = useState<DoctorReports | null>(null);
  const [revision, setRevision] = useState(0);
  const [threadRevision, setThreadRevision] = useState(0);
  const onUnauthorized = useCallback(() => onSignedOut(), [onSignedOut]);
  const [stream, setStream] = useState<WatchStatus>("connecting");
  const [repositoryId, setRepositoryId] = useState("");
  const [agent, setAgent] = useState("");
  const [view, setView] = useState<ConsoleView | null>(null);
  const [mailScope, setMailScope] = useState<"mine" | "all">("mine");
  const [showInactive, setShowInactive] = useState(false);
  const [agentSearch, setAgentSearch] = useState("");
  const [threadError, setThreadError] = useState(false);
  const readerTitle = useRef<HTMLHeadingElement>(null);
  const lastThreadButton = useRef<HTMLButtonElement | null>(null);
  const sessions = useMemo(() => overview?.agents ?? [], [overview]);
  const replyAgents = member ? sessions : noAgents;
  const waiting = phase === "ready" && sessions.length === 0;
  const activeView = view ?? (waiting && !member ? "diagnostics" : "messages");
  const participant = mailScope === "mine" && !member ? "human" : agent;
  const [query, setQuery] = useState("");
  const [searchDraft, setSearchDraft] = useState("");
  const [threads, setThreads] = useState<ThreadSummary[]>([]);
  // How many pages of threads the operator has opened under which filters, and the cursor of
  // the page after them. Other filters show other threads, so their older pages are not open.
  const [openedPages, setOpenedPages] = useState({ filters: "", pages: 1 });
  const [olderCursor, setOlderCursor] = useState<string | undefined>(undefined);
  const [threadsError, setThreadsError] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [thread, setThread] = useState<ThreadDetail | null>(null);
  const [compose, setCompose] = useState<Draft>(newDraft);
  // A conversation can be read through different repository views. Keep each view's
  // draft and retry identity together while the operator moves between conversations.
  const [replies, setReplies] = useState<Record<string, Draft>>({});
  const [emptyReply] = useState<Draft>(newDraft);
  const replyScope = JSON.stringify([repositoryId, selectedId]);
  const reply = replies[replyScope] ?? emptyReply;
  const setReply = (draft: Draft) => setReplies((current) => ({ ...current, [replyScope]: draft }));
  const composeReview = useReview(compose.key);
  const replyReview = useReview(reply.key);
  // One send at a time: a second submit while one is pending would be a second send. The ref
  // stops one that comes before React re-renders; the state disables the buttons.
  const inFlight = useRef(false);
  // Counts overview loads, so only the newest one lands. Closing the watch bumps it too.
  const overviewRequest = useRef(0);
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState("");
  const [formError, setFormError] = useState("");
  const [revoking, setRevoking] = useState<number | null>(null);
  const [signingOut, setSigningOut] = useState(false);
  const [localeSaving, setLocaleSaving] = useState(false);
  const writableRepositories = overview?.repositories.filter((repository) => repository.console_access === "rw") ?? [];
  const canSend = !member || writableRepositories.some((repository) => repository.id === repositoryId);
  const recipientAgents = member ? sessions.filter((agent) => writableRepositories.some((repository) => repository.id === agent.repository_id)) : sessions;
  const validRecipients = (draft: Draft) => recipientsOf(draft.to).every((id) => (!member && id === "human") || recipientAgents.some((agent) => agent.id === id));
  const replyVisible = !member || thread?.messages.some((message) => message.id === reply.replyTo) === true;
  const permissions = overview?.repositories.map((repository) => `${repository.id}:${repository.console_access}`).join(",");
  const previousPermissions = useRef<string | undefined>(undefined);
  useEffect(() => {
    if (member && previousPermissions.current !== undefined && permissions !== previousPermissions.current) {
      setCompose((draft) => edited(draft, {}));
      setReplies((drafts) => Object.fromEntries(Object.entries(drafts).map(([scope, draft]) => [scope, edited(draft, {})])));
      setNotice(t("accountUi.permissionChanged"));
    }
    previousPermissions.current = permissions;
  }, [member, permissions, t]);

  useEffect(() => {
    try {
      setSoundOn(soundEnabled());
      const saved = isSupportedLocale(session.account.ui_locale) ? session.account.ui_locale : "en";
      void loadLocale(saved).then(() => setLocale(saved));
    } catch {
      setSoundOn(false);
    }
  }, [session]);

  useEffect(() => {
    if (overview === null) {
      return;
    }
    const previous = Number(document.body.dataset.humanUnread ?? "-1");
    if (previous >= 0 && overview.human_unread > previous) {
      try {
        playNotificationTone();
      } catch {
        // A missing audio device never blocks the refresh.
      }
    }
    document.body.dataset.humanUnread = String(overview.human_unread);
  }, [overview]);

  // Reloads the overview on the watch's current connection. What the operator does reloads it
  // this way: reconnecting instead would make every read a new socket, whose first frame
  // reloads the open thread, which reads it again, without end.
  const reload = useCallback(async () => {
    const current = ++overviewRequest.current;
    try {
      const [nextOverview, nextDoctor] = await Promise.all([getOverview(), member ? Promise.resolve(null) : getDoctor()]);
      if (current !== overviewRequest.current) {
        return;
      }
      setOverview(nextOverview);
      setDoctor(nextDoctor);
      setPhase("ready");
    } catch (error) {
      if (current !== overviewRequest.current || isAbort(error)) {
        return;
      }
      setPhase("error");
    }
  }, [member]);

  // revision changes only to retry after an error, which reconnects too.
  useEffect(() => {
    const requests = overviewRequest;
    void reload();
    const stop = connect((frame: WatchFrame) => {
      if (frame.kind.length === 0) {
        return;
      }
      void reload();
      setThreadRevision((value) => value + 1);
    }, (status) => {
      // A refused session ends the watch; the operator is shown how to sign in again.
      if (status === "unauthorized") {
        onUnauthorized();
        return;
      }
      setStream(status);
    }, { check: () => getSession(undefined, session) });
    return () => {
      // A load still in flight lands on a closed view, so it is dropped.
      requests.current++;
      stop();
    };
  }, [connect, reload, revision, onUnauthorized, session]);

  useEffect(() => {
    if (phase !== "ready") {
      return;
    }
    let ignore = false;
    // Every refresh walks all the pages the operator has opened, from the newest. Reloading the
    // first page alone would let a new thread push one off it into a gap that no page shows,
    // and holding the old cursors would keep stale ones (gap review A15).
    const walk = async () => {
      const filters = threadFilters(repositoryId, participant, query);
      const threadPages = openedPages.filters === filters ? openedPages.pages : 1;
      const all: ThreadSummary[] = [];
      let before: string | undefined;
      for (let page = 0; page < threadPages; page++) {
        const next = await listThreads({ repository_id: repositoryId, agent: participant, q: query, before, limit: 50 });
        // A thread that got mail between two pages moves up and is listed again; keep it once.
        all.push(...next.threads.filter((item) => !all.some((seen) => seen.id === item.id)));
        before = next.next;
        if (before === undefined || ignore) {
          break;
        }
      }
      return { all, before };
    };
    void walk()
      .then(({ all, before }) => {
        if (!ignore) {
          setThreads(all);
          setOlderCursor(before);
          setThreadsError(false);
        }
      })
      .catch((error: unknown) => {
        if (ignore || isAbort(error)) {
          return;
        }
        setThreadsError(true);
      });
    return () => {
      ignore = true;
    };
  }, [participant, openedPages, phase, query, repositoryId, threadRevision]);

  useEffect(() => {
    if (phase !== "ready" || activeView !== "messages" || selectedId === null) {
      return;
    }
    let ignore = false;
    void getThread(selectedId, repositoryId)
      .then((detail) => {
        if (ignore) {
          return;
        }
        setThread(detail);
        setThreadError(false);
        // Opening a thread reads the operator's mail in it (A13); the overview's unread
        // counts then follow. A failure here leaves the mail unread, which is safe. Every
        // watch frame reloads the open thread, so a thread with nothing unread costs that
        // GET only: no read, and no second overview.
        if (!member && unreadForOperator(detail)) {
          readThread(selectedId, repositoryId)
            .then((read) => {
              if (read > 0) {
                void reload();
              }
            })
            .catch(() => undefined);
        }
        // Refresh an untouched suggestion, but retain an edited draft and its retry key
        // even when the operator left this conversation and has now returned to it.
        const suggestion = replyDraft(detail.messages);
        const fresh = suggestion !== null && member ? { ...suggestion, to: recipientsOf(suggestion.to).map((name) => {
          const matches = replyAgents.filter((agent) => agent.name === name);
          return matches.length === 1 ? matches[0]!.id : name;
        }).join(", ") } : suggestion;
        if (fresh !== null) {
          const scope = JSON.stringify([repositoryId, selectedId]);
          setReplies((current) => current[scope]?.offered === false ? current : { ...current, [scope]: fresh });
        }
      })
      .catch((error: unknown) => {
        if (ignore || isAbort(error)) {
          return;
        }
        setThreadError(true);
      });
    return () => {
      ignore = true;
    };
  }, [activeView, phase, reload, repositoryId, selectedId, threadRevision, member, replyAgents]);

  useEffect(() => {
    if (activeView !== "messages") {
      return;
    }
    // On phones the list is hidden while reading. Restore focus only after React
    // has made it visible again, otherwise the browser drops the focus request.
    if (selectedId === null) {
      lastThreadButton.current?.focus();
    } else {
      readerTitle.current?.focus();
    }
  }, [activeView, selectedId]);

  const chooseLocale = (next: SupportedLocale) => {
    setLocaleSaving(true);
    void updateProfile(session.account, session.account.display_name, next).then((account) => onSession({ ...session, account })).catch((error: unknown) => {
      if (error instanceof ApiError && error.status === 401) onSignedOut();
      else setFormError(t("localeStatus.saveError"));
    }).finally(() => setLocaleSaving(false));
  };

  const signOut = async () => {
    setSigningOut(true);
    try {
      await logout();
      onSignedOut();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) onSignedOut();
      else setFormError(t("accountUi.signOutError"));
    } finally {
      setSigningOut(false);
    }
  };

  const toggleSound = () => {
    const next = !soundOn;
    setSoundOn(next);
    try {
      setSoundEnabled(next);
      if (next) {
        playNotificationTone();
      }
    } catch {
      // Storage or audio can be blocked; the button still reflects the choice.
    }
  };

  const submit = async (start: Draft, keep: (draft: Draft) => void, clear: (sent: Draft) => void) => {
    if (inFlight.current) {
      return;
    }
    // The message belongs to the repository being read (ADR 004), and the hub refuses a key
    // reused for another repository. A retry from another view therefore gets a new key, and
    // the draft keeps it for its own retries.
    const expired = start.firstTried !== undefined && Date.now() - start.firstTried >= accountRetryWindow;
    if (expired) {
      keep(edited(start, { sentFrom: undefined }));
      setFormError(t("accountUi.retryExpired"));
      return;
    }
    const keyed = start.sentFrom === undefined || start.sentFrom === repositoryId ? { ...start, sentFrom: repositoryId } : { ...start, key: humanKey(), firstTried: undefined, sentFrom: repositoryId };
    const draft = { ...keyed, firstTried: keyed.firstTried ?? Date.now() };
    keep(draft);
    inFlight.current = true;
    setSending(true);
    setFormError("");
    setNotice("");
    try {
      await sendMessage({
        key: draft.key,
        to: recipientsOf(draft.to),
        subject: draft.subject,
        body: draft.body,
        reply_to: draft.replyTo,
        ...(repositoryId === "" ? {} : { repository_id: repositoryId }),
      });
      clear(draft);
      setNotice(t("operator.sent"));
      setThreadRevision((value) => value + 1);
      void reload();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onUnauthorized();
        return;
      }
      // A network failure or a 5xx may have come after the hub stored the message. The draft keeps
      // its key. The console offers a bounded retry; after its window, the draft
      // requires another review with a fresh key. A 4xx is a refusal.
      const ambiguous = !(error instanceof ApiError) || error.status >= 500;
      const uncertain = ambiguous && Date.now() - draft.firstTried < accountRetryWindow;
      setFormError(t(uncertain ? "accountUi.sendUncertain" : "operator.loadError"));
    } finally {
      inFlight.current = false;
      setSending(false);
    }
  };

  const onRevoke = async (claim: Claim) => {
    setRevoking(claim.fence);
    setFormError("");
    try {
      await revokeClaim(claim.fence);
      void reload();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        onUnauthorized();
        return;
      }
      setFormError(t("operator.loadError"));
    } finally {
      setRevoking(null);
    }
  };

  const composeRecipients = recipientsOf(compose.to);
  const composeSendable = canSend && sendable(compose) && validRecipients(compose);
  const activeAgents = sessions.filter((item) => item.state === "active");
  const visibleAgents = sessions.filter((item) => (showInactive || item.state === "active") &&
    `${item.name} ${item.host} ${item.client} ${item.branch} ${repositoryName(overview!, item.repository_id)}`.toLowerCase().includes(agentSearch.toLowerCase().trim()));
  const clearThread = () => {
    replyReview.stop();
    composeReview.stop();
    setSelectedId(null);
    setThread(null);
    setThreadError(false);
  };
  const chooseView = (next: ConsoleView) => {
    if (next !== activeView) {
      replyReview.stop();
      composeReview.stop();
    }
    setView(next);
  };
  const openThread = (id: string, trigger: HTMLButtonElement) => {
    lastThreadButton.current = trigger;
    chooseView("messages");
    if (id === selectedId) {
      readerTitle.current?.focus();
      return;
    }
    // Review is an explicit step in this visit to the draft. Restoring its text must
    // not reopen the send control and pull focus away from the conversation heading.
    replyReview.stop();
    setThread(null);
    setThreadError(false);
    setSelectedId(id);
  };
  const openMailbox = (scope: "mine" | "all") => {
    chooseView("messages");
    setMailScope(scope);
    clearThread();
  };
  const title = activeView === "messages" ? t(mailScope === "mine" ? "console.myMessages" : "console.allActivity")
    : activeView === "fleet" ? t("console.fleet")
    : activeView === "claims" ? t("console.reservations")
    : activeView === "diagnostics" ? t("console.diagnostics") : activeView === "account" ? t("accountUi.account") : activeView === "access" ? t("access.title") : t("operator.compose");

  return (
    <div className="app-shell operator-console">
      <a className="skip-link" href="#content">
        {t("skipToContent")}
      </a>
      <header className="topbar">
        <a className="brand" href="#content" onClick={() => openMailbox("mine")}>
          <span className="brand-mark" aria-hidden="true">
            <img src="/iris-rainbow.svg" alt="" className="brand-logo" width="34" height="34" />
          </span>
          <span>
            <strong>{t("appName")}</strong>
          </span>
        </a>
        <div className="topbar-controls">
          <p className={stream === "live" ? "stream-status stream-live" : "stream-status stream-reconnecting"}>
            {t(stream === "live" ? "stream.live" : stream === "reconnecting" ? "stream.reconnecting" : "stream.connecting")}
          </p>
          <LocalePicker disabled={localeSaving} locale={locale} onSelect={chooseLocale} />
          <button type="button" className="account-identity" onClick={() => chooseView("account")}>{session.account.display_name || session.account.username}</button>
          <button type="button" disabled={signingOut} onClick={() => void signOut()}>{t(signingOut ? "accountUi.signingOut" : "accountUi.signOut")}</button>
          <button
            type="button"
            className="sound-toggle"
            data-on={soundOn ? "yes" : "no"}
            aria-pressed={soundOn}
            aria-label={t(soundOn ? "notificationSoundOn" : "notificationSoundOff")}
            onClick={toggleSound}
          >
            {soundOn ? "♪" : "♩"}
          </button>
        </div>
      </header>
      <div className="console-workspace">
        {phase === "ready" && overview !== null ? (
          <aside className="console-sidebar">
            {canSend ? <button type="button" className="primary-button compose-action" onClick={() => chooseView("compose")}>+ {t("operator.compose")}</button> : null}
            <nav className="console-nav" aria-label={t("navigation")}>
              <button type="button" className="console-nav-item" aria-current={activeView === "messages" && mailScope === "mine" ? "page" : undefined} onClick={() => openMailbox("mine")}>
                {t(member ? "accountUi.messages" : "console.myMessages")}<span className="nav-count">{overview.human_unread}</span>
              </button>
              {!member ? <button type="button" className="console-nav-item" aria-current={activeView === "messages" && mailScope === "all" ? "page" : undefined} onClick={() => openMailbox("all")}>{t("console.allActivity")}</button> : null}
              <button type="button" className="console-nav-item" aria-current={activeView === "fleet" ? "page" : undefined} onClick={() => chooseView("fleet")}>{t("console.fleet")}<span className="nav-count">{activeAgents.length}</span></button>
              <button type="button" className="console-nav-item" aria-current={activeView === "claims" ? "page" : undefined} onClick={() => chooseView("claims")}>{t("operator.claims")}<span className="nav-count">{overview.claims.length}</span></button>
              {!member ? <button type="button" className="console-nav-item" aria-current={activeView === "diagnostics" ? "page" : undefined} onClick={() => chooseView("diagnostics")}>{t("console.diagnostics")}</button> : null}
              <button type="button" className="console-nav-item" aria-current={activeView === "account" ? "page" : undefined} onClick={() => chooseView("account")}>{t("accountUi.account")}</button>
              {session.account.role === "operator" ? <button type="button" className="console-nav-item" aria-current={activeView === "access" ? "page" : undefined} onClick={() => chooseView("access")}>{t("access.title")}</button> : null}
            </nav>
          </aside>
        ) : null}
      <main id="content" className="content console-content" data-view={activeView}>
        {phase === "loading" ? <p className="state-panel">{t("inbox.loading")}</p> : null}
        {phase === "error" ? (
          <div className="state-panel state-error" role="alert">
            <p>{t("operator.loadError")}</p>
            <button type="button" className="primary-button" onClick={() => setRevision((value) => value + 1)}>
              {t("operator.retry")}
            </button>
          </div>
        ) : null}
        {phase === "ready" && overview !== null ? (
          <>
            {activeView !== "account" && activeView !== "access" ? <header className="console-heading">
              <div><p className="console-kicker">{t("appName")}</p><h1>{member && activeView === "messages" ? t("accountUi.messages") : title}</h1>
                <p>{activeView === "messages" ? t("accountUi.sharedMail") : activeView === "fleet" ? t("console.fleetHint") : activeView === "compose" ? t("console.composeHint") : null}</p>
              </div>
            </header> : null}
            <div className="console-notices">
              {member && overview.repositories.length === 0 ? <p role="status">{t("accountUi.noRepositories")}</p> : null}
              {notice.length > 0 ? <p role="status">{notice}</p> : null}
              {formError.length > 0 ? <p role="alert">{formError}</p> : null}
            </div>
            <div hidden={activeView !== "messages"} className="console-view">
              <div className="console-stats">
                <button type="button" className="console-stat" onClick={() => openMailbox("mine")}><span>{t("console.unread")}</span><strong>{overview.human_unread}</strong></button>
                <button type="button" className="console-stat" onClick={() => chooseView("fleet")}><span>{t("console.active")}</span><strong>{activeAgents.length}</strong></button>
                <button type="button" className="console-stat" onClick={() => chooseView("claims")}><span>{t("console.reservations")}</span><strong>{overview.claims.length}</strong></button>
              </div>
              <DeliveryProblems repositoryId={repositoryId} revision={threadRevision} onOpenThread={openThread} />
              <section id="threads" aria-label={t("operator.threads")}>
                <form className="search-form mail-filters" onSubmit={(event) => { event.preventDefault(); setQuery(searchDraft); clearThread(); }}>
                  <label>{t("operator.search")}<input type="search" value={searchDraft} onChange={(event) => setSearchDraft(event.target.value)} /></label>
                  <label>{t("operator.repository")}
                    <select value={repositoryId} onChange={(event) => { setRepositoryId(event.target.value); clearThread(); }}>
                      <option value="">{t("operator.all")}</option>
                      {overview.repositories.map((repository) => <option key={repository.id} value={repository.id}>{repository.name} · {repository.id}</option>)}
                    </select>
                  </label>
                  {mailScope === "all" ? <label>{t("operator.agent")}
                    <select value={agent} onChange={(event) => { setAgent(event.target.value); clearThread(); }}>
                      <option value="">{t("operator.all")}</option>
                      {sessions.map((item) => <option key={item.id} value={item.name}>{item.name}</option>)}
                    </select>
                  </label> : null}
                  <button type="submit" className="primary-button">{t("search.submit")}</button>
                </form>
                <div className="mailbox-layout" data-reading={selectedId !== null ? "yes" : "no"}>
                  <div className="mailbox-list">
                    <div className="panel-heading"><h2>{t("operator.threads")}</h2></div>
                    {threadsError ? <p role="alert">{t("operator.loadError")}</p> : null}
                    <div className="thread-list">
                      {threads.length === 0 ? <div className="thread-empty"><h3>{t(mailScope === "mine" && query === "" && repositoryId === "" ? "console.emptyInbox" : "operator.emptyThreads")}</h3><p>{t("console.emptyHint")}</p></div> : (
                        <ul className="message-list">
                          {threads.map((item) => <li key={item.id}>
                            <button type="button" className="thread-row" aria-current={item.id === selectedId ? "true" : undefined} data-unread={item.unread > 0 ? "yes" : "no"} onClick={(event) => openThread(item.id, event.currentTarget)}>
                              <span className="thread-row-top"><strong>{item.subject}</strong><time dateTime={item.last_at}>{formatWhen(item.last_at)}</time></span>
                              <span className="thread-participants">{item.participants.join(", ")}{item.authors?.map((author) => <span key={author.sender_account_id}> · {author.sender_display_name || author.sender_username}</span>)}</span>
                              <span className="thread-row-bottom"><span>{t("inbox.count", { count: item.count })}</span><span>{t("operator.unread", { count: item.unread })}</span></span>
                              <span className="sr-only">{t("operator.openThread", { subject: item.subject })}</span>
                            </button>
                          </li>)}
                        </ul>
                      )}
                      {olderCursor !== undefined ? <button type="button" className="back-link" onClick={() => {
                        const filters = threadFilters(repositoryId, participant, query);
                        setOpenedPages((opened) => ({ filters, pages: opened.filters === filters ? opened.pages + 1 : 2 }));
                      }}>{t("operator.olderThreads")}</button> : null}
                    </div>
                  </div>
                  {selectedId === null ? <div className="thread-empty"><h2>{t("console.selectThread")}</h2><p>{t("console.selectHint")}</p></div> : (
                    <article className="message-detail thread-reader" aria-labelledby="thread-title" aria-busy={thread === null && !threadError}>
                      <button type="button" className="back-link reader-back" onClick={clearThread}>{t("operator.back")}</button>
                      <header className="page-heading">
                        <h2 id="thread-title" tabIndex={-1} ref={readerTitle}>{thread === null ? t(threadError ? "operator.loadError" : "message.loading") : thread.messages[0]?.subject ?? thread.id}</h2>
                      </header>
                      {threadError ? <div className="state-panel"><p role="alert">{t("operator.loadError")}</p><button type="button" onClick={() => { readerTitle.current?.focus(); setThreadError(false); setThreadRevision((value) => value + 1); }}>{t("operator.retry")}</button></div> : null}
                      {thread !== null ? <>
                  {thread.messages.map((message) => (
                    <article key={message.id} className="thread-message">
                      <header>
                        <strong>{message.sender_account_id ? t("accountUi.sharedAuthor", { person: message.sender_display_name || message.sender_username, sender: message.sender }) : message.sender}</strong>
                        {overview !== null && message.repository_id !== undefined && message.repository_id !== repositoryId ? (
                          <span className="message-repository">{repositoryName(overview, message.repository_id)}</span>
                        ) : null}
                        <small>
                          {t("message.to")}: {message.recipients.map((recipient) => `${recipient.name} (${recipient.state})`).join(", ")}
                        </small>
                        <time dateTime={message.created_at}>{formatWhen(message.created_at)}</time>
                      </header>
                      <MessageBody body={message.body} />
                    </article>
                  ))}
                  {thread.messages.length > 0 && canSend && replyVisible ? <form
                    className="search-form"
                    onSubmit={(event) => {
                      event.preventDefault();
                      if (sendable(reply) && (!member || validRecipients(reply))) {
                        replyReview.start();
                      }
                    }}
                  >
                    <h3>{t("operator.reply")}</h3>
                    {member ? <RecipientPicker agents={recipientAgents} selected={recipientsOf(reply.to)} allowHuman={false} onChange={(ids) => setReply(edited(reply, { to: ids.join(", ") }))} /> : <label>
                      {t("operator.recipients")}
                      <input value={reply.to} onChange={(event) => setReply(edited(reply, { to: event.target.value }))} />
                    </label>}
                    <label>
                      {t("operator.subject")}
                      <input value={reply.subject} onChange={(event) => setReply(edited(reply, { subject: event.target.value }))} />
                    </label>
                    <SubjectSize subject={reply.subject} />
                    <DraftBody value={reply.body} onChange={(body) => setReply(edited(reply, { body }))} />
                    <p>{t("markdown.byteCount", { count: byteLength(reply.body), maximum: maximumBodyBytes })}</p>
                    <DraftReview
                      review={replyReview}
                      recipients={recipientsOf(reply.to)}
                      subject={reply.subject}
                      body={reply.body}
                      sendable={sendable(reply) && (!member || validRecipients(reply))}
                      sending={sending}
                      sendLabel={t("operator.reply")}
                      onSend={() =>
                        void submit(reply, setReply, (sent) => {
                          // Only this send's unchanged draft is finished. A different thread,
                          // or words typed while sending, must survive its completion.
                          const cleared = { ...edited(sent, { body: "" }), offered: true };
                          setReplies((current) => Object.fromEntries(Object.entries(current).map(
                            ([scope, draft]) => [scope, draft.key === sent.key ? cleared : draft],
                          )));
                        })
                      }
                    />
                  </form> : null}
                      </> : null}
                    </article>
                  )}
                </div>
              </section>
            </div>
            {!member ? <div hidden={activeView !== "diagnostics"} className="console-view">
            <section id="overview" className="panel" aria-labelledby="clients-title">
              <div className="panel-heading">
                <h2 id="clients-title">{t("operator.doctorTitle")}</h2>
                <button type="button" className="primary-button" onClick={() => void reload()}>
                  {t("operator.doctorReload")}
                </button>
              </div>
              <p>{t("operator.doctorHint", { command: "iris doctor" })}</p>
              {doctor === null || doctor.hosts.length === 0 ? (
                <p className="state-panel">{t("operator.doctorMissing", { command: "iris doctor" })}</p>
              ) : (
                doctor.hosts.map((report) => <HostReport key={report.host} report={report} />)
              )}
              {waiting ? (
                <p className="state-panel" role="status">
                  {t("operator.waiting")}
                </p>
              ) : null}
            </section>
            </div> : null}
            <div hidden={activeView !== "fleet"} className="console-view">
            <section className="content-grid" aria-label={t("operator.repositories")}>
              <article className="panel">
                <div className="panel-heading">
                  <h2>{t("operator.repositories")}</h2>
                  <span>{t("operator.unread", { count: overview.human_unread })}</span>
                </div>
                {overview.repositories.length === 0 ? <p>{t("operator.none")}</p> : (
                  <table className="stack-table">
                    <caption className="sr-only">{t("operator.repositories")}</caption>
                    <thead>
                      <tr>
                        <th scope="col">{t("operator.repository")}</th>
                        <th scope="col">{t("operator.detail")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {overview.repositories.map((repository) => (
                        <tr key={repository.id}>
                          <th scope="row">{repository.name}</th>
                          <td><details className="repository-details"><summary>{t("console.aliases")}</summary><code>{repository.id}</code><ul>{repository.aliases.map((alias) => <li key={alias}>{alias}</li>)}</ul></details></td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </article>
              <article className="panel">
                <div className="panel-heading">
                  <h2>{t("operator.agents")}</h2>
                </div>
                <div className="fleet-toolbar">
                  <label>{t("console.findAgent")}<input type="search" value={agentSearch} onChange={(event) => setAgentSearch(event.target.value)} /></label>
                  <label><input type="checkbox" checked={showInactive} onChange={(event) => setShowInactive(event.target.checked)} />{t("console.showInactive")}</label>
                </div>
                {visibleAgents.length === 0 ? <p>{t("console.noAgentMatch")}</p> : (
                  <table className="stack-table">
                    <caption className="sr-only">{t("operator.agents")}</caption>
                    <thead>
                      <tr>
                        <th scope="col">{t("operator.agent")}</th>
                        <th scope="col">{t("operator.detail")}</th>
                        <th scope="col">{t("operator.repository")}</th>
                        {!member ? <th scope="col">{t("operator.clientHealth")}</th> : null}
                      </tr>
                    </thead>
                    <tbody>
                      {visibleAgents.map((item: Agent) => {
                        const health = healthOf(doctor, item);
                        return (
                          <tr key={item.id}>
                            <th scope="row">{item.name}</th>
                            <td>
                              {item.host} · {item.client} · {item.state}<br />{item.branch}
                            </td>
                            <td>{repositoryName(overview, item.repository_id)}</td>
                            {!member ? <td data-health={health?.status ?? "none"}>{health === undefined ? t("operator.noHostReport") : t(statusKey(health.status))}</td> : null}
                          </tr>
                        );
                      })}
                    </tbody>
                  </table>
                )}
              </article>

            </section>
            </div>
            <div hidden={activeView !== "claims"} className="console-view">
              <ClaimsPanel
                claims={overview.claims}
                repositories={overview.repositories}
                revoking={revoking}
                onRevoke={(claim) => void onRevoke(claim)}
                canRevoke={!member}
              />

            </div>
            {canSend ? <section id="compose" hidden={activeView !== "compose"} className="panel console-view" aria-labelledby="compose-title">
              <div className="panel-heading">
                <h2 id="compose-title">{t("operator.compose")}</h2>
              </div>
              <form
                className="search-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  if (composeSendable) {
                    composeReview.start();
                  }
                }}
              >
                <label>{t("console.composeRepository")}
                  <select value={repositoryId} onChange={(event) => { setRepositoryId(event.target.value); clearThread(); }}>
                    {!member ? <option value="">{t("console.automaticRepository")}</option> : null}
                    {(member ? writableRepositories : overview.repositories).map((repository) => <option key={repository.id} value={repository.id}>{repository.name} · {repository.id}</option>)}
                  </select>
                </label>
                <RecipientPicker agents={recipientAgents} allowHuman={!member} selected={composeRecipients} onChange={(ids) => setCompose(edited(compose, { to: ids.join(", ") }))} />
                <label>
                  {t("operator.subject")}
                  <input value={compose.subject} onChange={(event) => setCompose(edited(compose, { subject: event.target.value }))} />
                </label>
                <SubjectSize subject={compose.subject} />
                <DraftBody value={compose.body} onChange={(body) => setCompose(edited(compose, { body }))} />
                <p>{t("markdown.byteCount", { count: byteLength(compose.body), maximum: maximumBodyBytes })}</p>
                <DraftReview
                  review={composeReview}
                  recipients={composeRecipients.map((id) => {
                    const agent = sessions.find((entry) => entry.id === id);
                    return agent ? `${agent.name} (${id})` : id;
                  })}
                  subject={compose.subject}
                  body={compose.body}
                  sendable={composeSendable}
                  sending={sending}
                  sendLabel={t("operator.send")}
                  onSend={() => void submit(compose, setCompose, (sent) => {
                    const cleared = newDraft();
                    setCompose((current) => current.key === sent.key ? cleared : current);
                  })}
                />
              </form>
            </section> : null}
            {activeView === "account" ? <Account session={session} onSession={onSession} onSignedOut={onSignedOut} /> : null}
            {session.account.role === "operator" && activeView === "access" ? <AccessPanel session={session} repositories={overview.repositories} onSignedOut={onSignedOut} /> : null}
          </>
        ) : null}
      </main>
      </div>
    </div>
  );
}

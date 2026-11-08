import { useCallback, useEffect, useRef, useState } from "react";
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
  type DoctorReport,
  type DoctorStatus,
  type Overview,
  type ThreadDetail,
  type ThreadMessage,
  type ThreadSummary,
  type WatchFrame,
  type WatchStatus,
} from "./api";
import { loadLocale, type SupportedLocale } from "./i18n";
import LocalePicker from "./LocalePicker";
import { MessageBody } from "./MessageBody";
import {
  playNotificationTone,
  setSoundEnabled,
  soundEnabled,
} from "./notificationSound";
import { readUiLocale, writeUiLocale } from "./preferences";
import "./app.css";

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
  /** The thread a reply draft is for. */
  thread?: string;
  /**
   * Whether the console made this reply draft and the operator has not touched it, so a refresh
   * may replace it. An edit makes it the operator's, and a live refresh never discards it (A14).
   */
  offered?: boolean;
}

function newDraft(fields: Partial<Draft> = {}): Draft {
  return { to: "", subject: "", body: "", replyTo: "", ...fields, key: humanKey() };
}

// operatorName is the sender name the hub gives the console's own messages.
const operatorName = "human";

// replyDraft starts a reply to a thread. It answers the other party: the latest sender who is not
// the operator, or, in a thread only the operator has written to, whom they wrote to. Answering
// the latest sender would address a follow-up after the operator's own message back to them.
function replyDraft(thread: string, messages: ThreadMessage[]): Draft | null {
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
  return newDraft({ to, subject: last.subject.startsWith("Re:") ? last.subject : `Re: ${last.subject}`, replyTo: last.id, thread, offered: true });
}

// unreadForOperator is whether a thread holds mail to the operator not yet acknowledged.
function unreadForOperator(detail: ThreadDetail): boolean {
  return detail.messages.some((message) =>
    message.recipients.some((recipient) => recipient.name === operatorName && recipient.state !== "acked" && recipient.state !== "handled"),
  );
}

function edited(draft: Draft, change: Partial<Draft>): Draft {
  return { ...draft, ...change, key: humanKey(), offered: false };
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

export function App({ connect = watchUi }: { connect?: typeof watchUi } = {}) {
  const { t } = useTranslation();
  const [locale, setLocale] = useState<SupportedLocale>("en");
  const [soundOn, setSoundOn] = useState(false);
  const [phase, setPhase] = useState<"loading" | "ready" | "unauthorized" | "error">("loading");
  const [overview, setOverview] = useState<Overview | null>(null);
  const [doctor, setDoctor] = useState<DoctorReport | null>(null);
  const [revision, setRevision] = useState(0);
  const [threadRevision, setThreadRevision] = useState(0);
  const [stream, setStream] = useState<WatchStatus>("connecting");
  const [repositoryId, setRepositoryId] = useState("");
  const [agent, setAgent] = useState("");
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
  const [reply, setReply] = useState<Draft>(newDraft);
  // One send at a time: a second submit while one is pending would be a second send. The ref
  // stops one that comes before React re-renders; the state disables the buttons.
  const inFlight = useRef(false);
  // Counts overview loads, so only the newest one lands. Closing the watch bumps it too.
  const overviewRequest = useRef(0);
  const [sending, setSending] = useState(false);
  const [notice, setNotice] = useState("");
  const [formError, setFormError] = useState("");
  const [revoking, setRevoking] = useState<number | null>(null);

  useEffect(() => {
    try {
      setSoundOn(soundEnabled());
      const saved = readUiLocale();
      if (saved !== null) {
        void loadLocale(saved).then(() => setLocale(saved));
      }
    } catch {
      setSoundOn(false);
    }
  }, []);

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
      const [nextOverview, nextDoctor] = await Promise.all([getOverview(), getDoctor()]);
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
      setPhase(error instanceof ApiError && error.status === 401 ? "unauthorized" : "error");
    }
  }, []);

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
    }, setStream);
    return () => {
      // A load still in flight lands on a closed view, so it is dropped.
      requests.current++;
      stop();
    };
  }, [connect, reload, revision]);

  useEffect(() => {
    if (phase !== "ready") {
      return;
    }
    let ignore = false;
    // Every refresh walks all the pages the operator has opened, from the newest. Reloading the
    // first page alone would let a new thread push one off it into a gap that no page shows,
    // and holding the old cursors would keep stale ones (gap review A15).
    const walk = async () => {
      const filters = threadFilters(repositoryId, agent, query);
      const threadPages = openedPages.filters === filters ? openedPages.pages : 1;
      const all: ThreadSummary[] = [];
      let before: string | undefined;
      for (let page = 0; page < threadPages; page++) {
        const next = await listThreads({ repository_id: repositoryId, agent, q: query, before, limit: 50 });
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
        if (error instanceof ApiError && error.status === 401) {
          setPhase("unauthorized");
          return;
        }
        setThreadsError(true);
      });
    return () => {
      ignore = true;
    };
  }, [agent, openedPages, phase, query, repositoryId, threadRevision]);

  useEffect(() => {
    if (phase !== "ready" || selectedId === null) {
      return;
    }
    let ignore = false;
    void getThread(selectedId, repositoryId)
      .then((detail) => {
        if (ignore) {
          return;
        }
        setThread(detail);
        // Opening a thread reads the operator's mail in it (A13); the overview's unread
        // counts then follow. A failure here leaves the mail unread, which is safe. Every
        // watch frame reloads the open thread, so a thread with nothing unread costs that
        // GET only: no read, and no second overview.
        if (unreadForOperator(detail)) {
          readThread(selectedId, repositoryId)
            .then((read) => {
              if (read > 0) {
                void reload();
              }
            })
            .catch(() => undefined);
        }
        // A reply is per thread: opening another thread replaces even an edited draft, so one
        // thread's words never go out as a reply in another.
        const fresh = replyDraft(detail.id, detail.messages);
        if (fresh !== null) {
          setReply((current) => (current.thread === detail.id && current.offered === false ? current : fresh));
        }
      })
      .catch((error: unknown) => {
        if (ignore || isAbort(error)) {
          return;
        }
        if (error instanceof ApiError && error.status === 401) {
          setPhase("unauthorized");
          return;
        }
        setFormError(t("operator.loadError"));
      });
    return () => {
      ignore = true;
    };
  }, [phase, reload, repositoryId, selectedId, t, threadRevision]);

  const chooseLocale = (next: SupportedLocale) => {
    try {
      writeUiLocale(next);
    } catch {
      // The language still changes for this visit.
    }
    void loadLocale(next).then(() => setLocale(next));
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

  const submit = async (start: Draft, keep: (draft: Draft) => void, clear: () => void) => {
    if (inFlight.current) {
      return;
    }
    // The message belongs to the repository being read (ADR 004), and the hub refuses a key
    // reused for another repository. A retry from another view therefore gets a new key, and
    // the draft keeps it for its own retries.
    const draft = start.sentFrom === undefined || start.sentFrom === repositoryId ? { ...start, sentFrom: repositoryId } : { ...start, key: humanKey(), sentFrom: repositoryId };
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
      clear();
      setNotice(t("operator.sent"));
      setThreadRevision((value) => value + 1);
      void reload();
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setPhase("unauthorized");
        return;
      }
      setFormError(t("operator.loadError"));
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
        setPhase("unauthorized");
        return;
      }
      setFormError(t("operator.loadError"));
    } finally {
      setRevoking(null);
    }
  };

  const sessions = overview?.agents ?? [];
  const waiting = phase === "ready" && sessions.length === 0;

  return (
    <div className="app-shell">
      <a className="skip-link" href="#content">
        {t("skipToContent")}
      </a>
      <header className="topbar">
        <a className="brand" href="#overview">
          <span className="brand-mark" aria-hidden="true">
            I
          </span>
          <span>
            <strong>{t("appName")}</strong>
            <small>{t("appSubtitle")}</small>
          </span>
        </a>
        <div className="topbar-controls">
          <p className={stream === "live" ? "stream-status stream-live" : "stream-status stream-reconnecting"}>
            {t(stream === "live" ? "stream.live" : stream === "reconnecting" ? "stream.reconnecting" : "stream.connecting")}
          </p>
          <LocalePicker disabled={false} locale={locale} onSelect={chooseLocale} />
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
      <main id="content" className="content">
        {phase === "loading" ? <p className="state-panel">{t("inbox.loading")}</p> : null}
        {phase === "error" ? (
          <div className="state-panel state-error" role="alert">
            <p>{t("operator.loadError")}</p>
            <button type="button" className="primary-button" onClick={() => setRevision((value) => value + 1)}>
              {t("operator.retry")}
            </button>
          </div>
        ) : null}
        {phase === "unauthorized" ? (
          <section className="welcome" aria-labelledby="sign-in-title">
            <h1 id="sign-in-title">{t("appName")}</h1>
            <p>{t("operator.signIn", { command: "iris ui" })}</p>
          </section>
        ) : null}
        {phase === "ready" && overview !== null ? (
          <>
            <section id="overview" className="panel" aria-labelledby="clients-title">
              <div className="panel-heading">
                <h1 id="clients-title">{t("operator.doctorTitle")}</h1>
                <button type="button" className="primary-button" onClick={() => void reload()}>
                  {t("operator.doctorReload")}
                </button>
              </div>
              <p>{t("operator.doctorHint", { command: "iris doctor" })}</p>
              {doctor !== null && doctor.checked_at !== null ? (
                <p>{t("operator.checkedAt", { date: formatWhen(doctor.checked_at) })}</p>
              ) : null}
              {doctor === null || doctor.clients.length === 0 ? (
                <p className="state-panel">{t("operator.doctorMissing", { command: "iris doctor" })}</p>
              ) : (
                <ul className="message-list">
                  {doctor.clients.map((client) => (
                    <li key={client.client}>
                      <article className="message-row" data-client={client.client} data-status={client.status}>
                        <div>
                          <strong>{client.client}</strong>
                          <small>
                            {t("operator.version")}: {client.version.length > 0 ? client.version : t("operator.none")}
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
              )}
              {waiting ? (
                <p className="state-panel" role="status">
                  {t("operator.waiting")}
                </p>
              ) : null}
            </section>

            <section className="content-grid" aria-label={t("operator.repositories")}>
              <article className="panel">
                <div className="panel-heading">
                  <h2>{t("operator.repositories")}</h2>
                  <span>{t("operator.unread", { count: overview.human_unread })}</span>
                </div>
                {overview.repositories.length === 0 ? <p>{t("operator.none")}</p> : (
                  <table>
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
                          <td>{repository.aliases.join(", ")}</td>
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
                {sessions.length === 0 ? <p>{t("operator.waiting")}</p> : (
                  <table>
                    <caption className="sr-only">{t("operator.agents")}</caption>
                    <thead>
                      <tr>
                        <th scope="col">{t("operator.agent")}</th>
                        <th scope="col">{t("operator.detail")}</th>
                        <th scope="col">{t("operator.repository")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {sessions.map((item: Agent) => (
                        <tr key={item.id}>
                          <th scope="row">{item.name}</th>
                          <td>
                            {item.client} · {item.state} · {item.branch}
                          </td>
                          <td>{repositoryName(overview, item.repository_id)}</td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </article>
              <article className="panel">
                <div className="panel-heading">
                  <h2>{t("operator.claims")}</h2>
                </div>
                {overview.claims.length === 0 ? <p>{t("operator.none")}</p> : (
                  <table>
                    <caption className="sr-only">{t("operator.claims")}</caption>
                    <thead>
                      <tr>
                        <th scope="col">{t("reservations.path")}</th>
                        <th scope="col">{t("reservations.holder")}</th>
                        <th scope="col">{t("reservations.expires")}</th>
                        <th scope="col">{t("operator.revoke")}</th>
                      </tr>
                    </thead>
                    <tbody>
                      {overview.claims.map((claim) => (
                        <tr key={claim.fence}>
                          <th scope="row">{claim.path}</th>
                          <td>
                            {claim.holder} · {claim.kind}
                          </td>
                          <td>{claim.expires_at === null ? t("operator.none") : formatWhen(claim.expires_at)}</td>
                          <td>
                            <button
                              type="button"
                              className="primary-button"
                              disabled={revoking === claim.fence}
                              onClick={() => void onRevoke(claim)}
                            >
                              {revoking === claim.fence ? t("operator.revoking") : t("operator.revoke")}
                            </button>
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </article>
            </section>

            <section id="threads" className="panel" aria-labelledby="threads-title">
              <div className="panel-heading">
                <h2 id="threads-title">{t("operator.threads")}</h2>
              </div>
              <form
                className="search-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  setQuery(searchDraft);
                  setSelectedId(null);
                  setThread(null);
                }}
              >
                <label>
                  {t("operator.search")}
                  <input value={searchDraft} onChange={(event) => setSearchDraft(event.target.value)} />
                </label>
                <label>
                  {t("operator.repository")}
                  <select value={repositoryId} onChange={(event) => setRepositoryId(event.target.value)}>
                    <option value="">{t("operator.all")}</option>
                    {overview.repositories.map((repository) => (
                      <option key={repository.id} value={repository.id}>
                        {repository.name}
                      </option>
                    ))}
                  </select>
                </label>
                <label>
                  {t("operator.agent")}
                  <select value={agent} onChange={(event) => setAgent(event.target.value)}>
                    <option value="">{t("operator.all")}</option>
                    {sessions.map((item) => (
                      <option key={item.id} value={item.name}>
                        {item.name}
                      </option>
                    ))}
                  </select>
                </label>
                <button type="submit" className="primary-button">
                  {t("search.submit")}
                </button>
              </form>
              {threadsError ? <p role="alert">{t("operator.loadError")}</p> : null}
              {threads.length === 0 ? <p>{t("operator.emptyThreads")}</p> : (
                <ul className="message-list">
                  {threads.map((item) => (
                    <li key={item.id}>
                      <button
                        type="button"
                        className="message-row"
                        onClick={() => setSelectedId(item.id)}
                      >
                        <strong>{item.subject}</strong>
                        <small>
                          {t("operator.participants")}: {item.participants.join(", ")} · {t("operator.unread", { count: item.unread })}
                        </small>
                        <span className="sr-only">{t("operator.openThread", { subject: item.subject })}</span>
                      </button>
                    </li>
                  ))}
                </ul>
              )}
              {olderCursor !== undefined ? (
                <button
                  type="button"
                  onClick={() => {
                    const filters = threadFilters(repositoryId, agent, query);
                    setOpenedPages((opened) => ({ filters, pages: opened.filters === filters ? opened.pages + 1 : 2 }));
                  }}
                >
                  {t("operator.olderThreads")}
                </button>
              ) : null}
              {thread !== null ? (
                <article className="message-detail" aria-labelledby="thread-title">
                  <header className="page-heading">
                    <button type="button" className="back-link" onClick={() => { setSelectedId(null); setThread(null); }}>
                      {t("operator.back")}
                    </button>
                    <h2 id="thread-title">{thread.messages[0]?.subject ?? thread.id}</h2>
                  </header>
                  {thread.messages.map((message) => (
                    <article key={message.id} className="thread-message">
                      <header>
                        <strong>{message.sender}</strong>
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
                  <form
                    className="search-form"
                    onSubmit={(event) => {
                      event.preventDefault();
                      void submit(reply, setReply, () =>
                        // A sent reply leaves nothing of the operator's to keep, so the next
                        // refresh may answer the newest message.
                        setReply((current) => ({ ...edited(current, { body: "" }), offered: true })),
                      );
                    }}
                  >
                    <h3>{t("operator.reply")}</h3>
                    <label>
                      {t("operator.recipients")}
                      <input value={reply.to} onChange={(event) => setReply(edited(reply, { to: event.target.value }))} />
                    </label>
                    <label>
                      {t("operator.subject")}
                      <input value={reply.subject} onChange={(event) => setReply(edited(reply, { subject: event.target.value }))} />
                    </label>
                    <SubjectSize subject={reply.subject} />
                    <label>
                      {t("operator.body")}
                      <textarea value={reply.body} onChange={(event) => setReply(edited(reply, { body: event.target.value }))} />
                    </label>
                    <p>{t("markdown.byteCount", { count: byteLength(reply.body), maximum: maximumBodyBytes })}</p>
                    <button
                      type="submit"
                      className="primary-button"
                      disabled={sending || !sendable(reply)}
                    >
                      {t("operator.reply")}
                    </button>
                  </form>
                </article>
              ) : null}
            </section>

            <section id="compose" className="panel" aria-labelledby="compose-title">
              <div className="panel-heading">
                <h2 id="compose-title">{t("operator.compose")}</h2>
              </div>
              <form
                className="search-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  void submit(compose, setCompose, () => setCompose(newDraft()));
                }}
              >
                <label>
                  {t("operator.recipients")}
                  <input value={compose.to} onChange={(event) => setCompose(edited(compose, { to: event.target.value }))} />
                </label>
                <label>
                  {t("operator.subject")}
                  <input value={compose.subject} onChange={(event) => setCompose(edited(compose, { subject: event.target.value }))} />
                </label>
                <SubjectSize subject={compose.subject} />
                <label>
                  {t("operator.body")}
                  <textarea value={compose.body} onChange={(event) => setCompose(edited(compose, { body: event.target.value }))} />
                </label>
                <p>{t("markdown.byteCount", { count: byteLength(compose.body), maximum: maximumBodyBytes })}</p>
                <button
                  type="submit"
                  className="primary-button"
                  disabled={sending || !sendable(compose)}
                >
                  {t("operator.send")}
                </button>
              </form>
              {notice.length > 0 ? <p role="status">{notice}</p> : null}
              {formError.length > 0 ? <p role="alert">{formError}</p> : null}
            </section>
          </>
        ) : null}
      </main>
    </div>
  );
}

import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import {
  ApiError,
  getDoctor,
  getOverview,
  getThread,
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

const maximumBodyLength = 16 * 1024;

interface Draft {
  to: string;
  subject: string;
  body: string;
  replyTo: string;
}

const emptyDraft: Draft = { to: "", subject: "", body: "", replyTo: "" };

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

function statusKey(status: DoctorStatus): `operator.${"statusOk" | "statusNotConfigured" | "statusNeedsHuman" | "statusSilent"}` {
  switch (status) {
    case "ok":
      return "operator.statusOk";
    case "not_configured":
      return "operator.statusNotConfigured";
    case "needs_human":
      return "operator.statusNeedsHuman";
    case "silent":
      return "operator.statusSilent";
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
  const [threadsError, setThreadsError] = useState(false);
  const [selectedId, setSelectedId] = useState<string | null>(null);
  const [thread, setThread] = useState<ThreadDetail | null>(null);
  const [compose, setCompose] = useState<Draft>(emptyDraft);
  const [reply, setReply] = useState<Draft>(emptyDraft);
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

  useEffect(() => {
    let ignore = false;
    let request = 0;
    const load = async () => {
      const current = ++request;
      try {
        const [nextOverview, nextDoctor] = await Promise.all([getOverview(), getDoctor()]);
        if (ignore || current !== request) {
          return;
        }
        setOverview(nextOverview);
        setDoctor(nextDoctor);
        setPhase("ready");
      } catch (error) {
        if (ignore || current !== request || isAbort(error)) {
          return;
        }
        setPhase(error instanceof ApiError && error.status === 401 ? "unauthorized" : "error");
      }
    };
    void load();
    const stop = connect((frame: WatchFrame) => {
      if (frame.kind.length === 0) {
        return;
      }
      void load();
      setThreadRevision((value) => value + 1);
    }, setStream);
    return () => {
      ignore = true;
      stop();
    };
  }, [connect, revision]);

  useEffect(() => {
    if (phase !== "ready") {
      return;
    }
    let ignore = false;
    void listThreads({
      repository_id: repositoryId,
      agent,
      q: query,
      limit: 50,
    })
      .then((page) => {
        if (!ignore) {
          setThreads(page.threads);
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
  }, [agent, phase, query, repositoryId, threadRevision]);

  useEffect(() => {
    if (phase !== "ready" || selectedId === null) {
      return;
    }
    let ignore = false;
    void getThread(selectedId)
      .then((detail) => {
        if (ignore) {
          return;
        }
        setThread(detail);
        const last = detail.messages.at(-1);
        if (last !== undefined) {
          setReply({
            to: last.sender,
            subject: last.subject.startsWith("Re:") ? last.subject : `Re: ${last.subject}`,
            body: "",
            replyTo: last.id,
          });
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
  }, [phase, selectedId, t, threadRevision]);

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

  const submit = async (draft: Draft, clear: () => void) => {
    setFormError("");
    setNotice("");
    try {
      await sendMessage({
        key: humanKey(),
        to: recipientsOf(draft.to),
        subject: draft.subject,
        body: draft.body,
        reply_to: draft.replyTo,
      });
      clear();
      setNotice(t("operator.sent"));
      setThreadRevision((value) => value + 1);
      setRevision((value) => value + 1);
    } catch (error) {
      if (error instanceof ApiError && error.status === 401) {
        setPhase("unauthorized");
        return;
      }
      setFormError(t("operator.loadError"));
    }
  };

  const onRevoke = async (claim: Claim) => {
    setRevoking(claim.fence);
    setFormError("");
    try {
      await revokeClaim(claim.fence);
      setRevision((value) => value + 1);
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
              {t("operator.doctorRefresh")}
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
                <button type="button" className="primary-button" onClick={() => setRevision((value) => value + 1)}>
                  {t("operator.doctorRefresh")}
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
                          {client.status === "silent" ? <span>{t("operator.cause")}: </span> : null}
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
                      void submit(reply, () => setReply((current) => ({ ...current, body: "" })));
                    }}
                  >
                    <h3>{t("operator.reply")}</h3>
                    <label>
                      {t("operator.recipients")}
                      <input value={reply.to} onChange={(event) => setReply({ ...reply, to: event.target.value })} />
                    </label>
                    <label>
                      {t("operator.subject")}
                      <input value={reply.subject} onChange={(event) => setReply({ ...reply, subject: event.target.value })} />
                    </label>
                    <label>
                      {t("operator.body")}
                      <textarea value={reply.body} onChange={(event) => setReply({ ...reply, body: event.target.value })} />
                    </label>
                    <p>{t("markdown.characterCount", { count: reply.body.length, maximum: maximumBodyLength })}</p>
                    <button
                      type="submit"
                      className="primary-button"
                      disabled={reply.to.length === 0 || reply.subject.length === 0 || reply.body.length === 0 || reply.body.length > maximumBodyLength}
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
                  void submit(compose, () => setCompose(emptyDraft));
                }}
              >
                <label>
                  {t("operator.recipients")}
                  <input value={compose.to} onChange={(event) => setCompose({ ...compose, to: event.target.value })} />
                </label>
                <label>
                  {t("operator.subject")}
                  <input value={compose.subject} onChange={(event) => setCompose({ ...compose, subject: event.target.value })} />
                </label>
                <label>
                  {t("operator.body")}
                  <textarea value={compose.body} onChange={(event) => setCompose({ ...compose, body: event.target.value })} />
                </label>
                <p>{t("markdown.characterCount", { count: compose.body.length, maximum: maximumBodyLength })}</p>
                <button
                  type="submit"
                  className="primary-button"
                  disabled={compose.to.length === 0 || compose.subject.length === 0 || compose.body.length === 0 || compose.body.length > maximumBodyLength}
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

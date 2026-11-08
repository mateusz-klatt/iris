import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import { ApiError, getDeliveryProblems, type DeliveryProblems as Problems } from "./api";

interface Props {
  /** The repository being viewed, or "" for the whole hub. */
  repositoryId: string;
  /** Changes whenever the hub reports a change, so the list is read again. */
  revision: number;
  onOpenThread: (id: string) => void;
  onUnauthorized: () => void;
}

/**
 * The human's own messages whose delivery is in trouble, as the hub judges them by the
 * thresholds it returns. Nothing here sends anything: a stored message stays queued for its
 * recipient, and sending it again would only make a second copy.
 */
export function DeliveryProblems({ repositoryId, revision, onOpenThread, onUnauthorized }: Props) {
  const { t, i18n } = useTranslation();
  const [problems, setProblems] = useState<Problems | null>(null);
  const [failed, setFailed] = useState(false);

  useEffect(() => {
    const controller = new AbortController();
    getDeliveryProblems(repositoryId, controller.signal).then(
      (value) => {
        setProblems(value);
        setFailed(false);
      },
      (error: unknown) => {
        if (controller.signal.aborted) {
          return;
        }
        if (error instanceof ApiError && error.status === 401) {
          onUnauthorized();
          return;
        }
        setFailed(true);
      },
    );
    return () => controller.abort();
  }, [repositoryId, revision, onUnauthorized]);

  if (failed) {
    return (
      <section id="delivery-problems" className="panel" aria-labelledby="delivery-problems-title">
        <div className="panel-heading">
          <h2 id="delivery-problems-title">{t("operator.deliveryTitle")}</h2>
        </div>
        <p role="alert">{t("operator.deliveryLoadError")}</p>
      </section>
    );
  }
  if (problems === null || problems.messages.length === 0) {
    return null;
  }
  const days = Math.round(problems.window_s / 86400);
  const minutes = Math.round(problems.wait_s / 60);
  const when = new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" });
  return (
    <section id="delivery-problems" className="panel" aria-labelledby="delivery-problems-title">
      <div className="panel-heading">
        <h2 id="delivery-problems-title">{t("operator.deliveryTitle")}</h2>
      </div>
      <p>{t("operator.deliveryHint", { days, minutes })}</p>
      <ul className="message-list">
        {problems.messages.map((message) => (
          <li key={message.id}>
            <button type="button" className="message-row" onClick={() => onOpenThread(message.thread_id)}>
              <strong>{message.subject}</strong>
              <small>
                <time dateTime={message.created_at}>{when.format(new Date(message.created_at))}</time>
              </small>
              <span className="sr-only">{t("operator.openThread", { subject: message.subject })}</span>
            </button>
            <ul>
              {message.recipients.map((recipient) => (
                <li key={recipient.name}>
                  {recipient.reason === "unreachable"
                    ? t("operator.deliveryUnreachable", { name: recipient.name, session: t(`operator.deliverySession.${sessionKey(recipient.execution)}`) })
                    : t("operator.deliveryWaiting", { name: recipient.name, minutes })}
                </li>
              ))}
            </ul>
          </li>
        ))}
      </ul>
    </section>
  );
}

function sessionKey(state: string): "lost" | "ended" | "none" {
  return state === "lost" || state === "ended" ? state : "none";
}

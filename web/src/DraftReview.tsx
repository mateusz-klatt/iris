import { useEffect, useId, useRef, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";

import { MessageBody } from "./MessageBody";
import type { Review } from "./review";


/**
 * DraftBody is the message field with an Edit and a Preview view. The preview is the same safe
 * renderer the thread uses, so the operator sees what the recipient's console will show.
 * Ctrl+Enter or Command+Enter submits the form, which opens the review.
 */
export function DraftBody({ value, onChange }: { value: string; onChange: (value: string) => void }) {
  const { t } = useTranslation();
  const [previewing, setPreviewing] = useState(false);
  const hint = useId();
  const onKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && (event.ctrlKey || event.metaKey)) {
      event.preventDefault();
      event.currentTarget.form?.requestSubmit();
    }
  };
  return (
    <div className="draft-body">
      <div className="draft-mode" role="group" aria-label={t("markdown.mode")}>
        <button type="button" aria-pressed={!previewing} onClick={() => setPreviewing(false)}>
          {t("markdown.edit")}
        </button>
        <button type="button" aria-pressed={previewing} onClick={() => setPreviewing(true)}>
          {t("markdown.preview")}
        </button>
      </div>
      {previewing ? (
        <div className="draft-preview" role="region" aria-label={t("markdown.preview")}>
          {value.trim().length > 0 ? <MessageBody body={value} /> : <p>{t("markdown.emptyPreview")}</p>}
        </div>
      ) : (
        <label>
          {t("operator.body")}
          <textarea value={value} aria-describedby={hint} onChange={(event) => onChange(event.target.value)} onKeyDown={onKeyDown} />
        </label>
      )}
      <small id={hint}>{t("markdown.shortcutHint")}</small>
    </div>
  );
}

interface DraftReviewProps {
  review: Review;
  recipients: string[];
  subject: string;
  body: string;
  /** Whether the hub can take the draft as it is; the caller keeps the byte limits. */
  sendable: boolean;
  sending: boolean;
  sendLabel: string;
  onSend: () => void;
}

/**
 * DraftReview is the form's last step. Closed, it is the submit button that opens the review.
 * Open, it shows each recipient, the subject and the rendered body, and only then offers Send.
 */
export function DraftReview({ review, recipients, subject, body, sendable, sending, sendLabel, onSend }: DraftReviewProps) {
  const { t } = useTranslation();
  const send = useRef<HTMLButtonElement>(null);
  const title = useId();
  useEffect(() => {
    if (review.open) {
      send.current?.focus();
    }
  }, [review.open]);
  if (!review.open) {
    return (
      <button type="submit" className="primary-button" disabled={sending || !sendable}>
        {t("operator.review")}
      </button>
    );
  }
  return (
    <section className="draft-review" aria-labelledby={title}>
      <h4 id={title}>{t("operator.reviewTitle")}</h4>
      <dl>
        <dt>{t("operator.recipients")}</dt>
        <dd>
          <ul>
            {recipients.map((name) => (
              <li key={name}>{name}</li>
            ))}
          </ul>
        </dd>
        <dt>{t("operator.subject")}</dt>
        <dd>{subject}</dd>
      </dl>
      <MessageBody body={body} />
      <div className="draft-review-actions">
        <button ref={send} type="button" className="primary-button" disabled={sending || !sendable} onClick={onSend}>
          {sendLabel}
        </button>
        <button type="button" onClick={review.stop}>
          {t("operator.reviewBack")}
        </button>
      </div>
    </section>
  );
}

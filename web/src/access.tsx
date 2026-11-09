// Account list and conflict presentation adapted from the owner's UI export, ADR 014.
import { useEffect, useRef, useState, type FormEvent, type RefObject } from "react";
import { useTranslation } from "react-i18next";

import { ApiError, type Repository } from "./api";
import { getSession, listAccounts, updateAccess, type AccountSession, type ConsoleAccessAccount, type ConsoleGrant } from "./accountApi";
import "./access.css";

interface AccessProps {
  session: AccountSession;
  repositories: Repository[];
  onSignedOut: () => void;
}

type AccessInput = Parameters<typeof updateAccess>[1];

// Names are not unique and can be empty, so a grant is always offered against the id.
function repositoryLabel(repository: Repository): string {
  return repository.name === "" ? repository.id : `${repository.name} · ${repository.id}`;
}

function grantKey(grants: ConsoleGrant[]): string {
  return grants.map((grant) => `${grant.repository_id}:${grant.mode}`).sort().join();
}

// A write from a tab whose browser session was replaced is refused as 403 for its stale CSRF
// token, where a read gets 401. Ask, so the operator is not told they lack access.
async function sessionReplaced(session: AccountSession): Promise<boolean> {
  try {
    await getSession(undefined, session);
    return false;
  } catch (error) {
    return error instanceof ApiError && error.status === 401;
  }
}

export default function AccessPanel(props: AccessProps) {
  const { t } = useTranslation();
  if (props.session.account.role !== "operator" || props.session.account.disabled) {
    return <p role="alert">{t("access.forbidden")}</p>;
  }
  return <AccessEditor {...props} />;
}

function AccessEditor({ session, repositories, onSignedOut }: AccessProps) {
  const { t } = useTranslation();
  const [accounts, setAccounts] = useState<ConsoleAccessAccount[]>([]);
  const [selectedID, setSelectedID] = useState("");
  const [refresh, setRefresh] = useState(0);
  const [loading, setLoading] = useState(true);
  const [saving, setSaving] = useState(false);
  const [failure, setFailure] = useState("");
  const [status, setStatus] = useState("");
  const mounted = useRef(true);
  // Saving and reloading disable or replace the control that was used, and selecting a person
  // can leave the form out of view. Each ends with focus on the form's heading. The request is
  // state so that it always renders: a ref set for an unchanged selection would fire later.
  const heading = useRef<HTMLHeadingElement>(null);
  const [focusRequest, setFocusRequest] = useState(0);
  const focusHandled = useRef(0);
  const requestFocus = () => setFocusRequest((value) => value + 1);

  useEffect(() => {
    mounted.current = true;
    return () => { mounted.current = false; };
  }, []);

  useEffect(() => {
    if (focusRequest !== focusHandled.current && !loading && !saving) {
      focusHandled.current = focusRequest;
      heading.current?.focus();
    }
  }, [focusRequest, loading, saving]);

  useEffect(() => {
    const controller = new AbortController();
    setLoading(true);
    setFailure("");
    listAccounts(controller.signal).then((latest) => {
      if (!controller.signal.aborted) {
        setAccounts(latest);
        setSelectedID((current) => latest.some((account) => account.id === current) ? current : latest[0]?.id ?? "");
      }
    }).catch((error: unknown) => {
      if (!controller.signal.aborted) {
        setAccounts([]);
        setStatus("");
        setFailure(error instanceof ApiError && error.status === 403 ? "access.forbidden" : "admin.loadError");
        if (error instanceof ApiError && error.status === 401) onSignedOut();
      }
    }).finally(() => {
      if (!controller.signal.aborted) setLoading(false);
    });
    return () => controller.abort();
  }, [refresh, onSignedOut]);

  const save = async (account: ConsoleAccessAccount, input: AccessInput) => {
    requestFocus();
    setSaving(true);
    setStatus("");
    setFailure("");
    try {
      const updated = await updateAccess(account.id, input);
      if (!mounted.current) return;
      if (account.id === session.account.id) {
        onSignedOut();
      } else {
        setAccounts((current) => current.map((item) => item.id === updated.id ? updated : item));
        setStatus("access.saved");
      }
    } catch (error) {
      if (!mounted.current) return;
      if (error instanceof ApiError && error.status === 401) {
        onSignedOut();
      } else if (error instanceof ApiError && error.status === 409) {
        setLoading(true);
        setStatus("access.conflict");
        setRefresh((value) => value + 1);
      } else if (error instanceof ApiError && error.status === 403) {
        const replaced = await sessionReplaced(session);
        if (!mounted.current) return;
        if (replaced) onSignedOut();
        else setFailure("access.forbidden");
      } else {
        setFailure("admin.saveError");
      }
    } finally {
      if (mounted.current) setSaving(false);
    }
  };
  const selected = accounts.find((account) => account.id === selectedID);
  const lastOperator = selected?.role === "operator" && !selected.disabled
    && accounts.filter((account) => account.role === "operator" && !account.disabled).length === 1;

  return <section id="iris-access-panel" aria-labelledby="iris-access-heading">
    <div className="page-heading"><div>
      <p className="eyebrow">{t("admin.eyebrow")}</p>
      <h1 id="iris-access-heading">{t("access.title")}</h1>
      <p>{t("access.hint")}</p>
    </div>
      <button type="button" className="secondary-button" disabled={loading || saving} onClick={() => { requestFocus(); setLoading(true); setStatus(""); setRefresh((value) => value + 1); }}>{t("access.reload")}</button>
    </div>
    {loading ? <p role="status">{t("admin.loading")}</p> : null}
    {failure !== "" ? <p role="alert">{t(failure)}</p> : null}
    <p role="status">{status === "" || loading ? "" : t(status)}</p>
    {!loading && failure !== "access.forbidden" ? <div className="iris-access-layout">
      <section className="settings-card" aria-labelledby="iris-access-people">
        <h2 id="iris-access-people">{t("admin.usersTitle")}</h2>
        {accounts.length === 0 ? <p>{t("admin.usersEmpty")}</p> : <ul className="iris-access-people">
          {accounts.map((account) => <li key={account.id}><button type="button" aria-pressed={selectedID === account.id} disabled={saving}
            onClick={() => { requestFocus(); setSelectedID(account.id); setStatus(""); setFailure(""); }}>
            <strong>{account.username}</strong>
            {account.display_name !== "" ? <small>{account.display_name}</small> : null}
            <span>{t(`accountUi.${account.role}`)} · {t(account.disabled ? "admin.disabled" : "admin.active")}</span>
          </button></li>)}
        </ul>}
      </section>
      {selected === undefined ? null : <AccessForm key={`${selected.id}:${selected.edit_revision}`} account={selected} repositories={repositories} lastOperator={lastOperator}
        saving={saving} heading={heading} onSave={(input) => { void save(selected, input); }} />}
    </div> : null}
  </section>;
}

function AccessForm({ account, repositories, lastOperator, saving, heading, onSave }: {
  account: ConsoleAccessAccount;
  repositories: Repository[];
  lastOperator: boolean;
  saving: boolean;
  heading: RefObject<HTMLHeadingElement | null>;
  onSave: (input: AccessInput) => void;
}) {
  const { t } = useTranslation();
  const [role, setRole] = useState(account.role);
  const [disabled, setDisabled] = useState(account.disabled);
  const [grants, setGrants] = useState(account.grants);
  // Grants stay in state while the role changes, so going back to member restores them.
  const savedGrants = role === "member" ? grants : [];
  // Every accepted edit ends the account's sessions, so an unchanged form cannot be saved.
  const changed = role !== account.role || disabled !== account.disabled || grantKey(savedGrants) !== grantKey(account.grants);

  const submit = (event: FormEvent) => {
    event.preventDefault();
    onSave({ edit_revision: account.edit_revision, role, disabled, grants: savedGrants });
  };

  return <section className="settings-card" aria-labelledby="iris-access-selected">
    <h2 id="iris-access-selected" ref={heading} tabIndex={-1}>{t("admin.selectedUser", { user: account.username })}</h2>
    <form className="settings-form" onSubmit={submit}>
      <label htmlFor="iris-access-role">{t("access.role")}</label>
      <select id="iris-access-role" value={role} disabled={saving || lastOperator} onChange={(event) => {
        setRole(event.target.value);
        if (event.target.value === "none") setDisabled(true);
      }}>
        <option value="operator">{t("accountUi.operator")}</option>
        <option value="member">{t("accountUi.member")}</option>
        <option value="none">{t("accountUi.none")}</option>
      </select>
      <label className="iris-access-disabled"><input type="checkbox" checked={disabled} disabled={saving || lastOperator || role === "none"}
        onChange={(event) => setDisabled(event.target.checked)} />{t("admin.disabled")}</label>
      {lastOperator ? <p>{t("access.lastOperator")}</p> : null}
      {role === "operator" ? <p>{t("access.operatorHint")}</p> : null}
      {role === "member" ? <fieldset disabled={saving} className="iris-access-grants"><legend>{t("operator.repositories")}</legend>
        {repositories.length === 0 ? <p>{t("operator.none")}</p> : repositories.map((repository) => <label key={repository.id}>
          <span>{repositoryLabel(repository)}</span>
          <select aria-label={t("access.assignment", { repository: repositoryLabel(repository) })} value={grants.find((grant) => grant.repository_id === repository.id)?.mode ?? ""}
            onChange={(event) => {
              const remaining = grants.filter((grant) => grant.repository_id !== repository.id);
              const mode = event.target.value;
              setGrants(mode === "" ? remaining : [...remaining, { repository_id: repository.id, mode: mode as "ro" | "rw" }]);
            }}>
            <option value="">{t("access.noGrant")}</option>
            <option value="ro">{t("access.ro")}</option>
            <option value="rw">{t("access.rw")}</option>
          </select>
        </label>)}
      </fieldset> : null}
      <button type="submit" className="primary-button iris-access-save" aria-busy={saving} disabled={saving || !changed}>{t(saving ? "access.saving" : "access.save")}</button>
    </form>
  </section>;
}

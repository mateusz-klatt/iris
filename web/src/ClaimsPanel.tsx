import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";

import type { TFunction } from "i18next";

import type { Claim, Repository } from "./api";
import { claimAutoScopeText, claimEffectiveText, claimLocation } from "./claimScope";
import { second, timeLeft } from "./timeLeft";

// The claims panel: every live claim, narrowed by repository, holder and path, each with the
// time left on it. Rows stack, so a long path and holder fit a phone without a table to scroll.

// Expiry shows the time left on a claim, "Expired" after it, and a time it cannot read as given.
function Expiry({ at, now, locale }: { at: string; now: number; locale: string }) {
  const { t } = useTranslation();
  const left = timeLeft(at, now, locale);
  if (left === undefined) {
    return <span className="claim-left">{at}</span>;
  }
  return (
    <time className="claim-left" dateTime={at} title={new Date(at).toLocaleString(locale)} data-expired={left === null ? "yes" : "no"}>
      {left ?? t("operator.claimExpired")}
    </time>
  );
}

// options lists the distinct values present in the order of their labels, keeping the chosen
// one even after its last claim ends, so the filter still says what it filters by.
function options(values: string[], chosen: string, label: (value: string) => string = (value) => value): string[] {
  const set = new Set(values);
  if (chosen !== "") {
    set.add(chosen);
  }
  return [...set].sort((a, b) => label(a).localeCompare(label(b)));
}

// ClaimScopeLine names one repository's claim.auto_scope. The worktree value says
// what that opt-in cannot do. There is no control here: only the owner token sets it.
export function ClaimScopeLine({ name, scope }: { name: string; scope?: string }) {
  const { t } = useTranslation();
  const label = claimAutoScopeText(scope, t);
  return (
    <p className="claim-scope">
      {name === "" ? label : `${name}: ${label}`}
      {scope === "worktree" ? <small>{` ${t("operator.claimAutoScopeWorktreeLimit")}`}</small> : null}
    </p>
  );
}

function projectScope(repository: Repository): boolean {
  return repository.kind !== "outside";
}

function claimDetail(claim: Claim, claims: Claim[], nameOf: (id: string) => string, t: TFunction): string {
  return `${nameOf(claim.repository_id)} · ${claim.holder} · ${claim.kind} · ${claimLocation(claim, claims, t)} · ${claimEffectiveText(claim.effective_scope, t)}`;
}

export function ClaimsPanel({
  claims,
  repositories,
  revoking,
  onRevoke,
  canRevoke = true,
}: {
  claims: Claim[];
  repositories: Repository[];
  revoking: number | null;
  onRevoke: (claim: Claim) => void;
  canRevoke?: boolean;
}) {
  const { t, i18n } = useTranslation();
  const [repository, setRepository] = useState("");
  const [holder, setHolder] = useState("");
  const [path, setPath] = useState("");
  const [now, setNow] = useState(() => Date.now());
  const counting = claims.some((claim) => claim.expires_at !== null);

  useEffect(() => {
    if (!counting) {
      return undefined;
    }
    const timer = window.setInterval(() => setNow(Date.now()), second);
    return () => window.clearInterval(timer);
  }, [counting]);

  const nameOf = (id: string) => repositories.find((item) => item.id === id)?.name ?? id;
  const projects = repositories.filter(projectScope);
  const needle = path.trim().toLowerCase();
  const shown = claims.filter(
    (claim) =>
      (repository === "" || claim.repository_id === repository) &&
      (holder === "" || claim.holder === holder) &&
      (needle === "" || claim.path.toLowerCase().includes(needle)),
  );

  return (
    <article className="panel claims-panel">
      <div className="panel-heading">
        <h2>{t("operator.claims")}</h2>
      </div>
      {projects.length === 0 ? null : (
        <div aria-label={t("operator.claimAutoScope")}>
          {projects.map((repository) => (
            <ClaimScopeLine key={repository.id} name={repository.name === "" ? repository.id : repository.name} scope={repository.claim_auto_scope} />
          ))}
        </div>
      )}
      {claims.length === 0 ? (
        <p>{t("operator.none")}</p>
      ) : (
        <>
          <div className="claim-filters" role="group" aria-label={t("operator.claimFilters")}>
            <label>
              {t("operator.repository")}
              <select id="iris-claims-repository" name="claims_repository" value={repository} onChange={(event) => setRepository(event.target.value)}>
                <option value="">{t("operator.all")}</option>
                {options(
                  claims.map((claim) => claim.repository_id),
                  repository,
                  nameOf,
                ).map((id) => (
                  <option key={id} value={id}>
                    {nameOf(id)}
                  </option>
                ))}
              </select>
            </label>
            <label>
              {t("reservations.holder")}
              <select id="iris-claims-holder" name="claims_holder" value={holder} onChange={(event) => setHolder(event.target.value)}>
                <option value="">{t("operator.all")}</option>
                {options(
                  claims.map((claim) => claim.holder),
                  holder,
                ).map((name) => (
                  <option key={name} value={name}>
                    {name}
                  </option>
                ))}
              </select>
            </label>
            <label>
              {t("reservations.path")}
              <input id="iris-claims-path" name="claims_path" type="search" value={path} onChange={(event) => setPath(event.target.value)} />
            </label>
          </div>
          {shown.length === 0 ? (
            <p role="status">{t("operator.noClaimsMatch")}</p>
          ) : (
            <ul className="claim-list">
              {shown.map((claim) => (
                <li key={claim.fence} className="claim-row" data-fence={claim.fence}>
                  <div>
                    <strong>{claim.path}</strong>
                    <small>
                      {claimDetail(claim, claims, nameOf, t)}
                    </small>
                  </div>
                  {claim.expires_at === null ? (
                    <span className="claim-left">{t("operator.none")}</span>
                  ) : (
                    <Expiry at={claim.expires_at} now={now} locale={i18n.language} />
                  )}
                  {canRevoke ? <button type="button" className="primary-button" disabled={revoking === claim.fence} onClick={() => onRevoke(claim)}>
                    {revoking === claim.fence ? t("operator.revoking") : t("operator.revoke")}
                  </button> : null}
                </li>
              ))}
            </ul>
          )}
        </>
      )}
    </article>
  );
}

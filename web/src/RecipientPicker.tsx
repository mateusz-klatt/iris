import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import type { Agent } from "./api";

export function RecipientPicker({ agents, selected, onChange, allowHuman = true, scopes = {} }: {
  agents: Agent[];
  selected: string[];
  onChange: (ids: string[]) => void;
  allowHuman?: boolean;
  /** Labels by repository id for agents whose record is not a project, such as a host's context
   * of sessions outside git (ADR 034). */
  scopes?: Record<string, string>;
}) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const [inactive, setInactive] = useState(false);
  const pickerId = useId();
  const search = `${pickerId}-search`;
  // Inactive agents are listed on request; an agent already chosen stays listed either way.
  const listed = agents.filter((agent) => inactive || agent.state === "active" || selected.includes(agent.id));
  const choices = [...(allowHuman ? [{ id: "human", name: "human", client: "human", host: "", repository_id: "" }] : []), ...listed];
  const matches = choices.filter((agent) => `${agent.name} ${agent.client} ${agent.host}`.toLowerCase().includes(query.toLowerCase().trim()));
  const missing = selected.filter((id) => !choices.some((agent) => agent.id === id));
  return <fieldset className="recipient-picker">
    <legend>{t("operator.recipients")}</legend>
    <label htmlFor={search}>{t("compose.recipientSearch")}</label>
    <input id={search} name="recipient_search" type="search" value={query} onChange={(event) => setQuery(event.target.value)} />
    <label><input id={`${pickerId}-inactive`} name="recipient_show_inactive" type="checkbox" checked={inactive} onChange={(event) => setInactive(event.target.checked)} />{t("console.showInactive")}</label>
    <div className="recipient-options">
      {matches.map((agent) => <label key={agent.id} className="recipient-option">
        <input id={`${pickerId}-option-${agent.id}`} name="recipients" type="checkbox" checked={selected.includes(agent.id)} onChange={(event) => onChange(event.target.checked ? [...selected, agent.id] : selected.filter((id) => id !== agent.id))} />
        <span>{agent.name}<small>{scopes[agent.repository_id] === undefined ? agent.id : `${scopes[agent.repository_id]} · ${agent.id}`}</small></span>
      </label>)}
    </div>
    {matches.length === 0 ? <p>{t("compose.noRecipientResults")}</p> : null}
    {missing.length > 0 ? <p role="alert">{t("compose.directory.refreshed")} {t("compose.clearRecipients")}: {missing.join(", ")}</p> : null}
    {selected.length > 0 ? <button type="button" onClick={() => onChange([])}>{t("compose.clearRecipients")}</button> : null}
  </fieldset>;
}

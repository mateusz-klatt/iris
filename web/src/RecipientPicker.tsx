import { useId, useState } from "react";
import { useTranslation } from "react-i18next";
import type { Agent } from "./api";

export function RecipientPicker({ agents, selected, onChange }: {
  agents: Agent[];
  selected: string[];
  onChange: (ids: string[]) => void;
}) {
  const { t } = useTranslation();
  const [query, setQuery] = useState("");
  const search = useId();
  const choices = [{ id: "human", name: "human", client: "human", host: "" }, ...agents];
  const matches = choices.filter((agent) => `${agent.name} ${agent.client} ${agent.host}`.toLowerCase().includes(query.toLowerCase().trim()));
  const missing = selected.filter((id) => !choices.some((agent) => agent.id === id));
  return <fieldset className="recipient-picker">
    <legend>{t("operator.recipients")}</legend>
    <label htmlFor={search}>{t("compose.recipientSearch")}</label>
    <input id={search} type="search" value={query} onChange={(event) => setQuery(event.target.value)} />
    <div className="recipient-options">
      {matches.map((agent) => <label key={agent.id} className="recipient-option">
        <input type="checkbox" checked={selected.includes(agent.id)} onChange={(event) => onChange(event.target.checked ? [...selected, agent.id] : selected.filter((id) => id !== agent.id))} />
        <span>{agent.name}<small>{agent.id}</small></span>
      </label>)}
    </div>
    {matches.length === 0 ? <p>{t("compose.noRecipientResults")}</p> : null}
    {missing.length > 0 ? <p role="alert">{t("compose.directory.refreshed")} {t("compose.clearRecipients")}: {missing.join(", ")}</p> : null}
    {selected.length > 0 ? <button type="button" onClick={() => onChange([])}>{t("compose.clearRecipients")}</button> : null}
  </fieldset>;
}

import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";

import { ApiError } from "./api";
import { ConsoleClosedError, login, type AccountSession } from "./accountApi";
import LocalePicker from "./LocalePicker";
import { loadLocale, type SupportedLocale } from "./i18n";
import { readUiLocale, writeUiLocale } from "./preferences";
import "./account.css";

export default function Login({ onLogin }: { onLogin: (session: AccountSession) => void }) {
  const { t } = useTranslation();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState("");
  const [locale, setLocale] = useState<SupportedLocale>(() => readUiLocale() ?? "en");
  const submitting = useRef(false);

  useEffect(() => {
    void loadLocale(locale).catch(() => setError("localeStatus.saveError"));
  }, [locale]);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (submitting.current) {
      return;
    }
    submitting.current = true;
    setSaving(true);
    setError("");
    try {
      onLogin(await login(username, password));
    } catch (failure) {
      setPassword("");
      setError(failure instanceof ConsoleClosedError ? "accountUi.consoleClosed" : failure instanceof ApiError && failure.status === 401
        ? "accountUi.invalidLogin"
        : failure instanceof ApiError && failure.status === 429
          ? "accountUi.rateLimited"
          : "accountUi.loginError");
    } finally {
      submitting.current = false;
      setSaving(false);
    }
  };

  return <section className="iris-login settings-card" aria-labelledby="iris-login-heading">
    <LocalePicker disabled={saving} locale={locale} onSelect={(next) => { writeUiLocale(next); setLocale(next); }} />
    <h1 id="iris-login-heading">{t("accountUi.loginTitle")}</h1>
    <form className="settings-form" onSubmit={(event) => void submit(event)}>
      <label htmlFor="iris-username">{t("account.username")}</label>
      <input id="iris-username" name="username" autoComplete="username" required value={username}
        onChange={(event) => setUsername(event.target.value)} disabled={saving} />
      <label htmlFor="iris-password">{t("accountUi.password")}</label>
      <input id="iris-password" name="password" type="password" autoComplete="current-password" required value={password}
        onChange={(event) => setPassword(event.target.value)} disabled={saving} />
      <button className="primary-button" type="submit" disabled={saving}>
        {t(saving ? "accountUi.signingIn" : "accountUi.signIn")}
      </button>
      {error !== "" ? <p role="alert">{t(error)}</p> : null}
    </form>
  </section>;
}

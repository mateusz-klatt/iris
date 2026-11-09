// Adapted from the owner's UI export: account forms and feedback, ADR 014 inventory.
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";

import { ApiError } from "./api";
import { changePassword, getSession, updateProfile, validNewPassword, type AccountSession } from "./accountApi";
import { localeMetadata, supportedLocales } from "./i18n";
import "./account.css";

interface AccountProps {
  session: AccountSession;
  onSession: (session: AccountSession) => void;
  onSignedOut: (passwordChanged?: boolean) => void;
}

export default function Account({ session, onSession, onSignedOut }: AccountProps) {
  const { t } = useTranslation();
  const account = session.account;
  const [displayName, setDisplayName] = useState(account.display_name);
  const [profileStatus, setProfileStatus] = useState("");
  const [profileSaving, setProfileSaving] = useState(false);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [passwordStatus, setPasswordStatus] = useState("");
  const [passwordSaving, setPasswordSaving] = useState(false);

  const saveProfile = async (locale: string, editDisplayName = true) => {
    const requestedName = editDisplayName ? displayName : account.display_name;
    if (Array.from(requestedName).length > 128) {
      setProfileStatus("accountUi.displayNameInvalid");
      return;
    }
    setProfileSaving(true);
    setProfileStatus("");
    try {
      const updated = await updateProfile(account, requestedName, locale);
      if (updated.id !== account.id) {
        onSignedOut();
        return;
      }
      onSession({ ...session, account: updated });
      setProfileStatus(editDisplayName ? "account.displayNameSaved" : "localeStatus.saved");
    } catch (failure) {
      if (failure instanceof ApiError && failure.status === 401) {
        onSignedOut();
      } else if (failure instanceof ApiError && failure.status === 409) {
        try {
          const latest = await getSession();
          if (latest.account.id !== account.id) {
            onSignedOut();
            return;
          }
          onSession(latest);
          setDisplayName(latest.account.display_name);
          setProfileStatus("account.displayNameConflict");
        } catch (refreshFailure) {
          if (refreshFailure instanceof ApiError && refreshFailure.status === 401) {
            onSignedOut();
          } else {
            setProfileStatus("account.displayNameError");
          }
        }
      } else {
        setProfileStatus("account.displayNameError");
      }
    } finally {
      setProfileSaving(false);
    }
  };

  const savePassword = async (event: FormEvent) => {
    event.preventDefault();
    if (newPassword !== confirmation) {
      setPasswordStatus("account.passwordMismatch");
      return;
    }
    if (!validNewPassword(newPassword)) {
      setPasswordStatus("accountUi.passwordInvalid");
      return;
    }
    if (new TextEncoder().encode(currentPassword).byteLength > 1024) {
      setPasswordStatus("account.passwordError");
      return;
    }
    setPasswordSaving(true);
    setPasswordStatus("");
    try {
      await changePassword(currentPassword, newPassword);
      setCurrentPassword("");
      setNewPassword("");
      setConfirmation("");
      onSignedOut(true);
    } catch (failure) {
      if (failure instanceof ApiError && failure.status === 401) {
        try {
          const current = await getSession();
          if (current.account.id !== account.id) {
            onSignedOut();
          } else {
            setPasswordStatus("account.passwordError");
          }
        } catch {
          onSignedOut();
        }
      } else {
        setPasswordStatus(failure instanceof ApiError && failure.status === 429
          ? "account.passwordRateLimit" : "account.passwordError");
      }
    } finally {
      setPasswordSaving(false);
    }
  };

  return <section aria-labelledby="iris-account-heading">
    <div className="page-heading"><div>
      <p className="eyebrow">{t("account.eyebrow")}</p>
      <h1 id="iris-account-heading">{t("account.title")}</h1>
      <p>{t("account.hint")}</p>
    </div></div>
    <div className="settings-grid">
      <section className="settings-card" aria-labelledby="iris-identity-heading">
        <h2 id="iris-identity-heading">{t("account.identityTitle")}</h2>
        <dl className="account-facts">
          <div><dt>{t("account.username")}</dt><dd>{account.username}</dd></div>
          <div><dt>{t("account.globalRole")}</dt><dd>{t(`accountUi.${account.role}`)}</dd></div>
        </dl>
        <form className="settings-form" onSubmit={(event) => { event.preventDefault(); void saveProfile(account.ui_locale); }}>
          <label htmlFor="iris-display-name">{t("account.displayName")}</label>
          <input id="iris-display-name" name="display-name" autoComplete="name" value={displayName}
            onChange={(event) => { setDisplayName(event.target.value); setProfileStatus(""); }}
            aria-describedby="iris-display-hint iris-profile-status" disabled={profileSaving} />
          <small id="iris-display-hint">{t("account.displayNameHint")}</small>
          <button className="primary-button" type="submit" disabled={profileSaving}>
            {t(profileSaving ? "account.savingDisplayName" : "account.saveDisplayName")}
          </button>
          <p id="iris-profile-status" className="form-status" role="status">{profileStatus === "" ? "" : t(profileStatus)}</p>
        </form>
      </section>
      <section className="settings-card" aria-labelledby="iris-language-heading">
        <h2 id="iris-language-heading">{t("account.languagesTitle")}</h2>
        <p>{t("account.languagesHint")}</p>
        <div className="settings-form">
          <label htmlFor="iris-account-language">{t("account.uiLanguage")}</label>
          <select id="iris-account-language" value={account.ui_locale} onChange={(event) => void saveProfile(event.target.value, false)} disabled={profileSaving}>
            <option value="">{t("accountUi.browserLanguage")}</option>
            {supportedLocales.map((locale) => <option key={locale} value={locale}>{localeMetadata[locale].nativeName}</option>)}
          </select>
        </div>
      </section>
      <section className="settings-card" aria-labelledby="iris-password-heading">
        <h2 id="iris-password-heading">{t("account.passwordTitle")}</h2>
        <p>{t("account.passwordHint")}</p>
        <form className="settings-form" onSubmit={(event) => void savePassword(event)}>
          <label htmlFor="iris-current-password">{t("account.currentPassword")}</label>
          <input id="iris-current-password" name="current-password" type="password" autoComplete="current-password" required value={currentPassword}
            onChange={(event) => { setCurrentPassword(event.target.value); setPasswordStatus(""); }} disabled={passwordSaving} />
          <label htmlFor="iris-new-password">{t("account.newPassword")}</label>
          <input id="iris-new-password" name="new-password" type="password" autoComplete="new-password" required value={newPassword}
            onChange={(event) => { setNewPassword(event.target.value); setPasswordStatus(""); }} disabled={passwordSaving} />
          <label htmlFor="iris-confirm-password">{t("account.confirmPassword")}</label>
          <input id="iris-confirm-password" name="confirm-password" type="password" autoComplete="new-password" required value={confirmation}
            onChange={(event) => { setConfirmation(event.target.value); setPasswordStatus(""); }} disabled={passwordSaving} />
          <button className="primary-button" type="submit" disabled={passwordSaving}>{t(passwordSaving ? "account.changingPassword" : "account.changePassword")}</button>
          <p className="form-status" role="status">{passwordStatus === "" ? "" : t(passwordStatus)}</p>
        </form>
      </section>
    </div>
  </section>;
}

import { supportedLocales, type SupportedLocale } from "./i18n";

export function isSupportedLocale(value: unknown): value is SupportedLocale {
  return supportedLocales.some((locale) => locale === value);
}

export const uiLocalePreferenceKey = "iris.uiLocale";

export function clearUiLocale(): void {
  try {
    globalThis.localStorage.removeItem(uiLocalePreferenceKey);
  } catch {
    // Account language is also held only in memory when storage is unavailable.
  }
}

export function readUiLocale(
  storage: Pick<Storage, "getItem"> | undefined = globalThis.localStorage,
): SupportedLocale | null {
  try {
    const value = storage?.getItem(uiLocalePreferenceKey);
    return isSupportedLocale(value) ? value : null;
  } catch {
    return null;
  }
}

export function writeUiLocale(
  locale: SupportedLocale,
  storage: Pick<Storage, "setItem"> | undefined = globalThis.localStorage,
): void {
  try {
    storage?.setItem(uiLocalePreferenceKey, locale);
  } catch {
    // The choice still applies for this visit when storage is blocked.
  }
}

import { ApiError, setConsoleCsrf } from "./api";

export interface ConsoleAccount {
  id: string;
  username: string;
  role: string;
  disabled: boolean;
  display_name: string;
  ui_locale: string;
  correspondence_locale: string;
  edit_revision: number;
  auth_version?: number;
}

export interface AccountSession {
  account: ConsoleAccount;
  csrf: string;
  created_ms: number;
  expires_ms: number;
}

export class ConsoleClosedError extends ApiError {
  constructor() {
    super(401);
    this.name = "ConsoleClosedError";
  }
}

export interface ConsoleGrant {
  repository_id: string;
  mode: "ro" | "rw";
}

export interface ConsoleAccessAccount extends ConsoleAccount {
  grants: ConsoleGrant[];
}

export interface ConsoleAccessEdit {
  edit_revision: number;
  role: string;
  disabled: boolean;
  grants: ConsoleGrant[];
}

let csrf = "";
let sessionGeneration = 0;

function requireSessionGeneration(generation: number): void {
  if (generation !== sessionGeneration) {
    const failure = new Error("Iris session changed");
    failure.name = "AbortError";
    throw failure;
  }
}

export function validNewPassword(password: string): boolean {
  return Array.from(password).length >= 15 && new TextEncoder().encode(password).byteLength <= 1024;
}

export function clearAccountSession(): void {
  sessionGeneration++;
  csrf = "";
  setConsoleCsrf("");
}

async function request(path: string, method: string, body?: unknown, token = csrf, signal?: AbortSignal): Promise<Response> {
  const response = await fetch(new URL(path, window.location.origin), {
    method,
    cache: "no-store",
    credentials: "same-origin",
    signal,
    headers: method === "GET"
      ? { Accept: "application/json", ...(token !== "" ? { "X-Iris-CSRF": token } : {}) }
      : { Accept: "application/json", "Content-Type": "application/json", "X-Iris-CSRF": token },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 401 && response.headers.get("X-Iris-Console-Mode") === "closed") {
    throw new ConsoleClosedError();
  }
  if (!response.ok) {
    throw new ApiError(response.status);
  }
  return response;
}

export async function getSession(signal?: AbortSignal, expected?: AccountSession): Promise<AccountSession> {
  const generation = sessionGeneration;
  const session = await (await request("/v1/ui/session", "GET", undefined, expected?.csrf ?? "", signal)).json() as AccountSession;
  signal?.throwIfAborted();
  requireSessionGeneration(generation);
  if (expected !== undefined && (session.account.id !== expected.account.id || session.account.auth_version !== expected.account.auth_version || session.csrf !== expected.csrf)) {
    throw new ApiError(401);
  }
  csrf = session.csrf;
  setConsoleCsrf(csrf);
  return session;
}

export async function login(username: string, password: string): Promise<AccountSession> {
  const generation = sessionGeneration;
  // Each attempt consumes its own single-use browser challenge, including failures.
  const challenge = await (await request("/v1/ui/login", "GET")).json() as { csrf: string };
  requireSessionGeneration(generation);
  await request("/v1/ui/login", "POST", { username, password }, challenge.csrf);
  requireSessionGeneration(generation);
  return getSession();
}

export async function logout(): Promise<void> {
  const generation = sessionGeneration;
  await request("/v1/ui/logout", "POST", {});
  requireSessionGeneration(generation);
  clearAccountSession();
}

export async function updateProfile(account: ConsoleAccount, displayName: string, uiLocale: string): Promise<ConsoleAccount> {
  const result = await (await request("/v1/ui/profile", "PATCH", {
    edit_revision: account.edit_revision,
    display_name: displayName,
    ui_locale: uiLocale,
    correspondence_locale: account.correspondence_locale,
  })).json() as { account: ConsoleAccount };
  return result.account;
}

export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  const generation = sessionGeneration;
  await request("/v1/ui/password", "POST", { current_password: currentPassword, new_password: newPassword });
  requireSessionGeneration(generation);
  clearAccountSession();
}

async function accessJson<T>(path: string, method: string, body?: unknown, signal?: AbortSignal): Promise<T> {
  const generation = sessionGeneration;
  const value = await (await request(path, method, body, csrf, signal)).json() as T;
  signal?.throwIfAborted();
  requireSessionGeneration(generation);
  return value;
}

export async function listAccounts(signal?: AbortSignal): Promise<ConsoleAccessAccount[]> {
  const result = await accessJson<{ accounts: ConsoleAccessAccount[] }>("/v1/ui/accounts", "GET", undefined, signal);
  return result.accounts;
}

export async function updateAccess(accountID: string, edit: ConsoleAccessEdit): Promise<ConsoleAccessAccount> {
  const result = await accessJson<{ account: ConsoleAccessAccount }>(`/v1/ui/accounts/${encodeURIComponent(accountID)}/access`, "PUT", edit);
  return result.account;
}

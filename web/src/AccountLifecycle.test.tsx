import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import { ApiError, type OutgoingMessage, type WatchFrame, type WatchOptions, type WatchStatus } from "./api";
import { clearAccountSession, type AccountSession } from "./accountApi";
import { loadLocale } from "./i18n";
import { uiLocalePreferenceKey } from "./preferences";
import { server, sessionOverview } from "./test/server";

const accountSession: AccountSession = {
  csrf: "iris-lifecycle-csrf", created_ms: 1, expires_ms: 9999999999999,
  account: { id: "ca_lifecycle", username: "iris-person", role: "member", disabled: false, display_name: "Iris person", ui_locale: "en", correspondence_locale: "pl", edit_revision: 1, auth_version: 1 },
};

function installSession(mode: "ro" | "rw" = "ro") {
  const current = structuredClone(accountSession);
  server.use(
    http.get("*/v1/ui/session", () => HttpResponse.json(current)),
    http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, repositories: sessionOverview.repositories.map((repository) => ({ ...repository, console_access: mode })) })),
  );
  return current;
}

beforeEach(async () => {
  clearAccountSession();
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
    removeItem: (key: string) => values.delete(key),
  });
  await loadLocale("en");
});

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); server.events.removeAllListeners(); });

describe("named account lifecycle in the console", () => {
  it("keeps login visible when a revoked initial session lookup finishes late", async () => {
    let finish: () => void = () => {};
    const requested = vi.fn();
    const responseFinished = new Promise<void>((resolve) => {
      server.events.on("response:mocked", ({ request }) => { if (new URL(request.url).pathname === "/v1/ui/session") resolve(); });
    });
    server.use(http.get("*/v1/ui/session", async () => {
      requested();
      await new Promise<void>((resolve) => { finish = resolve; });
      return HttpResponse.json(accountSession);
    }));
    const connect = vi.fn(() => () => {});
    render(<App connect={connect} />);
    await waitFor(() => expect(requested).toHaveBeenCalledOnce());
    act(() => window.dispatchEvent(new Event("iris:unauthorized")));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    await act(async () => { finish(); await responseFinished; });
    expect(screen.getByLabelText("Username")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(connect).not.toHaveBeenCalled();
  });

  it("signs in through a browser challenge and returns to login after an explicit password change", async () => {
    let authenticated = false;
    server.use(
      http.get("*/v1/ui/session", () => authenticated ? HttpResponse.json(accountSession) : new HttpResponse(null, { status: 401 })),
      http.get("*/v1/ui/login", () => HttpResponse.json({ csrf: "iris-login-challenge" })),
      http.post("*/v1/ui/login", async ({ request }) => {
        expect(request.headers.get("X-Iris-CSRF")).toBe("iris-login-challenge");
        expect(await request.json()).toEqual({ username: "iris-person", password: "old password  " });
        authenticated = true;
        return HttpResponse.json({ authenticated: true });
      }),
      http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, repositories: sessionOverview.repositories.map((repository) => ({ ...repository, console_access: "ro" })) })),
      http.post("*/v1/ui/password", async ({ request }) => {
        expect(request.headers.get("X-Iris-CSRF")).toBe(accountSession.csrf);
        expect(await request.json()).toEqual({ current_password: "old password  ", new_password: "😀".repeat(15) });
        authenticated = false;
        return HttpResponse.json({ login_required: true });
      }),
    );
    const stopped = vi.fn();
    render(<App connect={() => stopped} />);
    fireEvent.change(await screen.findByLabelText("Username"), { target: { value: "iris-person" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "old password  " } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "Iris person" }));
    expect(screen.getByRole("heading", { name: "Account" })).toBeVisible();
    fireEvent.change(screen.getByLabelText("Current password"), { target: { value: "old password  " } });
    fireEvent.change(screen.getByLabelText("New password"), { target: { value: "😀".repeat(15) } });
    fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: "😀".repeat(15) } });
    fireEvent.click(screen.getByRole("button", { name: "Change password" }));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(screen.getByRole("status")).toHaveTextContent(/Password changed.*Sign in again/);
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(stopped).toHaveBeenCalledOnce();
  });

  it("saves a signed-in language with the account revision and leaves browser preferences untouched", async () => {
    const current = installSession();
    localStorage.setItem(uiLocalePreferenceKey, "de");
    server.use(http.patch("*/v1/ui/profile", async ({ request }) => {
      expect(request.headers.get("X-Iris-CSRF")).toBe(current.csrf);
      expect(await request.json()).toEqual({ edit_revision: 1, display_name: "Iris person", ui_locale: "pl", correspondence_locale: "pl" });
      current.account.ui_locale = "pl";
      current.account.edit_revision++;
      return HttpResponse.json({ account: current.account });
    }));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    expect(document.documentElement.lang).toBe("en");
    await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
    await user.click(screen.getByRole("option", { name: /Use Polski/ }));
    await waitFor(() => expect(document.documentElement.lang).toBe("pl"), { timeout: 15_000 });
    expect(current.account.edit_revision).toBe(2);
    expect(localStorage.getItem(uiLocalePreferenceKey)).toBe("de");
    expect(await screen.findByRole("button", { name: /Polski/ }, { timeout: 15_000 })).toBeEnabled();
  });

  it("keeps the previous account language and reports a failed topbar save", async () => {
    installSession();
    server.use(http.patch("*/v1/ui/profile", () => new HttpResponse(null, { status: 503 })));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
    await user.click(screen.getByRole("option", { name: /Use Deutsch/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not save your language. Your previous language is still active.");
    expect(document.documentElement.lang).toBe("en");
    expect(screen.getByRole("button", { name: /Choose language.*English/ })).toBeEnabled();
  });

  it("clears prior-account data when a topbar profile mutation is refused", async () => {
    installSession();
    server.use(http.patch("*/v1/ui/profile", () => new HttpResponse(null, { status: 401 })));
    const user = userEvent.setup();
    const stopped = vi.fn();
    render(<App connect={() => stopped} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
    await user.click(screen.getByRole("option", { name: /Use Deutsch/ }));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(screen.queryByText("Iris person")).toBeNull();
    expect(stopped).toHaveBeenCalledOnce();
  });

  it("discards a profile response that arrives after session revocation", async () => {
    installSession();
    let finish: () => void = () => {};
    const requested = vi.fn();
    const responseFinished = new Promise<void>((resolve) => {
      server.events.on("response:mocked", ({ request }) => { if (request.method === "PATCH") resolve(); });
    });
    server.use(http.patch("*/v1/ui/profile", async () => {
      requested();
      await new Promise<void>((resolve) => { finish = resolve; });
      return HttpResponse.json({ account: { ...accountSession.account, ui_locale: "pl", edit_revision: 2 } });
    }));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
    await user.click(screen.getByRole("option", { name: /Use Polski/ }));
    await waitFor(() => expect(requested).toHaveBeenCalledOnce());
    act(() => window.dispatchEvent(new Event("iris:unauthorized")));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    await act(async () => { finish(); await responseFinished; });
    expect(screen.getByLabelText("Username")).toBeVisible();
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(screen.queryByText("Iris person")).toBeNull();
  });

  it("does not replace the active identity with another account's profile response", async () => {
    installSession();
    server.use(http.patch("*/v1/ui/profile", () => HttpResponse.json({ account: { ...accountSession.account, id: "ca_other", display_name: "Another person", ui_locale: "pl" } })));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
    await user.click(screen.getByRole("option", { name: /Use Polski/ }));
    await waitFor(() => expect(screen.getByRole("button", { name: /Choose language.*English/ })).toBeEnabled());
    expect(screen.getByRole("button", { name: "Iris person" })).toBeVisible();
    expect(screen.queryByRole("button", { name: "Another person" })).toBeNull();
    expect(document.documentElement.lang).toBe("en");
  });

  it.each(["profile", "logout"])("keeps the next account signed in after a prior account's delayed %s refusal", async (operation) => {
    const current = installSession();
    let finish: () => void = () => {};
    const requested = vi.fn();
    const responseFinished = new Promise<void>((resolve) => {
      server.events.on("response:mocked", ({ request }) => {
        if ((operation === "profile" && request.method === "PATCH") || (operation === "logout" && new URL(request.url).pathname === "/v1/ui/logout")) resolve();
      });
    });
    const refuseLate = async () => {
      requested();
      await new Promise<void>((resolve) => { finish = resolve; });
      return new HttpResponse(null, { status: 401 });
    };
    server.use(
      http.patch("*/v1/ui/profile", refuseLate),
      http.post("*/v1/ui/logout", refuseLate),
      http.get("*/v1/ui/login", () => HttpResponse.json({ csrf: "iris-next-login-challenge" })),
      http.post("*/v1/ui/login", () => HttpResponse.json({ authenticated: true })),
    );
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    if (operation === "profile") {
      await user.click(screen.getByRole("button", { name: /Choose language.*English/ }));
      await user.click(screen.getByRole("option", { name: /Use Polski/ }));
    } else {
      await user.click(screen.getByRole("button", { name: "Sign out" }));
    }
    await waitFor(() => expect(requested).toHaveBeenCalledOnce());
    act(() => window.dispatchEvent(new Event("iris:unauthorized")));
    const username = await screen.findByLabelText("Username");
    current.account.id = "ca_next";
    current.account.username = "next-person";
    current.account.display_name = "Next person";
    current.csrf = "iris-next-session-csrf";
    fireEvent.change(username, { target: { value: "next-person" } });
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "next valid password" } });
    fireEvent.click(screen.getByRole("button", { name: "Sign in" }));
    expect(await screen.findByRole("button", { name: "Next person" })).toBeVisible();
    await act(async () => { finish(); await responseFinished; });
    expect(screen.getByRole("button", { name: "Next person" })).toBeVisible();
    expect(screen.queryByLabelText("Username")).toBeNull();
    expect(screen.getByRole("heading", { name: "Messages" })).toBeVisible();
  });

  it.each(["account", "authentication version", "session token"])("checks the current %s before admitting a reconnected stream", async (change) => {
    const current = installSession();
    let check: NonNullable<WatchOptions["check"]> = async () => {};
    let status: (status: WatchStatus) => void = () => {};
    const stopped = vi.fn();
    const connect = (_: (frame: WatchFrame) => void, onStatus?: (status: WatchStatus) => void, options?: WatchOptions) => {
      check = options!.check!;
      status = onStatus!;
      return stopped;
    };
    render(<App connect={connect} />);
    await screen.findByRole("heading", { name: "Messages" });
    await expect(check()).resolves.toBeUndefined();
    if (change === "account") current.account.id = "ca_another_person";
    else if (change === "authentication version") current.account.auth_version = 2;
    else current.csrf = "iris-another-session-csrf";
    await expect(check()).rejects.toEqual(new ApiError(401));
    act(() => status("unauthorized"));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(screen.queryByRole("navigation")).toBeNull();
    expect(stopped).toHaveBeenCalledOnce();
  });

  it("reuses an account send key within ten minutes and replaces it after that retry window", async () => {
    installSession("rw");
    let now = Date.now();
    vi.spyOn(Date, "now").mockImplementation(() => now);
    const attempts: OutgoingMessage[] = [];
    server.use(http.post("*/v1/ui/messages", async ({ request }) => {
      attempts.push(await request.json() as OutgoingMessage);
      return new HttpResponse(null, { status: 503 });
    }));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(screen.getByRole("button", { name: "+ New message" }));
    const compose = within(document.getElementById("compose")!);
    await user.click(compose.getByRole("checkbox", { name: /claude-host/ }));
    fireEvent.change(compose.getByRole("textbox", { name: "Subject" }), { target: { value: "Retry evidence" } });
    fireEvent.change(compose.getByRole("textbox", { name: "Message" }), { target: { value: "Keep this reviewed text" } });
    await user.click(compose.getByRole("button", { name: "Review" }));
    await user.click(compose.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(attempts).toHaveLength(1));
    await waitFor(() => expect(compose.getByRole("button", { name: "Send" })).toBeEnabled());
    expect(screen.getByRole("alert")).toHaveTextContent(/result is unknown.*ten minutes/i);
    now += 9 * 60 * 1000;
    await user.click(compose.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(attempts).toHaveLength(2));
    await waitFor(() => expect(compose.getByRole("button", { name: "Send" })).toBeEnabled());
    expect(attempts[1]?.key).toBe(attempts[0]?.key);
    now += 60 * 1000;
    await user.click(compose.getByRole("button", { name: "Send" }));
    expect(attempts).toHaveLength(2);
    expect(screen.getByRole("alert")).toHaveTextContent(/retry window has ended/i);
    expect(compose.queryByRole("button", { name: "Send" })).toBeNull();
    await user.click(compose.getByRole("button", { name: "Review" }));
    await user.click(compose.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(attempts).toHaveLength(3));
    expect(attempts[2]?.key).not.toBe(attempts[0]?.key);
    expect(attempts.map((attempt) => ({ ...attempt, key: "ignored" }))).toEqual(Array(3).fill({ key: "ignored", repository_id: "rp_iris", to: ["ag_claude"], subject: "Retry evidence", body: "Keep this reviewed text", reply_to: "" }));
  });
});

import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import { ApiError, setConsoleCsrf, type WatchFrame, type WatchStatus } from "./api";
import { clearAccountSession, type AccountSession } from "./accountApi";
import i18n from "./i18n";
import { clearUiLocale, uiLocalePreferenceKey } from "./preferences";
import { server, sessionOverview, threadDetail, threadsPage } from "./test/server";

const accountSession: AccountSession = {
  csrf: "iris-session-csrf", created_ms: 1, expires_ms: 9999999999999,
  account: { id: "ca_reader", username: "reader", role: "member", disabled: false, display_name: "Reader", ui_locale: "en", correspondence_locale: "pl", edit_revision: 1 },
};

function serveAccount(mode: "ro" | "rw" = "ro", role = "member") {
  const session = structuredClone(accountSession);
  session.account.role = role;
  server.use(
    http.get("*/v1/ui/session", () => HttpResponse.json(session)),
    http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, repositories: sessionOverview.repositories.map((repository) => ({ ...repository, console_access: mode })) })),
  );
  return session;
}

beforeEach(async () => {
  clearAccountSession();
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
    removeItem: (key: string) => values.delete(key),
  });
  await i18n.changeLanguage("en");
});

afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("staged named console", () => {
  it("offers a retry after a session lookup failure without opening a stream", async () => {
    const connect = vi.fn(() => () => {});
    server.use(http.get("*/v1/ui/session", () => new HttpResponse(null, { status: 503 })));
    render(<App connect={connect} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not check your session");
    expect(connect).not.toHaveBeenCalled();
    serveAccount();
    await userEvent.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    expect(connect).toHaveBeenCalledOnce();
  });

  it("shows login on an initial 401 and preserves the signed-out browser language", async () => {
    localStorage.setItem(uiLocalePreferenceKey, "en");
    server.use(http.get("*/v1/ui/session", () => new HttpResponse(null, { status: 401 })));
    render(<App connect={() => () => {}} />);
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(localStorage.getItem(uiLocalePreferenceKey)).toBe("en");
  });

  it("members read threads and claims without acknowledgement, diagnostics or revoke", async () => {
    serveAccount();
    const forbidden = vi.fn();
    server.use(
      http.post("*/v1/ui/threads/:id/read", () => { forbidden(); return HttpResponse.json({ read: 1 }); }),
      http.get("*/v1/ui/doctor", () => { forbidden(); return HttpResponse.json({ hosts: [] }); }),
    );
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    expect(screen.queryByRole("button", { name: "+ New message" })).toBeNull();
    expect(screen.queryByRole("button", { name: "Diagnostics" })).toBeNull();
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    expect(await screen.findByText("All checks passed.", { exact: false })).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Reply" })).toBeNull();
    await user.click(within(screen.getByRole("navigation")).getByRole("button", { name: /Claims/ }));
    expect(screen.getByText("docs/api.md")).toBeVisible();
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeNull();
    await user.click(within(screen.getByRole("navigation")).getByRole("button", { name: /Fleet/ }));
    expect(screen.queryByRole("columnheader", { name: "Client health" })).toBeNull();
    expect(forbidden).not.toHaveBeenCalled();
  });

  it("keeps profile available for an account with no repositories", async () => {
    serveAccount();
    server.use(http.get("*/v1/ui/overview", () => HttpResponse.json({ repositories: [], agents: [], claims: [], human_unread: 0 })));
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(/No repository access has been granted/)).toBeVisible();
    await userEvent.click(within(screen.getByRole("navigation")).getByRole("button", { name: "Account" }));
    expect(screen.getByRole("heading", { name: "Account" })).toBeVisible();
    expect(screen.getByDisplayValue("Reader")).toBeVisible();
  });

  it.each(["Author", ""])("uses current rw grants and immutable author snapshots for a scoped reply (%s)", async (displayName) => {
    serveAccount("rw");
    server.use(http.get("*/v1/ui/threads/:id", () => HttpResponse.json({ ...threadDetail, messages: threadDetail.messages.map((message) => ({ ...message, sender_account_id: "ca_author", sender_username: "at-send", sender_display_name: displayName })) })));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    expect(await screen.findAllByText(/Author via|at-send via/)).toHaveLength(threadDetail.messages.length);
    const reply = screen.getByRole("heading", { name: "Reply" }).closest("form")!;
    expect(within(reply).queryByRole("checkbox", { name: /^human/ })).toBeNull();
    expect(within(reply).getByRole("checkbox", { name: /claude-host/ })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "+ New message" }));
    const compose = document.getElementById("compose")!;
    expect(within(compose).getByRole("combobox", { name: "Send from repository" })).toHaveValue("rp_iris");
    expect(within(compose).queryByRole("checkbox", { name: /^human/ })).toBeNull();
  });

  it.each(["At-send person", ""])("shows at-send authors in the thread list (%s)", async (displayName) => {
    serveAccount();
    server.use(http.get("*/v1/ui/threads", () => HttpResponse.json({ ...threadsPage, threads: threadsPage.threads.map((thread) => ({ ...thread, authors: [{ sender_account_id: "ca_author", sender_username: "at-send-name", sender_display_name: displayName }] })) })));
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(`· ${displayName || "at-send-name"}`)).toBeVisible();
  });

  it("requires an explicit recipient when a cross-repository sender is outside the writable directory", async () => {
    serveAccount("rw");
    server.use(http.get("*/v1/ui/threads/:id", () => HttpResponse.json({ ...threadDetail, messages: [{ ...threadDetail.messages[0], sender: "outside-directory" }] })));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    const reply = (await screen.findByRole("heading", { name: "Reply" })).closest("form")!;
    expect(within(reply).getByRole("alert")).toHaveTextContent("outside-directory");
    fireEvent.change(within(reply).getByRole("textbox", { name: "Message" }), { target: { value: "A reply" } });
    expect(within(reply).getByRole("button", { name: "Review" })).toBeDisabled();
    await user.click(within(reply).getByRole("button", { name: "Clear selection" }));
    await user.click(within(reply).getByRole("checkbox", { name: /claude-host/ }));
    expect(within(reply).getByRole("button", { name: "Review" })).toBeEnabled();
  });

  it("invalidates an open review when repository permissions change", async () => {
    serveAccount("rw");
    let refresh: (frame: WatchFrame) => void = () => {};
    const connect = (frame: (frame: WatchFrame) => void) => { refresh = frame; return () => {}; };
    const user = userEvent.setup();
    render(<App connect={connect} />);
    await screen.findByRole("heading", { name: "Messages" });
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(screen.getByRole("button", { name: "+ New message" }));
    const compose = within(document.getElementById("compose")!);
    await user.click(compose.getByRole("checkbox", { name: /claude-host/ }));
    await user.type(compose.getByRole("textbox", { name: "Subject" }), "Draft");
    await user.type(compose.getByRole("textbox", { name: "Message" }), "Retain these words");
    await user.click(compose.getByRole("button", { name: "Review" }));
    expect(compose.getByRole("button", { name: "Send" })).toBeVisible();
    serveAccount("ro");
    act(() => refresh({ cursor: 2, kind: "changed" }));
    await waitFor(() => expect(screen.queryByRole("button", { name: "+ New message" })).toBeNull());
    expect(screen.queryByRole("button", { name: "Send" })).toBeNull();
    expect(await screen.findByText(/Your permissions changed/)).toBeVisible();
  });

  it("ends streams and clears drafts on revocation before a different account signs in", async () => {
    serveAccount("rw");
    const stopped = vi.fn();
    let status: (value: WatchStatus) => void = () => {};
    const connect = (_: (frame: WatchFrame) => void, callback?: (value: WatchStatus) => void) => { status = callback!; return stopped; };
    render(<App connect={connect} />);
    await screen.findByRole("heading", { name: "Messages" });
    act(() => status("unauthorized"));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(screen.queryByText("Reader")).toBeNull();
    expect(stopped).toHaveBeenCalledOnce();
  });

  it("reports sign-out errors and then clears the account after successful CSRF logout", async () => {
    serveAccount("rw", "operator");
    let fail = true;
    server.use(http.post("*/v1/ui/logout", ({ request }) => {
      expect(request.headers.get("X-Iris-CSRF")).toBe(accountSession.csrf);
      return new HttpResponse(null, { status: fail ? 503 : 200 });
    }));
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not sign out");
    fail = false;
    await user.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByLabelText("Username")).toBeVisible();
  });

  it("treats a stale sign-out as signed out and account locale errors as feedback", async () => {
    serveAccount("ro", "operator");
    server.use(http.post("*/v1/ui/logout", () => new HttpResponse(null, { status: 401 })));
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });
    await userEvent.click(screen.getByRole("button", { name: "Sign out" }));
    expect(await screen.findByLabelText("Username")).toBeVisible();
  });

  it("clears data after a protected request returns 401", async () => {
    serveAccount();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Messages" });
    act(() => window.dispatchEvent(new Event("iris:unauthorized")));
    expect(await screen.findByLabelText("Username")).toBeVisible();
    expect(screen.queryByRole("navigation")).toBeNull();
  });

  it("clears cached locale even when browser storage is unavailable", () => {
    const spy = vi.spyOn(localStorage, "removeItem").mockImplementation(() => { throw new Error("blocked"); });
    expect(() => clearUiLocale()).not.toThrow();
    spy.mockRestore();
    setConsoleCsrf("");
    expect(new ApiError(401).status).toBe(401);
  });
});

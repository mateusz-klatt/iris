import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "./api";
import { changePassword, clearAccountSession, getSession, listAccounts, login, logout, updateAccess, updateProfile, type AccountSession } from "./accountApi";
import { server } from "./test/server";

const session: AccountSession = {
  account: { id: "ca_iris", username: "Iris operator", display_name: "Owner", role: "operator", disabled: false, ui_locale: "en", correspondence_locale: "pl", edit_revision: 3 },
  csrf: "iris-session-csrf", created_ms: 1, expires_ms: 2,
};

afterEach(() => { clearAccountSession(); vi.restoreAllMocks(); });

describe("account browser API", () => {
  it("loads current access and sends an explicit revision, role, disabled state and ro/rw grants with session CSRF", async () => {
    const account = { ...session.account, grants: [{ repository_id: "rp_one", mode: "ro" }] };
    const edit = { edit_revision: 3, role: "member", disabled: false, grants: [{ repository_id: "rp_one", mode: "rw" as const }] };
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(session)),
      http.get("*/v1/ui/accounts", ({ request }) => {
        expect(request.headers.get("X-Iris-CSRF")).toBe(session.csrf);
        expect(request.credentials).toBe("same-origin");
        expect(request.cache).toBe("no-store");
        return HttpResponse.json({ accounts: [account] });
      }),
      http.put("*/v1/ui/accounts/:id/access", async ({ request, params }) => {
        expect(params.id).toBe("ca target/with slash");
        expect(request.headers.get("X-Iris-CSRF")).toBe(session.csrf);
        expect(await request.json()).toEqual(edit);
        return HttpResponse.json({ account: { ...account, ...edit, edit_revision: 4 } });
      }),
    );
    await getSession();
    expect(await listAccounts()).toEqual([account]);
    expect(await updateAccess("ca target/with slash", edit)).toMatchObject({ ...edit, edit_revision: 4 });
  });

  it.each([401, 403, 409])("preserves access authorization/conflict status %s for the editor", async (status) => {
    server.use(
      http.get("*/v1/ui/accounts", () => new HttpResponse(null, { status })),
      http.put("*/v1/ui/accounts/:id/access", () => new HttpResponse(null, { status })),
    );
    await expect(listAccounts()).rejects.toEqual(new ApiError(status));
    await expect(updateAccess("ca_target", { edit_revision: 1, role: "member", disabled: false, grants: [] })).rejects.toEqual(new ApiError(status));
  });

  it.each(["list", "save"])("discards the previous account's delayed access %s response", async (operation) => {
    let finish: (response: Response) => void = () => undefined;
    vi.spyOn(globalThis, "fetch").mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    const pending = operation === "list" ? listAccounts() : updateAccess("ca_target", { edit_revision: 1, role: "member", disabled: false, grants: [] });
    clearAccountSession();
    finish(new Response(JSON.stringify({ accounts: [], account: session.account })));
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
  });

  it("discards an access snapshot if its caller aborts before the response is admitted", async () => {
    const controller = new AbortController();
    vi.spyOn(globalThis, "fetch").mockImplementationOnce(async () => {
      controller.abort();
      return new Response(JSON.stringify({ accounts: [] }));
    });
    await expect(listAccounts(controller.signal)).rejects.toMatchObject({ name: "AbortError" });
  });

  it.each(["account", "authentication version", "session token"])("rejects a changed %s before replacing the mounted session token", async (change) => {
    const next = structuredClone(session);
    const tokens: (string | null)[] = [];
    server.use(
      http.get("*/v1/ui/session", ({ request }) => {
        tokens.push(request.headers.get("X-Iris-CSRF"));
        return HttpResponse.json(next);
      }),
      http.post("*/v1/ui/logout", ({ request }) => {
        expect(request.headers.get("X-Iris-CSRF")).toBe(session.csrf);
        return HttpResponse.json({ logged_out: true });
      }),
    );
    await getSession();
    await expect(getSession(undefined, session)).resolves.toEqual(session);
    if (change === "account") next.account.id = "ca_other";
    else if (change === "authentication version") next.account.auth_version = 2;
    else next.csrf = "iris-other-csrf";
    await expect(getSession(undefined, session)).rejects.toEqual(new ApiError(401));
    expect(tokens).toEqual([null, session.csrf, session.csrf]);
    await logout();
  });

  it.each([401, 404])("recognizes the staged legacy-mode marker before interpreting authentication", async (status) => {
    server.use(http.get("*/v1/ui/session", () => new HttpResponse(null, { status, headers: { "X-Iris-Console-Mode": "links" } })));
    await expect(getSession()).rejects.toEqual(new ApiError(404));
  });

  it("uses a fresh same-origin challenge on each attempt and keeps tokens only in memory", async () => {
    let challenges = 0;
    let attempts = 0;
    server.use(
      http.get("*/v1/ui/login", () => HttpResponse.json({ csrf: `iris-challenge-${++challenges}` })),
      http.post("*/v1/ui/login", async ({ request }) => {
        attempts++;
        expect(request.headers.get("X-Iris-CSRF")).toBe(`iris-challenge-${attempts}`);
        expect(await request.json()).toEqual({ username: "Iris operator", password: "unmodified password  " });
        expect(request.credentials).toBe("same-origin");
        expect(request.cache).toBe("no-store");
        expect(new URL(request.url).origin).toBe(window.location.origin);
        return attempts === 1 ? new HttpResponse(null, { status: 401 }) : HttpResponse.json({ authenticated: true });
      }),
      http.get("*/v1/ui/session", ({ request }) => {
        expect(request.headers.has("X-Iris-CSRF")).toBe(false);
        return HttpResponse.json(session);
      }),
    );
    const storage = vi.spyOn(Storage.prototype, "setItem");
    await expect(login("Iris operator", "unmodified password  ")).rejects.toEqual(new ApiError(401));
    await expect(login("Iris operator", "unmodified password  ")).resolves.toEqual(session);
    expect(challenges).toBe(2);
    expect(storage).not.toHaveBeenCalled();
  });

  it("preserves correspondence locale and uses the current revision and session CSRF for profile edits", async () => {
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(session)),
      http.patch("*/v1/ui/profile", async ({ request }) => {
        expect(request.headers.get("X-Iris-CSRF")).toBe(session.csrf);
        expect(await request.json()).toEqual({ edit_revision: 3, display_name: "New name", ui_locale: "de", correspondence_locale: "pl" });
        return HttpResponse.json({ account: { ...session.account, display_name: "New name", ui_locale: "de", edit_revision: 4 } });
      }),
    );
    const controller = new AbortController();
    expect(await getSession(controller.signal)).toEqual(session);
    expect(await updateProfile(session.account, "New name", "de")).toMatchObject({ display_name: "New name", ui_locale: "de", edit_revision: 4 });
  });

  it("ends the session only after successful logout or password change and clears its CSRF", async () => {
    const passwords: unknown[] = [];
    const tokens: (string | null)[] = [];
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(session)),
      http.post("*/v1/ui/password", async ({ request }) => {
        tokens.push(request.headers.get("X-Iris-CSRF"));
        passwords.push(await request.json());
        return HttpResponse.json({ login_required: true });
      }),
      http.post("*/v1/ui/logout", ({ request }) => {
        tokens.push(request.headers.get("X-Iris-CSRF"));
        return HttpResponse.json({ logged_out: true });
      }),
    );
    await getSession();
    await changePassword("old password", "new password without trimming  ");
    await logout();
    await getSession();
    await logout();
    expect(passwords).toEqual([{ current_password: "old password", new_password: "new password without trimming  " }]);
    expect(tokens).toEqual([session.csrf, "", session.csrf]);
  });

  it("does not admit a late session response after the caller aborts", async () => {
    const controller = new AbortController();
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementation(async () => {
      controller.abort();
      return new Response(JSON.stringify(session), { headers: { "Content-Type": "application/json" } });
    });
    await expect(getSession(controller.signal)).rejects.toMatchObject({ name: "AbortError" });
    fetch.mockResolvedValue(new Response("{}"));
    await logout();
    expect(fetch.mock.calls[1]?.[1]?.headers).toMatchObject({ "X-Iris-CSRF": "" });
  });

  it("rejects a previous account's delayed response after logout and allows a fresh login", async () => {
    let finish: (response: Response) => void = () => undefined;
    const fetch = vi.spyOn(globalThis, "fetch").mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    const pending = getSession();
    clearAccountSession();
    finish(new Response(JSON.stringify(session)));
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    fetch.mockResolvedValueOnce(new Response("{}"));
    await logout();
    expect(fetch.mock.calls[1]?.[1]?.headers).toMatchObject({ "X-Iris-CSRF": "" });
    fetch.mockRestore();
    server.use(
      http.get("*/v1/ui/login", () => HttpResponse.json({ csrf: "iris-fresh-challenge" })),
      http.post("*/v1/ui/login", () => HttpResponse.json({ authenticated: true })),
      http.get("*/v1/ui/session", () => HttpResponse.json(session)),
    );
    await expect(login("iris-person", "a current password")).resolves.toEqual(session);
  });

  it.each(["challenge", "credentials"])("does not finish a pending %s after the browser clears the account", async (stage) => {
    let finish: (response: Response) => void = () => undefined;
    const fetch = vi.spyOn(globalThis, "fetch");
    if (stage === "credentials") {
      fetch.mockResolvedValueOnce(new Response(JSON.stringify({ csrf: "iris-challenge" })));
    }
    fetch.mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }));
    const pending = login("iris-person", "a current password");
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledTimes(stage === "challenge" ? 1 : 2));
    clearAccountSession();
    finish(new Response(JSON.stringify({ csrf: "iris-old-challenge", authenticated: true })));
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    expect(fetch).toHaveBeenCalledTimes(stage === "challenge" ? 1 : 2);
  });

  it.each(["logout", "password change"])("does not let a delayed %s clear the next account's session", async (operation) => {
    let finish: (response: Response) => void = () => undefined;
    const nextSession = { ...session, account: { ...session.account, id: "ca_next" }, csrf: "iris-next-session-csrf" };
    const fetch = vi.spyOn(globalThis, "fetch")
      .mockResolvedValueOnce(new Response(JSON.stringify(session)))
      .mockImplementationOnce(() => new Promise((resolve) => { finish = resolve; }))
      .mockResolvedValueOnce(new Response(JSON.stringify(nextSession)))
      .mockResolvedValueOnce(new Response("{}"));
    await getSession();
    const pending = operation === "logout" ? logout() : changePassword("old password", "a valid new password");
    clearAccountSession();
    await expect(getSession()).resolves.toEqual(nextSession);
    finish(new Response("{}"));
    await expect(pending).rejects.toMatchObject({ name: "AbortError" });
    await logout();
    expect(fetch.mock.calls.at(-1)?.[1]?.headers).toMatchObject({ "X-Iris-CSRF": nextSession.csrf });
  });
});

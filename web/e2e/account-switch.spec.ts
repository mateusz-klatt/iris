import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";

test("a second tab's login clears the first operator's access editor and mail drafts", async ({ context, page }) => {
  const secondTab = await context.newPage();
  const errors: string[] = [];
  for (const tab of [page, secondTab]) tab.on("pageerror", (error) => { errors.push(error.message); });
  const accounts = [
    { id: "ca_first", username: "first-person", display_name: "First person", role: "operator", ui_locale: "pl", csrf: "iris-first-csrf", cookie: "first-session", subject: "First-only conversation" },
    { id: "ca_second", username: "second-person", display_name: "Second person", role: "member", ui_locale: "en", csrf: "iris-second-csrf", cookie: "second-session", subject: "Second-only conversation" },
  ];
  const sockets = new Map<Page, WebSocketRoute[]>();
  const closed = new Set<WebSocketRoute>();
  for (const tab of [page, secondTab]) {
    sockets.set(tab, []);
    await tab.routeWebSocket("**/v1/ui/watch", (socket) => {
      sockets.get(tab)!.push(socket);
      socket.onClose(() => { closed.add(socket); });
    });
  }
  const reads: { cookie: string; csrf: string | null; status: number }[] = [];
  const directoryReads: string[] = [];
  await context.route("**/v1/ui/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (path === "/v1/ui/login") {
      if (request.method() === "GET") {
        await route.fulfill({ json: { csrf: "iris-login-challenge" } });
      } else {
        const { username } = request.postDataJSON() as { username: string };
        const account = accounts.find((candidate) => candidate.username === username)!;
        await route.fulfill({ status: 204, headers: { "Set-Cookie": `iris_console_session=${account.cookie}; Path=/; HttpOnly; SameSite=Strict` } });
      }
      return;
    }
    // WebKit's intercepted fetch headers omit Cookie even when its shared jar
    // contains it. Read that same browser jar when the interception hides it.
    const cookie = (await request.headerValue("cookie"))?.match(/(?:^|;\s*)iris_console_session=([^;]+)/)?.[1]
      ?? (await context.cookies(request.url())).find((candidate) => candidate.name === "iris_console_session")?.value ?? "";
    const account = accounts.find((candidate) => candidate.cookie === cookie);
    const csrf = await request.headerValue("x-iris-csrf");
    // Model the hub's request/session binding. Bootstrap reads can omit the token;
    // a mounted console's supplied token must belong to the cookie on this request.
    const status = account === undefined || (csrf !== null && csrf !== account.csrf) ? 401 : 200;
    if (request.frame().page() === page && path === "/v1/ui/overview") reads.push({ cookie, csrf, status });
    if (status === 401 || account === undefined) {
      await route.fulfill({ status: 401 });
      return;
    }
    if (path === "/v1/ui/session") {
      await route.fulfill({ json: {
        account: { id: account.id, username: account.username, display_name: account.display_name, ui_locale: account.ui_locale, role: account.role, disabled: false, correspondence_locale: "en", edit_revision: 1, auth_version: 1 },
        csrf: account.csrf, created_ms: 1, expires_ms: 99_999_999_999_999,
      } });
    } else if (path === "/v1/ui/accounts") {
      directoryReads.push(account.id);
      await route.fulfill(account.role === "operator" ? { json: { accounts: [
        { id: account.id, username: account.username, display_name: account.display_name, ui_locale: account.ui_locale, role: account.role, disabled: false, correspondence_locale: "en", edit_revision: 1, auth_version: 1, grants: [] },
        { id: "ca_directory_member", username: "First-only directory member", display_name: "First-only directory member", ui_locale: "en", role: "member", disabled: false, correspondence_locale: "en", edit_revision: 1, auth_version: 1, grants: [{ repository_id: "rp_shared", mode: "ro" }] },
      ] } } : { status: 403 });
    } else if (path === "/v1/ui/overview") {
      await route.fulfill({ json: {
        repositories: [{ id: "rp_shared", name: account.subject, aliases: [], console_access: "rw" }],
        agents: [{ id: "ag_peer", name: "codex-peer", repository_id: "rp_shared", client: "codex", host: "local", worktree: "/iris", branch: "main", state: "active", last_seen: "2026-11-09T10:00:00Z", unread: 0 }],
        claims: [], human_unread: 0,
      } });
    } else if (path === "/v1/ui/threads") {
      await route.fulfill({ json: { threads: [{ id: "ms_shared", subject: account.subject, participants: ["codex-peer"], count: 1, last_at: "2026-11-09T10:00:00Z", unread: 0 }] } });
    } else if (path === "/v1/ui/threads/ms_shared") {
      await route.fulfill({ json: { id: "ms_shared", messages: [{ id: "ms_reply", sender: "codex-peer", repository_id: "rp_shared", recipients: [{ name: "human", state: "acked" }], subject: account.subject, body: account.subject, created_at: "2026-11-09T10:00:00Z" }] } });
    } else if (path === "/v1/ui/deliveries") {
      await route.fulfill({ json: { messages: [], more: false, window_s: 604800, wait_s: 900 } });
    } else if (path.endsWith("/read")) {
      await route.fulfill({ json: { read: 0 } });
    } else if (path === "/v1/ui/doctor") {
      await route.fulfill({ json: { hosts: [] } });
    } else {
      await route.fulfill({ status: 404 });
    }
  });
  await context.addInitScript(() => localStorage.setItem("iris.uiLocale", "en"));
  await Promise.all([page.goto("/"), secondTab.goto("/")]);
  for (const tab of [page, secondTab]) await expect(tab.locator(".iris-login")).toBeVisible();
  const signIn = async (tab: Page, username: string) => {
    await tab.locator("#iris-username").fill(username);
    await tab.locator("#iris-password").fill("existing account password");
    await tab.locator(".iris-login button[type=submit]").click();
  };
  await signIn(page, "first-person");
  await expect(page.locator(".account-identity")).toHaveText("First person");
  await expect(page.locator("html")).toHaveAttribute("lang", "pl");
  await page.locator(".mail-filters select").first().selectOption("rp_shared");
  await page.locator(".thread-row").filter({ hasText: "First-only conversation" }).click();
  await page.locator(".thread-reader textarea").fill("First account reply draft");
  await page.locator(".compose-action").click();
  await page.locator("#compose textarea").fill("First account compose draft");
  await expect.poll(() => sockets.get(page)!.length).toBe(1);
  await expect(page.locator(".stream-status")).toHaveClass(/stream-live/);
  const firstSocket = sockets.get(page)![0]!;
  await page.locator('nav button[aria-controls="iris-access-panel"]').click();
  await page.getByRole("button", { name: "First-only directory member", exact: true }).click();
  const repositoryAccess = page.locator("#iris-access-panel select").last();
  await expect(repositoryAccess).toHaveValue("ro");
  await repositoryAccess.selectOption("rw");
  await expect(secondTab.locator(".iris-login")).toBeVisible();
  await signIn(secondTab, "second-person");
  await expect(secondTab.locator(".account-identity")).toHaveText("Second person");
  expect((await context.cookies()).find((cookie) => cookie.name === "iris_console_session")?.value).toBe("second-session");
  expect(closed.has(firstSocket)).toBe(false);
  await expect(page.locator(".account-identity")).toHaveText("First person");
  await expect(page.getByRole("button", { name: "First-only directory member", exact: true })).toBeVisible();
  await expect(repositoryAccess).toHaveValue("rw");
  await page.evaluate(() => {
    const state = window as Window & { irisMixedAccounts?: boolean };
    state.irisMixedAccounts = false;
    new MutationObserver(() => {
      if (document.querySelector(".account-identity")?.textContent === "First person" && document.body.textContent?.includes("Second-only conversation")) state.irisMixedAccounts = true;
    }).observe(document.body, { childList: true, subtree: true, characterData: true });
  });
  firstSocket.send(JSON.stringify({ cursor: 2, kind: "changed" }));
  await expect.poll(async () => page.evaluate(() => document.querySelector(".iris-login") !== null || (window as Window & { irisMixedAccounts?: boolean }).irisMixedAccounts === true)).toBe(true);
  expect(await page.evaluate(() => (window as Window & { irisMixedAccounts?: boolean }).irisMixedAccounts)).toBe(false);
  await expect(page.locator(".iris-login")).toBeVisible();
  await expect(page.locator(".account-identity")).toHaveCount(0);
  await expect(page.locator("#compose")).toHaveCount(0);
  await expect(page.locator("#iris-access-panel")).toHaveCount(0);
  await expect(page.getByText("First-only directory member", { exact: true })).toHaveCount(0);
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  expect(await page.evaluate(() => localStorage.getItem("iris.uiLocale"))).toBeNull();
  await expect.poll(() => closed.has(firstSocket)).toBe(true);
  expect(reads.at(-1)).toEqual({ cookie: "second-session", csrf: "iris-first-csrf", status: 401 });
  await expect(secondTab.locator(".account-identity")).toHaveText("Second person");
  await expect(secondTab.locator('nav button[aria-controls="iris-access-panel"]')).toHaveCount(0);
  await signIn(page, "second-person");
  await expect(page.locator(".account-identity")).toHaveText("Second person");
  await page.locator(".mail-filters select").first().selectOption("rp_shared");
  await page.locator(".thread-row").filter({ hasText: "Second-only conversation" }).click();
  await expect(page.locator(".thread-reader textarea")).toHaveValue("");
  await page.locator(".compose-action").click();
  await expect(page.locator("#compose textarea")).toHaveValue("");
  expect(directoryReads).toEqual(["ca_first"]);
  expect(errors).toEqual([]);
});

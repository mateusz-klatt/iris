import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";

let browserErrors: string[] = [];
test.beforeEach(async ({ page }) => {
  browserErrors = [];
  page.on("pageerror", (error) => { browserErrors.push(error.message); });
});
test.afterEach(async ({ page }, info) => {
  if (info.status !== info.expectedStatus) console.log("Iris account failure view:", await page.locator("body").innerText());
  expect(browserErrors).toEqual([]);
});

// Account/profile interaction scenarios adapted from the owner's permitted UI export,
// using Iris's named session, CSRF and shared-mail contracts (ADR 014).
const csrf = "iris-browser-test-csrf";
const account = {
  id: "ca_first",
  username: "first-person",
  role: "member",
  disabled: false,
  display_name: "First person",
  ui_locale: "en",
  correspondence_locale: "pl",
  created_ms: 1,
  last_login_ms: 2,
  auth_version: 1,
  edit_revision: 1,
};

const repositories = [
  { id: "rp_origin", name: "origin", aliases: [], console_access: "rw" },
  { id: "rp_target", name: "target", aliases: [], console_access: "rw" },
  { id: "rp_read", name: "read-only", aliases: [], console_access: "ro" },
];
const agents = repositories.map((repository) => ({
  id: `ag_${repository.name}`,
  name: `codex-${repository.name}`,
  client: "codex",
  host: "local",
  repository_id: repository.id,
  worktree: "/iris",
  branch: "main",
  state: "active",
  last_seen: "2026-11-09T10:00:00Z",
  unread: 0,
}));
const summary = { id: "ms_shared", subject: "Shared review", participants: ["human", "codex-target"], count: 2, last_at: "2026-11-09T10:00:00Z", unread: 1 };
const detail = {
  id: summary.id,
  messages: [
    {
      id: "ms_authored", sender: "human", sender_account_id: "ca_author", sender_username: "author", sender_display_name: "Earlier author",
      repository_id: "rp_origin", recipients: [{ name: "codex-target", state: "acked", repository_id: "rp_target", agent_id: "ag_target" }],
      subject: summary.subject, body: "An attributed shared message.", created_at: "2026-11-09T09:00:00Z",
    },
    {
      id: "ms_reply", sender: "codex-target", repository_id: "rp_target", recipients: [{ name: "human", state: "pending", repository_id: "rp_origin", agent_id: "ag_human" }],
      subject: summary.subject, body: "Please review the shared reply.", created_at: summary.last_at,
    },
  ],
};

async function installAccounts(page: Page, signedIn = true) {
  const state = {
    signedIn,
    account: { ...account },
    repositories: repositories.map((repository) => ({ ...repository })),
    challenges: 0,
    rejectLogin: false,
    conflictProfile: false,
    failProfile: false,
    failLogout: false,
    failSession: false,
    mutations: [] as { path: string; csrf: string | undefined; origin: string | undefined; body: Record<string, unknown> }[],
    deniedRequests: [] as string[],
    sockets: [] as WebSocketRoute[],
    sent: [] as Record<string, unknown>[],
  };
  await page.routeWebSocket("**/v1/ui/watch", (socket) => { state.sockets.push(socket); });
  await page.route("**/v1/ui/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (request.method() !== "GET") {
      state.mutations.push({ path, csrf: request.headers()["x-iris-csrf"], origin: request.headers().origin, body: request.postDataJSON() as Record<string, unknown> });
    }
    if (path === "/v1/ui/session") {
      await route.fulfill(state.failSession ? { status: 503 } : state.signedIn ? { json: { account: state.account, csrf, created_ms: 1, expires_ms: 99_999_999_999_999 } } : { status: 401 });
      return;
    }
    if (path === "/v1/ui/login") {
      if (request.method() === "GET") {
        state.challenges++;
        await route.fulfill({ json: { csrf: `iris-challenge-${state.challenges}`, expires_ms: 99_999_999_999_999 } });
      } else if (state.rejectLogin) {
        await route.fulfill({ status: 401 });
      } else {
        state.signedIn = true;
        await route.fulfill({ status: 204 });
      }
      return;
    }
    if (path === "/v1/ui/logout") {
      if (state.failLogout) { await route.fulfill({ status: 503 }); return; }
      state.signedIn = false;
      await route.fulfill({ status: 204 });
      return;
    }
    if (path === "/v1/ui/profile") {
      if (state.failProfile) { await route.fulfill({ status: 503 }); return; }
      if (state.conflictProfile) {
        state.conflictProfile = false;
        state.account = { ...state.account, display_name: "Changed elsewhere", edit_revision: state.account.edit_revision + 1 };
        await route.fulfill({ status: 409 });
        return;
      }
      state.account = { ...state.account, ...request.postDataJSON(), edit_revision: state.account.edit_revision + 1 };
      await route.fulfill({ json: { account: state.account } });
      return;
    }
    if (path === "/v1/ui/password") {
      state.signedIn = false;
      await route.fulfill({ status: 204 });
      return;
    }
    if (path === "/v1/ui/doctor" || path === "/v1/ui/accounts" || path.endsWith("/read") || path.includes("/revoke")) {
      if (state.account.role === "member") { state.deniedRequests.push(path); await route.fulfill({ status: 403 }); return; }
      await route.fulfill({ json: path.endsWith("/read") ? { read: 1 } : { hosts: [] } });
      return;
    }
    if (!state.signedIn) { await route.fulfill({ status: 401 }); return; }
    if (path === "/v1/ui/overview") {
      await route.fulfill({ json: {
        repositories: state.repositories,
        agents: agents.filter((agent) => state.repositories.some((repository) => repository.id === agent.repository_id)),
        claims: [{ fence: 1, repository_id: "rp_origin", path: "web/src/App.tsx", kind: "exact", holder: "codex-origin", origin: "auto", expires_at: null }],
        human_unread: 1,
      } });
      return;
    }
    if (path === "/v1/ui/deliveries") { await route.fulfill({ json: { messages: [], more: false, window_s: 604800, wait_s: 900 } }); return; }
    if (path === "/v1/ui/threads") { await route.fulfill({ json: { threads: state.repositories.length === 0 ? [] : [summary] } }); return; }
    if (path === "/v1/ui/threads/ms_shared") { await route.fulfill({ json: detail }); return; }
    if (path === "/v1/ui/messages") { state.sent.push(request.postDataJSON() as Record<string, unknown>); await route.fulfill({ status: 204 }); return; }
    await route.fulfill({ status: 404 });
  });
  return state;
}

async function fitsScreen(page: Page) {
  const layout = await page.evaluate(() => ({
    overflow: document.documentElement.scrollWidth - document.documentElement.clientWidth,
    clipped: [...document.querySelectorAll<HTMLElement>("input, select, textarea, .settings-card")]
      .filter((element) => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== "hidden")
      .flatMap((element) => {
        const bounds = element.getBoundingClientRect();
        return bounds.left < 0 || bounds.right > window.innerWidth ? [{ element: element.tagName, id: element.id, left: bounds.left, right: bounds.right }] : [];
      }),
  }));
  expect(layout.overflow).toBeLessThanOrEqual(0);
  expect(layout.clipped).toEqual([]);
}

test("login renews its challenge after rejection and signs in using the keyboard", async ({ page }) => {
  const state = await installAccounts(page, false);
  state.rejectLogin = true;
  await page.goto("/");
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  await page.getByLabel("Username", { exact: true }).fill("first-person");
  const password = page.getByLabel("Password", { exact: true });
  await password.fill("existing imported password");
  await password.press("Enter");
  await expect(page.getByText("Invalid username or password", { exact: false })).toBeVisible();
  expect(state.mutations[0]).toMatchObject({ path: "/v1/ui/login", csrf: "iris-challenge-1" });
  expect(state.challenges).toBe(1);
  state.rejectLogin = false;
  await password.fill("existing imported password");
  await password.press("Enter");
  await expect(page.getByRole("heading", { name: "Messages", exact: true })).toBeVisible();
  expect(state.challenges).toBe(2);
  expect(state.mutations[1]).toMatchObject({ path: "/v1/ui/login", csrf: "iris-challenge-2", body: { username: "first-person", password: "existing imported password" } });
  await fitsScreen(page);
});

test("profile conflicts refresh facts, preserve correspondence locale and save Unicode names", async ({ page }) => {
  const state = await installAccounts(page);
  await page.goto("/");
  await page.getByRole("navigation").getByRole("button", { name: "Account", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Account", exact: true })).toBeVisible();
  await expect(page.getByLabel("Correspondence language")).toHaveCount(0);
  const display = page.getByLabel("Display name", { exact: true });
  state.conflictProfile = true;
  await display.fill("First edit");
  await page.getByRole("button", { name: "Save display name", exact: true }).click();
  await expect(display).toHaveValue("Changed elsewhere");
  await expect(page.getByText("Your profile changed elsewhere.", { exact: false })).toBeVisible();
  const unicodeName = "𐐀".repeat(128);
  await display.fill(unicodeName);
  await page.getByRole("button", { name: "Save display name", exact: true }).click();
  await expect(page.getByText("Display name saved.", { exact: true })).toBeVisible();
  expect(state.account.display_name).toBe(unicodeName);
  expect(state.mutations.at(-1)).toMatchObject({ path: "/v1/ui/profile", csrf, body: { edit_revision: 2, display_name: unicodeName, ui_locale: "en", correspondence_locale: "pl" } });
  await fitsScreen(page);
});

test("password validation counts code points and UTF-8 bytes then ends all sessions", async ({ page }) => {
  const state = await installAccounts(page);
  await page.goto("/");
  await page.getByRole("navigation").getByRole("button", { name: "Account", exact: true }).click();
  await page.getByLabel("Current password", { exact: true }).fill("existing password");
  const next = page.getByLabel("New password", { exact: true });
  const confirm = page.getByLabel("Confirm new password", { exact: true });
  const submit = page.getByRole("button", { name: "Change password", exact: true });
  await next.fill("😀".repeat(14));
  await confirm.fill("😀".repeat(14));
  await submit.click();
  expect(state.mutations.filter((mutation) => mutation.path === "/v1/ui/password")).toHaveLength(0);
  await next.fill("😀".repeat(257));
  await confirm.fill("😀".repeat(257));
  await submit.click();
  expect(state.mutations.filter((mutation) => mutation.path === "/v1/ui/password")).toHaveLength(0);
  await next.fill("😀".repeat(15));
  await confirm.fill("😀".repeat(15));
  await fitsScreen(page);
  await submit.click();
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  expect(state.mutations.at(-1)).toMatchObject({ path: "/v1/ui/password", csrf, body: { current_password: "existing password", new_password: "😀".repeat(15) } });
});

test("read-only members navigate shared mail and claims without mutation or diagnostics controls", async ({ page }) => {
  const state = await installAccounts(page);
  state.repositories = [{ ...repositories[0]!, console_access: "ro" }];
  await page.goto("/");
  const navigation = page.getByRole("navigation");
  await expect(navigation.getByRole("button", { name: /^Messages/ })).toBeVisible();
  await expect(page.getByRole("button", { name: "+ New message", exact: true })).toHaveCount(0);
  await expect(navigation.getByRole("button", { name: "Diagnostics", exact: true })).toHaveCount(0);
  await expect(navigation.getByRole("button", { name: "Administration", exact: true })).toHaveCount(0);
  await page.getByRole("button", { name: "Open thread: Shared review", exact: false }).click();
  await expect(page.getByText("Earlier author", { exact: false })).toBeVisible();
  await expect(page.locator(".thread-reader textarea")).toHaveCount(0);
  await navigation.getByRole("button", { name: /^Claims/ }).click();
  await expect(page.getByText("web/src/App.tsx", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /Revoke/ })).toHaveCount(0);
  expect(state.deniedRequests).toEqual([]);
  expect(state.mutations).toEqual([]);
  await fitsScreen(page);
});

test("mixed grants restrict compose recipients and cross-repository replies to explicit IDs", async ({ page }) => {
  const state = await installAccounts(page);
  await page.goto("/");
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  const compose = page.locator("#compose");
  await expect(compose.getByRole("checkbox", { name: /^human/ })).toHaveCount(0);
  await expect(compose.getByRole("checkbox", { name: /codex-read-only/ })).toHaveCount(0);
  await compose.getByRole("checkbox", { name: /codex-target/ }).check();
  await compose.getByRole("textbox", { name: "Subject", exact: true }).fill("Across writable repositories");
  await compose.getByRole("textbox", { name: "Message", exact: true }).fill("A member's reviewed message.");
  await compose.getByRole("button", { name: "Review", exact: true }).click();
  await compose.getByRole("region", { name: "Check before sending", exact: true }).getByRole("button", { name: "Send", exact: true }).click();
  await expect.poll(() => state.sent.length).toBe(1);
  expect(state.sent[0]).toMatchObject({ repository_id: "rp_origin", to: ["ag_target"], subject: "Across writable repositories" });
  await page.getByRole("navigation").getByRole("button", { name: /^Messages/ }).click();
  await page.getByRole("button", { name: "Open thread: Shared review", exact: false }).click();
  const reader = page.locator(".thread-reader");
  await reader.getByRole("textbox", { name: "Message", exact: true }).fill("Reply through the selected shared mailbox.");
  await reader.getByRole("button", { name: "Review", exact: true }).click();
  await reader.getByRole("region", { name: "Check before sending", exact: true }).getByRole("button", { name: "Reply", exact: true }).click();
  await expect.poll(() => state.sent.length).toBe(2);
  expect(state.sent[1]).toMatchObject({ repository_id: "rp_origin", to: ["ag_target"], reply_to: "ms_reply" });
  expect(state.deniedRequests).toEqual([]);
  for (const mutation of state.mutations) expect(mutation.csrf).toBe(csrf);
  await fitsScreen(page);
});

test("logout failure remains retryable and the next account cannot recover the previous draft", async ({ page }) => {
  const state = await installAccounts(page);
  await page.addInitScript(() => localStorage.setItem("iris.uiLocale", "de"));
  await page.goto("/");
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  await page.locator("#compose").getByRole("textbox", { name: "Message", exact: true }).fill("First account private draft");
  state.failLogout = true;
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByText("Could not sign out. Try again.", { exact: true })).toBeVisible();
  state.failLogout = false;
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  expect(await page.evaluate(() => localStorage.getItem("iris.uiLocale"))).toBeNull();
  state.account = { ...account, id: "ca_second", username: "second-person", display_name: "Second person" };
  await page.getByLabel("Username", { exact: true }).fill("second-person");
  await page.getByLabel("Password", { exact: true }).fill("second password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Messages", exact: true })).toBeVisible();
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  await expect(page.locator("#compose").getByRole("textbox", { name: "Message", exact: true })).toHaveValue("");
  await expect(page.getByText("First account private draft", { exact: true })).toHaveCount(0);
  await fitsScreen(page);
});

test("a member without grants keeps account and logout access", async ({ page }) => {
  const state = await installAccounts(page);
  state.repositories = [];
  await page.goto("/");
  await expect(page.getByText("No repository access has been granted. You can manage your account.", { exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: "+ New message", exact: true })).toHaveCount(0);
  await page.getByRole("navigation").getByRole("button", { name: "Account", exact: true }).click();
  await expect(page.getByLabel("Display name", { exact: true })).toHaveValue("First person");
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  expect(state.deniedRequests).toEqual([]);
  await fitsScreen(page);
});

test("a revoked stream clears the open draft and account facts before signing in again", async ({ page }) => {
  const state = await installAccounts(page);
  await page.addInitScript(() => localStorage.setItem("iris.uiLocale", "de"));
  await page.goto("/");
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  await page.locator("#compose").getByRole("textbox", { name: "Message", exact: true }).fill("Revoked account draft");
  await expect.poll(() => state.sockets.length).toBe(1);
  state.signedIn = false;
  state.sockets[0]!.close({ code: 1008, reason: "Session revoked" });
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  await expect(page.getByText("First person", { exact: true })).toHaveCount(0);
  await expect(page.getByText("Revoked account draft", { exact: true })).toHaveCount(0);
  expect(await page.evaluate(() => localStorage.getItem("iris.uiLocale"))).toBeNull();
  state.account = { ...account, id: "ca_second", username: "second-person", display_name: "Second person" };
  await page.getByLabel("Username", { exact: true }).fill("second-person");
  await page.getByLabel("Password", { exact: true }).fill("second password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Messages", exact: true })).toBeVisible();
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  await expect(page.locator("#compose").getByRole("textbox", { name: "Message", exact: true })).toHaveValue("");
});

test("permission changes invalidate a draft review before a send can occur", async ({ page }) => {
  const state = await installAccounts(page);
  await page.goto("/");
  await page.locator(".mail-filters").getByRole("combobox", { name: "Repository", exact: true }).selectOption("rp_origin");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  const compose = page.locator("#compose");
  await compose.getByRole("checkbox", { name: /codex-target/ }).check();
  await compose.getByRole("textbox", { name: "Subject", exact: true }).fill("Review before access changes");
  await compose.getByRole("textbox", { name: "Message", exact: true }).fill("Access must be current when sending.");
  await compose.getByRole("button", { name: "Review", exact: true }).click();
  await expect(compose.getByRole("region", { name: "Check before sending", exact: true })).toBeVisible();
  state.repositories[1]!.console_access = "ro";
  state.sockets[0]!.send(JSON.stringify({ cursor: 2, kind: "changed" }));
  await expect(page.getByText("Your permissions changed. Review the repository and recipients before sending.", { exact: true })).toBeVisible();
  await expect(compose.getByRole("region", { name: "Check before sending", exact: true })).toHaveCount(0);
  await expect(compose.getByRole("checkbox", { name: /codex-target/ })).toHaveCount(0);
  await expect(compose.getByRole("button", { name: "Review", exact: true })).toBeDisabled();
  expect(state.sent).toEqual([]);
});

test("session lookup errors expose no stale console data and can be retried", async ({ page }) => {
  const state = await installAccounts(page);
  state.failSession = true;
  await page.goto("/");
  await expect(page.getByRole("alert")).toHaveText("Could not check your session. Try again.");
  await expect(page.getByRole("navigation")).toHaveCount(0);
  await expect(page.getByText("First person", { exact: true })).toHaveCount(0);
  state.failSession = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Messages", exact: true })).toBeVisible();
  await fitsScreen(page);
});

test("a closed console explains who can restore sign-in and retries without anonymous access", async ({ page }) => {
  const state = await installAccounts(page, false);
  let closed = true;
  await page.route("**/v1/ui/session", async (route) => {
    if (closed) await route.fulfill({ status: 401, headers: { "X-Iris-Console-Mode": "closed" } });
    else await route.fulfill({ status: 401 });
  });
  await page.goto("/");
  await expect(page.getByRole("alert")).toHaveText("Console sign-in is unavailable. Contact the hub owner.");
  await expect(page.getByLabel("Username", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("navigation")).toHaveCount(0);
  expect(state.sockets).toEqual([]);
  expect(state.mutations).toEqual([]);
  await fitsScreen(page);
  closed = false;
  await page.getByRole("button", { name: "Try again", exact: true }).click();
  await expect(page.getByLabel("Username", { exact: true })).toBeVisible();
  await expect(page.getByLabel("Password", { exact: true })).toBeVisible();
  expect(state.sockets).toEqual([]);
});

test("a profile response arriving after logout cannot restore the prior account", async ({ page }) => {
  await installAccounts(page);
  let releaseProfile!: () => void;
  let profileRequested = false;
  const pending = new Promise<void>((resolve) => { releaseProfile = resolve; });
  await page.route("**/v1/ui/profile", async (route) => {
    profileRequested = true;
    await pending;
    await route.fulfill({ json: { account: { ...account, display_name: "Late old-account response", edit_revision: 2 } } });
  });
  await page.goto("/");
  await page.getByRole("navigation").getByRole("button", { name: "Account", exact: true }).click();
  await page.getByLabel("Display name", { exact: true }).fill("Late old-account response");
  await page.getByRole("button", { name: "Save display name", exact: true }).click();
  await expect.poll(() => profileRequested).toBe(true);
  await expect(page.getByRole("button", { name: "Saving display name…", exact: true })).toBeDisabled();
  await expect(page.getByLabel("Display name", { exact: true })).toBeDisabled();
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
  const response = page.waitForResponse("**/v1/ui/profile");
  releaseProfile();
  await (await response).finished();
  await page.evaluate(() => new Promise<void>((resolve) => requestAnimationFrame(() => requestAnimationFrame(() => resolve()))));
  await expect(page.getByText("Late old-account response", { exact: true })).toHaveCount(0);
  await expect(page.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
});

test("account language overrides browser storage and preserves the hidden correspondence preference", async ({ page }) => {
  const state = await installAccounts(page);
  await page.addInitScript(() => localStorage.setItem("iris.uiLocale", "de"));
  await page.goto("/");
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await page.getByRole("navigation").getByRole("button", { name: "Account", exact: true }).click();
  await page.locator("#iris-account-language").selectOption("pl");
  await expect(page.locator("html")).toHaveAttribute("lang", "pl");
  expect(state.account.ui_locale).toBe("pl");
  expect(state.account.correspondence_locale).toBe("pl");
  expect(state.mutations.at(-1)).toMatchObject({ path: "/v1/ui/profile", csrf, body: { ui_locale: "pl", correspondence_locale: "pl" } });
  // A failed save leaves the account's active language intact.
  state.failProfile = true;
  await page.locator("#iris-account-language").selectOption("fr");
  await expect(page.locator("#iris-profile-status")).not.toHaveText("");
  await expect(page.locator("html")).toHaveAttribute("lang", "pl");
  await expect(page.locator("#iris-account-language")).toHaveValue("pl");
  state.failProfile = false;
  await page.locator("#iris-account-language").selectOption("en");
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await fitsScreen(page);
});

test("named operators retain acknowledgement, claim and diagnostics controls with CSRF", async ({ page }) => {
  const state = await installAccounts(page);
  state.account.role = "operator";
  await page.goto("/");
  const navigation = page.getByRole("navigation");
  await expect(page.getByRole("heading", { name: "My messages", exact: true })).toBeVisible();
  await expect(navigation.getByRole("button", { name: "Diagnostics", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "Open thread: Shared review", exact: false }).click();
  await expect.poll(() => state.mutations.filter((mutation) => mutation.path.endsWith("/read")).length).toBeGreaterThan(0);
  await navigation.getByRole("button", { name: /^Claims/ }).click();
  page.once("dialog", (dialog) => { void dialog.accept(); });
  await page.getByRole("button", { name: "Revoke", exact: true }).click();
  await expect.poll(() => state.mutations.filter((mutation) => mutation.path.endsWith("/revoke")).length).toBe(1);
  for (const mutation of state.mutations) expect(mutation.csrf).toBe(csrf);
  await navigation.getByRole("button", { name: "Diagnostics", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Diagnostics", exact: true })).toBeVisible();
  await fitsScreen(page);
});

test("signed-out language selection survives a reload and yields to the account language", async ({ page }) => {
  await installAccounts(page, false);
  await page.goto("/");
  await page.getByRole("button", { name: "Choose language. Current language: English", exact: true }).click();
  await page.getByRole("option", { name: /Use العربية/ }).click();
  await expect(page.locator("html")).toHaveAttribute("lang", "ar");
  await expect(page.locator("html")).toHaveAttribute("dir", "rtl");
  await fitsScreen(page);
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("lang", "ar");
  await expect(page.locator("html")).toHaveAttribute("dir", "rtl");
  await page.locator("#iris-username").fill("first-person");
  await page.locator("#iris-password").fill("existing password");
  await page.locator(".iris-login button[type=submit]").click();
  await expect(page.locator("html")).toHaveAttribute("lang", "en");
  await expect(page.locator("html")).toHaveAttribute("dir", "ltr");
  await expect(page.getByRole("heading", { name: "Messages", exact: true })).toBeVisible();
  await fitsScreen(page);
});

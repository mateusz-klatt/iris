import { expect, test, type Page, type WebSocketRoute } from "@playwright/test";

// Access interactions follow ADR 014's Iris roles and revisioned grant contract.
// These browser fixtures contain only synthetic identities and repositories.
const csrf = "iris-access-test-csrf";
interface Grant { repository_id: string; mode: "ro" | "rw" }
interface AccessAccount {
  id: string;
  username: string;
  display_name: string;
  role: string;
  disabled: boolean;
  ui_locale: string;
  correspondence_locale: string;
  edit_revision: number;
  auth_version: number;
  grants: Grant[];
}
const person = (id: string, username: string, role: string, disabled = false): AccessAccount => ({
  id, username, role, disabled, display_name: username, ui_locale: "en", correspondence_locale: "",
  edit_revision: 1, auth_version: 1, grants: [],
});
const repositories = [
  { id: "rp_alpha", name: "alpha", aliases: [], console_access: "rw" },
  { id: "rp_beta", name: "beta", aliases: [], console_access: "rw" },
];
const alphaAccess = "alpha · rp_alpha access";
const betaAccess = "beta · rp_beta access";

async function installAccess(page: Page) {
  const operator = person("ca_operator", "iris-operator", "operator");
  const member = person("ca_member", "iris-member", "member");
  const quarantined = person("ca_none", "iris-quarantined", "none", true);
  const state = {
    current: operator,
    accounts: [operator, member, quarantined],
    signedIn: true,
    accountReads: 0,
    conflict: false,
    writes: [] as { path: string; csrf: string | undefined; origin: string | undefined; body: Record<string, unknown> }[],
    sockets: [] as WebSocketRoute[],
    errors: [] as string[],
  };
  page.on("pageerror", (error) => { state.errors.push(error.message); });
  await page.routeWebSocket("**/v1/ui/watch", (socket) => { state.sockets.push(socket); });
  await page.route("**/v1/ui/**", async (route) => {
    const request = route.request();
    const path = new URL(request.url()).pathname;
    if (request.method() !== "GET") state.writes.push({ path, csrf: request.headers()["x-iris-csrf"], origin: request.headers().origin, body: request.postDataJSON() as Record<string, unknown> });
    if (path === "/v1/ui/login") {
      if (request.method() === "GET") await route.fulfill({ json: { csrf: "iris-access-login-challenge" } });
      else { state.signedIn = true; await route.fulfill({ status: 204 }); }
      return;
    }
    if (path === "/v1/ui/logout") { state.signedIn = false; await route.fulfill({ status: 204 }); return; }
    if (!state.signedIn) { await route.fulfill({ status: 401 }); return; }
    if (path === "/v1/ui/session") {
      await route.fulfill({ json: { account: state.current, csrf, created_ms: 1, expires_ms: 99_999_999_999_999 } });
    } else if (path === "/v1/ui/accounts") {
      state.accountReads++;
      await route.fulfill(state.current.role === "operator" ? { json: { accounts: state.accounts } } : { status: 403 });
    } else if (path.startsWith("/v1/ui/accounts/") && path.endsWith("/access")) {
      const target = state.accounts.find((account) => account.id === path.split("/")[4])!;
      const body = request.postDataJSON() as { edit_revision: number; role: string; disabled: boolean; grants: Grant[] };
      if (state.conflict) {
        state.conflict = false;
        target.edit_revision++;
        target.grants = [{ repository_id: "rp_alpha", mode: "ro" }];
        await route.fulfill({ status: 409 });
      } else if (body.edit_revision !== target.edit_revision || (target.role === "operator" && !target.disabled && (body.role !== "operator" || body.disabled) && state.accounts.filter((account) => account.role === "operator" && !account.disabled).length === 1)) {
        await route.fulfill({ status: 409 });
      } else {
        Object.assign(target, body, { disabled: body.role === "none" || body.disabled, edit_revision: target.edit_revision + 1, auth_version: target.auth_version + 1 });
        await route.fulfill({ json: { account: target } });
      }
    } else if (path === "/v1/ui/overview") {
      const visible = state.current.role === "operator" ? repositories : repositories.filter((repository) => state.current.grants.some((grant) => grant.repository_id === repository.id));
      await route.fulfill({ json: { repositories: visible, agents: visible.map((repository) => ({ id: `ag_${repository.id}`, name: `codex-${repository.name}`, repository_id: repository.id, client: "codex", host: "local", worktree: "/iris", branch: "main", state: "active", last_seen: "2026-11-09T10:00:00Z", unread: 0 })), claims: [], human_unread: 0 } });
    } else if (path === "/v1/ui/threads") await route.fulfill({ json: { threads: [] } });
    else if (path === "/v1/ui/deliveries") await route.fulfill({ json: { messages: [], more: false, window_s: 604800, wait_s: 900 } });
    else if (path === "/v1/ui/doctor") await route.fulfill({ json: { hosts: [] } });
    else await route.fulfill({ status: 404 });
  });
  return state;
}

async function openAccess(page: Page) {
  await page.goto("/");
  await page.getByRole("navigation").getByRole("button", { name: "Access", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Access", exact: true })).toBeVisible();
}

async function fitsScreen(page: Page) {
  expect(await page.evaluate(() => document.documentElement.scrollWidth - document.documentElement.clientWidth)).toBeLessThanOrEqual(0);
  const clipped = await page.locator("input, select, .settings-card, .iris-access-people button").evaluateAll((elements) => elements
    .filter((element) => element.getClientRects().length > 0)
    .filter((element) => { const bounds = element.getBoundingClientRect(); return bounds.left < 0 || bounds.right > window.innerWidth; })
    .map((element) => element.tagName));
  expect(clipped).toEqual([]);
}

test("an operator grants read and send access with keyboard controls and explicit repository IDs", async ({ page }) => {
  const state = await installAccess(page);
  await openAccess(page);
  const selectMember = page.getByRole("button", { name: /^iris-member/ });
  await selectMember.focus();
  await page.keyboard.press("Enter");
  const alpha = page.getByRole("combobox", { name: alphaAccess, exact: true });
  const beta = page.getByRole("combobox", { name: betaAccess, exact: true });
  await expect(alpha).toHaveValue("");
  await expect(beta).toHaveValue("");
  await alpha.focus();
  await page.keyboard.press("ArrowDown");
  await page.keyboard.press("Tab");
  await expect(alpha).toHaveValue("ro");
  await beta.focus();
  await page.keyboard.press("End");
  await page.keyboard.press("Tab");
  await expect(beta).toHaveValue("rw");
  await fitsScreen(page);
  const save = page.getByRole("button", { name: "Save access", exact: true });
  await save.focus();
  await page.keyboard.press("Enter");
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  await expect(page.locator("#iris-access-selected")).toBeFocused();
  expect(state.writes).toEqual([{ path: "/v1/ui/accounts/ca_member/access", csrf, origin: "http://127.0.0.1:4173", body: { edit_revision: 1, role: "member", disabled: false, grants: [{ repository_id: "rp_alpha", mode: "ro" }, { repository_id: "rp_beta", mode: "rw" }] } }]);
  expect(state.errors).toEqual([]);
});

test("account roles, disabled state and no-grant controls round-trip without implicit member access", async ({ page }) => {
  const state = await installAccess(page);
  await openAccess(page);
  await page.getByRole("button", { name: /^iris-quarantined/ }).click();
  await expect(page.getByRole("combobox", { name: "Role", exact: true })).toHaveValue("none");
  await expect(page.getByRole("checkbox", { name: "Disabled", exact: true })).toBeChecked();
  await page.getByRole("combobox", { name: "Role", exact: true }).selectOption("member");
  await page.getByRole("checkbox", { name: "Disabled", exact: true }).uncheck();
  await expect(page.getByRole("combobox", { name: alphaAccess, exact: true })).toHaveValue("");
  await expect(page.getByRole("combobox", { name: betaAccess, exact: true })).toHaveValue("");
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect.poll(() => state.writes.length).toBe(1);
  expect(state.writes[0]!.body).toEqual({ edit_revision: 1, role: "member", disabled: false, grants: [] });
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  await page.getByRole("combobox", { name: alphaAccess, exact: true }).selectOption("rw");
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect.poll(() => state.writes.length).toBe(2);
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  await page.getByRole("combobox", { name: alphaAccess, exact: true }).selectOption("");
  await page.getByRole("checkbox", { name: "Disabled", exact: true }).check();
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect.poll(() => state.writes.length).toBe(3);
  expect(state.writes[2]!.body).toEqual({ edit_revision: 3, role: "member", disabled: true, grants: [] });
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  await page.getByRole("combobox", { name: "Role", exact: true }).selectOption("operator");
  await page.getByRole("checkbox", { name: "Disabled", exact: true }).uncheck();
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect.poll(() => state.writes.length).toBe(4);
  expect(state.writes[3]!.body).toEqual({ edit_revision: 4, role: "operator", disabled: false, grants: [] });
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  await page.getByRole("combobox", { name: "Role", exact: true }).selectOption("none");
  await expect(page.getByRole("checkbox", { name: "Disabled", exact: true })).toBeChecked();
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect.poll(() => state.writes.length).toBe(5);
  expect(state.writes[4]!.body).toEqual({ edit_revision: 5, role: "none", disabled: true, grants: [] });
  await fitsScreen(page);
  expect(state.errors).toEqual([]);
});

test("a stale revision refreshes grants and requires a new reviewed save", async ({ page }) => {
  const state = await installAccess(page);
  await openAccess(page);
  await page.getByRole("button", { name: /^iris-member/ }).click();
  await page.getByRole("combobox", { name: betaAccess, exact: true }).selectOption("rw");
  state.conflict = true;
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect(page.getByText("Access changed or the last enabled operator would be removed. Review current accounts before trying again.", { exact: true })).toBeVisible();
  await expect(page.locator("#iris-access-selected")).toBeFocused();
  await expect(page.getByRole("combobox", { name: alphaAccess, exact: true })).toHaveValue("ro");
  await expect(page.getByRole("combobox", { name: betaAccess, exact: true })).toHaveValue("");
  expect(state.writes).toHaveLength(1);
  expect(state.accountReads).toBeGreaterThanOrEqual(2);
  await page.getByRole("combobox", { name: betaAccess, exact: true }).selectOption("rw");
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect(page.getByText("Access saved. The account must sign in again.", { exact: true })).toBeVisible();
  expect(state.writes[1]!.body).toEqual({ edit_revision: 2, role: "member", disabled: false, grants: [{ repository_id: "rp_alpha", mode: "ro" }, { repository_id: "rp_beta", mode: "rw" }] });
  await fitsScreen(page);
  expect(state.errors).toEqual([]);
});

test("the last enabled operator stays protected and members never request the account directory", async ({ page }) => {
  const state = await installAccess(page);
  await openAccess(page);
  await page.locator("#iris-access-panel").getByRole("button", { name: /^iris-operator/ }).click();
  await expect(page.getByText("The last enabled operator cannot be disabled or demoted.", { exact: true })).toBeVisible();
  await expect(page.getByRole("combobox", { name: "Role", exact: true })).toBeDisabled();
  await expect(page.getByRole("checkbox", { name: "Disabled", exact: true })).toBeDisabled();
  expect(state.writes).toEqual([]);
  await fitsScreen(page);
  await page.getByRole("button", { name: "Sign out", exact: true }).click();
  await expect(page.getByRole("heading", { name: "Sign in to Iris", exact: true })).toBeVisible();
  state.current = state.accounts[1]!;
  await page.getByLabel("Username", { exact: true }).fill("iris-member");
  await page.getByLabel("Password", { exact: true }).fill("synthetic member password");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();
  await expect(page.getByText("No repository access has been granted. You can manage your account.", { exact: true })).toBeVisible();
  await expect(page.getByRole("navigation").getByRole("button", { name: "Access", exact: true })).toHaveCount(0);
  await expect(page.getByText("iris-quarantined", { exact: true })).toHaveCount(0);
  expect(state.accountReads).toBe(1);
  await fitsScreen(page);
  expect(state.errors).toEqual([]);
});

test("selecting a person far down a long list brings the form into view", async ({ page }) => {
  const state = await installAccess(page);
  for (let i = 0; i < 25; i++) state.accounts.push(person(`ca_${i}`, `person-${String(i).padStart(2, "0")}`, "member"));
  await openAccess(page);
  await page.getByRole("button", { name: /^person-20/ }).click();
  const heading = page.locator("#iris-access-selected");
  await expect(heading).toHaveText("Selected account: person-20");
  await expect(heading).toBeFocused();
  await expect(heading).toBeInViewport();
  await fitsScreen(page);
  expect(state.errors).toEqual([]);
});

test("a concurrent operator removal refreshes last-operator protection without signing out", async ({ page }) => {
  const state = await installAccess(page);
  const secondOperator = person("ca_second_operator", "iris-second-operator", "operator");
  state.accounts.push(secondOperator);
  await openAccess(page);
  const disabled = page.getByRole("checkbox", { name: "Disabled", exact: true });
  await expect(disabled).toBeEnabled();
  await disabled.check();
  // Another operator wins the concurrent change before this save reaches the hub.
  secondOperator.disabled = true;
  secondOperator.edit_revision++;
  await page.getByRole("button", { name: "Save access", exact: true }).click();
  await expect(page.getByText("Access changed or the last enabled operator would be removed. Review current accounts before trying again.", { exact: true })).toBeVisible();
  await expect(page.getByText("The last enabled operator cannot be disabled or demoted.", { exact: true })).toBeVisible();
  await expect(disabled).not.toBeChecked();
  await expect(disabled).toBeDisabled();
  await expect(page.getByRole("heading", { name: "Access", exact: true })).toBeVisible();
  expect(state.writes).toHaveLength(1);
  expect(state.current.disabled).toBe(false);
  await fitsScreen(page);
  expect(state.errors).toEqual([]);
});

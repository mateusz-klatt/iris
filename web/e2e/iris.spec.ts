import { expect, test, type Page } from "@playwright/test";

const silentCodex = {
  client: "codex",
  version: "0.9.0",
  status: "silent",
  detail: "No hook has arrived since install.",
  last_hook_at: "",
};

async function serve(page: Page, overview: object, clients: object[], messages: object[] = []) {
  await page.routeWebSocket("**/v1/ui/watch", () => {});
  await page.route("**/v1/ui/overview", (route) => route.fulfill({ json: overview }));
  await page.route("**/v1/ui/doctor", (route) =>
    route.fulfill({ json: { hosts: [{ host: "laptop", checked_at: new Date().toISOString(), clients }] } }),
  );
  await page.route("**/v1/ui/threads**", (route) => route.fulfill({ json: { threads: [] } }));
  await page.route("**/v1/ui/deliveries**", (route) =>
    route.fulfill({ json: { messages, window_s: 604800, wait_s: 900 } }),
  );
}

async function expectLayoutToFit(page: Page) {
  const layout = await page.evaluate(() => {
    const visible = (element: HTMLElement) => element.getClientRects().length > 0;
    const panels = [...document.querySelectorAll<HTMLElement>(".panel")].filter(visible);
    // Tables may scroll inside a panel, but the panel and the page stay bounded.
    const cut = panels.filter((panel) => {
      const tables = [...panel.querySelectorAll<HTMLElement>(":scope > table")];
      const scrolls = tables.every((table) => getComputedStyle(table).overflowX === "auto");
      return panel.scrollWidth > panel.clientWidth + 1 && !(tables.length > 0 && scrolls);
    });
    const overlaps = [...document.querySelectorAll<HTMLElement>(".host-report .message-row")].filter(visible).filter((row) => {
      const status = row.querySelector(".status")!.getBoundingClientRect();
      const detail = row.querySelector("p")!.getBoundingClientRect();
      return status.right > detail.left && status.left < detail.right && status.bottom > detail.top && status.top < detail.bottom;
    });
    return {
      width: document.documentElement.clientWidth,
      page: document.documentElement.scrollWidth,
      cut: cut.map((panel) => panel.querySelector("h1, h2")?.textContent),
      overlaps: overlaps.length,
      clippedDeliveryRows: [...document.querySelectorAll<HTMLElement>("#delivery-problems .message-row")].filter(visible)
        .filter((row) => {
          const panel = row.closest(".panel")!.getBoundingClientRect();
          const bounds = row.getBoundingClientRect();
          return row.scrollWidth > row.clientWidth + 1 || bounds.right > panel.right + 1 || bounds.left < panel.left - 1;
        }).length,
      clippedDeliveryRecipients: [...document.querySelectorAll<HTMLElement>("#delivery-problems li li")].filter(visible)
        .filter((recipient) => recipient.scrollWidth > recipient.clientWidth + 1).length,
      clippedThreadRows: [...document.querySelectorAll<HTMLElement>(".thread-row")].filter(visible)
        .filter((row) => {
          const list = row.closest(".mailbox-list")!.getBoundingClientRect();
          const bounds = row.getBoundingClientRect();
          return row.scrollWidth > row.clientWidth + 1 || bounds.right > list.right + 1 || bounds.left < list.left - 1;
        }).length,
    };
  });
  expect(layout.page).toBeLessThanOrEqual(layout.width);
  expect(layout.cut).toEqual([]);
  expect(layout.overlaps).toBe(0);
  expect(layout.clippedDeliveryRows).toBe(0);
  expect(layout.clippedDeliveryRecipients).toBe(0);
  expect(layout.clippedThreadRows).toBe(0);
}

test("waits for the first session beside a silent client", async ({ page }) => {
  await serve(page, { repositories: [], agents: [], claims: [], human_unread: 0 }, [silentCodex]);

  await page.goto("/");

  await expect(page.getByRole("status")).toHaveText("Waiting for the first session.");
  await expect(page.getByText("No hook has arrived since install.")).toBeVisible();
  await expect(page.getByText("Cause:")).toBeVisible();
});

test("delivery rows keep their subject and time inside the screen without button chrome", async ({ page }) => {
  const subject = "Review delivery to claude-home-iris-latency-20261107-224019: src/application/portfolio/a_rather_long_module_name.py";
  await serve(page, { repositories: [], agents: [], claims: [], human_unread: 0 }, [silentCodex], [{
    id: "ms_delivery",
    thread_id: "ms_delivery",
    subject,
    created_at: "2026-11-08T09:00:00Z",
    recipients: [{ name: "claude-home-iris-latency-20261107-224019", state: "pending", reason: "unreachable", execution: "lost" }],
  }]);
  await page.route("**/v1/ui/threads/ms_delivery", (route) => route.fulfill({ json: {
    id: "ms_delivery",
    messages: [{ id: "ms_delivery", sender: "human", subject, body: "Delivery is still waiting.", created_at: "2026-11-08T09:00:00Z", recipients: [{ name: "claude-home-iris-latency-20261107-224019", state: "pending" }] }],
  } }));
  await page.goto("/");
  await page.getByRole("navigation").getByRole("button", { name: /^My messages/ }).click();
  await page.getByRole("region", { name: "Delivery problems" }).locator("summary").click();
  const row = page.getByRole("region", { name: "Delivery problems" }).getByRole("button");
  await expect(row).toBeVisible();
  await expect(row).toHaveAccessibleName(new RegExp(`Open thread: ${subject}$`));

  const rowBounds = (await row.boundingBox())!;
  const viewportWidth = page.viewportSize()!.width;
  for (const element of [row, row.locator("strong"), row.locator("time"), row.locator("small")]) {
    const bounds = (await element.boundingBox())!;
    expect(bounds.width).toBeGreaterThan(0);
    expect(bounds.x).toBeGreaterThanOrEqual(rowBounds.x);
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(rowBounds.x + rowBounds.width);
    expect(bounds.x).toBeGreaterThanOrEqual(0);
    expect(bounds.x + bounds.width).toBeLessThanOrEqual(viewportWidth);
    expect(bounds.y).toBeGreaterThanOrEqual(rowBounds.y);
    expect(bounds.y + bounds.height).toBeLessThanOrEqual(rowBounds.y + rowBounds.height);
  }

  await expect(row).toHaveCSS("appearance", "none");
  await expect(row).toHaveCSS("background-color", "rgba(0, 0, 0, 0)");
  await expect(row).toHaveCSS("border-top-style", "none");
  await expect(row).toHaveCSS("border-right-style", "none");
  await expect(row).toHaveCSS("border-bottom-style", "solid");
  await expect(row).toHaveCSS("border-bottom-width", "1px");
  await expect(row).toHaveCSS("border-bottom-color", "rgb(217, 219, 234)");
  await expect(row).toHaveCSS("border-left-style", "none");
  await expect(row).toHaveCSS("text-align", "start");
  await row.hover();
  await expect(row).toHaveCSS("background-color", "rgb(250, 250, 254)");

  // Establish keyboard modality before focus; macOS WebKit's Tab order depends on OS settings.
  await page.keyboard.press("Tab");
  await row.focus();
  await expect(row).toBeFocused();
  expect(await row.evaluate((button) => button.matches(":focus-visible"))).toBe(true);
  await expect(row).toHaveCSS("outline-style", "solid");
  await expect(row).toHaveCSS("outline-width", "3px");
  await expect(row).toHaveCSS("outline-color", "rgb(109, 40, 217)");
  await expect(row).toHaveCSS("outline-offset", "-5px");
  await row.click();
  await expect(page.getByRole("heading", { name: subject, exact: true })).toBeFocused();
  await page.getByRole("button", { name: "Back to threads", exact: true }).click();
  await expect(row).toBeFocused();
});

// A busy hub, with the long names, paths and causes production has, fits the screen: nothing
// is cut off, a table wider than its panel scrolls inside it, and a status never runs into
// its detail.
test("a busy hub fits the screen", async ({ page }) => {
  const long = "claude-home-iris-latency-20261107-224019";
  const repositories = Array.from({ length: 4 }, (_, i) => ({
    id: `rp_${i}`,
    name: `repository-${i}`,
    aliases: [`gitdir:holzera:/mnt/d/projects/repository-${i}/.git`, `local:home:/home/someone/projects/repository-${i}-wt-a-long-worktree-name`],
  }));
  const agents = Array.from({ length: 6 }, (_, i) => ({
    id: `ag_${i}`,
    name: `${long}-${i}`,
    client: "claude",
    host: "laptop",
    repository_id: "rp_0",
    worktree: "/repo",
    branch: "feat/a-branch-with-a-long-name",
    state: "active",
    last_seen: "2026-11-08T09:00:00Z",
    unread: 0,
  }));
  const claims = Array.from({ length: 6 }, (_, i) => ({
    fence: i + 1,
    repository_id: "rp_0",
    path: `src/application/portfolio/a_rather_long_module_name_${i}.py`,
    kind: "exact",
    holder: `${long}-${i}`,
    origin: "auto",
    expires_at: "2026-11-08T10:57:00Z",
  }));
  const slow = {
    client: "claude",
    version: "2.1.319 (Claude Code)",
    status: "slow",
    detail: "70 shell reconciliations ran past their budget (1s, or 300ms for a hub on this machine) in 24 h, so those commands' changes went unclaimed",
    last_hook_at: "",
  };
  await serve(page, { repositories, agents, claims, human_unread: 0 }, [slow, silentCodex]);
  const subject = `Review delivery to ${long}: src/application/portfolio/a_rather_long_module_name.py`;
  await page.route("**/v1/ui/deliveries**", (route) => route.fulfill({ json: {
    window_s: 604800,
    wait_s: 900,
    messages: [{
      id: "ms_delivery",
      thread_id: "ms_delivery",
      subject,
      created_at: "2026-11-08T09:00:00Z",
      recipients: [
        { name: long, state: "pending", reason: "unreachable", execution: "lost" },
        { name: `${long}-waiting`, state: "pending", reason: "waiting", execution: "active" },
      ],
    }],
  } }));

  await page.goto("/");
  await expect(page.getByRole("heading", { name: "My messages", exact: true })).toBeVisible();
  const deliveries = page.getByRole("region", { name: "Delivery problems" });
  await deliveries.locator("summary").click();
  await expect(deliveries.getByRole("button").filter({ hasText: subject })).toBeVisible();
  await expect(deliveries).toContainText(`${long}: no live session (lost)`);
  await expect(deliveries).toContainText(`${long}-waiting: not accepted after 15 minutes`);
  await expectLayoutToFit(page);

  const navigation = page.getByRole("navigation");
  await navigation.getByRole("button", { name: /^Fleet/ }).click();
  await expect(page.getByRole("heading", { name: "Fleet", exact: true })).toBeVisible();
  await page.getByText("Technical details", { exact: true }).first().click();
  await expect(page.getByText(repositories[0]!.aliases[0]!, { exact: true })).toBeVisible();
  await expectLayoutToFit(page);
  if ((page.viewportSize()?.width ?? 0) <= 900) {
    const columns = await page.locator(".content-grid > .panel").evaluateAll((panels) =>
      new Set(panels.map((panel) => Math.round(panel.getBoundingClientRect().left))).size,
    );
    expect(columns).toBe(1);
  }

  await navigation.getByRole("button", { name: /^Claims/ }).click();
  await expect(page.getByText(claims[0]!.path, { exact: true })).toBeVisible();
  await expectLayoutToFit(page);

  await navigation.getByRole("button", { name: "Diagnostics", exact: true }).click();
  await expect(page.getByText(slow.detail)).toBeVisible();
  await expectLayoutToFit(page);
});

test("mailbox navigation opens the reader, returns focus, and separates team activity", async ({ page }) => {
  const agents = [{ id: "ag_claude", name: "claude-laptop", client: "claude", host: "laptop", repository_id: "rp_iris", worktree: "/work/iris", branch: "main", state: "active", last_seen: new Date().toISOString(), unread: 0 }];
  await serve(page, { repositories: [{ id: "rp_iris", name: "iris", aliases: [] }], agents, claims: [], human_unread: 1 }, []);
  const queries: URL[] = [];
  const myThread = { id: "ms_human", subject: "Ready for your review: src/application/portfolio/a_rather_long_module_name.py", participants: ["claude-laptop-iris-latency-20261107-224019", "human"], count: 1, last_at: "2026-11-08T10:00:00Z", unread: 1 };
  const teamThread = { id: "ms_team", subject: "Review handoff between agents", participants: ["claude-laptop", "codex-desk"], count: 3, last_at: "2026-11-08T09:00:00Z", unread: 0 };
  const additionalThreads = Array.from({ length: 48 }, (_, index) => ({ ...teamThread, id: `ms_extra_${index}`, subject: `Additional conversation ${index}` }));
  await page.route("**/v1/ui/threads**", async (route) => {
    const url = new URL(route.request().url());
    if (url.pathname.endsWith("/read")) { await route.fulfill({ json: { read: 0 } }); return; }
    if (url.pathname === "/v1/ui/threads/ms_human") {
      const body = ["Please review the local inbox preview.", ...Array.from({ length: 24 }, (_, index) => `Review detail ${index + 1}: the conversation can be longer than the screen while you read and prepare your response.`)].join("\n\n");
      await route.fulfill({ json: { id: myThread.id, messages: [{ id: myThread.id, sender: "claude-laptop", repository_id: "rp_iris", recipients: [{ name: "human", state: "pending" }], subject: myThread.subject, body, created_at: myThread.last_at }] } });
      return;
    }
    queries.push(url);
    await route.fulfill({ json: { threads: url.searchParams.get("agent") === "human" ? [myThread] : [myThread, teamThread, ...additionalThreads] } });
  });
  await page.goto("/");
  const navigation = page.getByRole("navigation");
  const mine = navigation.getByRole("button", { name: /^My messages/ });
  await expect(mine).toHaveAttribute("aria-current", "page");
  const row = page.getByRole("button", { name: /Open thread: Ready for your review/ });
  await expect(row).toBeVisible();
  // Mail is the first task: a complete conversation row is visible without scrolling.
  const firstRow = (await row.boundingBox())!;
  expect(firstRow.y + firstRow.height).toBeLessThanOrEqual(page.viewportSize()!.height);
  await expectLayoutToFit(page);
  expect(queries.at(-1)?.searchParams.get("agent")).toBe("human");
  await expect(page.getByRole("button", { name: /Open thread: Review handoff/ })).toHaveCount(0);
  await row.focus();
  await page.keyboard.press("Enter");

  const title = page.locator("#thread-title");
  await expect(title).toHaveText(myThread.subject);
  await expect(title).toBeFocused();
  await expect(page.getByText("Please review the local inbox preview.", { exact: true })).toBeVisible();
  if ((page.viewportSize()?.width ?? 0) < 700) {
    await expect(page.locator(".mailbox-list")).toBeHidden();
    await expect(page.locator(".mail-filters")).toBeHidden();
    await expect(page.locator(".console-stats")).toBeHidden();
    await expect(page.locator(".console-heading")).toBeHidden();
    await expect(page.locator(".thread-reader textarea")).toHaveCSS("font-size", "16px");
  } else {
    await expect(page.locator(".mailbox-list")).toBeVisible();
    await expect(row).toHaveAttribute("aria-current", "true");
    const listBox = await page.locator(".mailbox-list").boundingBox();
    const readerBox = await page.locator(".thread-reader").boundingBox();
    expect(listBox).not.toBeNull();
    expect(readerBox).not.toBeNull();
    expect((listBox?.x ?? 0) + (listBox?.width ?? 0)).toBeLessThanOrEqual((readerBox?.x ?? 0) + 1);
  }
  await expectLayoutToFit(page);
  // Back stays reachable after reading a long conversation, including on a phone.
  await page.locator(".thread-reader textarea").scrollIntoViewIfNeeded();
  await expect(page.getByRole("button", { name: "Back to threads", exact: true })).toBeInViewport();
  // A reader kept in page memory must not hide the heading of another view.
  await navigation.getByRole("button", { name: /^Fleet/ }).click();
  await expect(page.getByRole("heading", { name: "Fleet", exact: true })).toBeVisible();
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  await expect(page.getByRole("heading", { name: "New message", level: 1, exact: true })).toBeVisible();
  await mine.click();
  await row.click();
  await expect(title).toHaveText(myThread.subject);
  await page.locator(".thread-reader").getByRole("button", { name: "Back to threads", exact: true }).click();
  await expect(page.locator(".thread-reader")).toHaveCount(0);
  await expect(row).toBeVisible();
  await expect(row).toBeFocused();
  await expect(row).toBeInViewport();
  await expect(page.locator(".mail-filters")).toBeVisible();

  await navigation.getByRole("button", { name: "All activity", exact: true }).click();
  await expect(page.getByRole("heading", { name: "All activity", exact: true })).toBeVisible();
  await expect(page.getByRole("button", { name: /Open thread: Review handoff/ })).toBeVisible();
  expect(queries.at(-1)?.searchParams.has("agent")).toBe(false);
  await expectLayoutToFit(page);
  if (page.viewportSize()!.width >= 1101) {
    // Long lists scroll inside the mailbox; offscreen accessibility labels must
    // not leave thousands of pixels of empty page below it.
    expect(await page.evaluate(() => document.documentElement.scrollHeight)).toBeLessThanOrEqual(page.viewportSize()!.height);
  }
  await mine.click();
  await expect(page.getByRole("button", { name: /Open thread: Review handoff/ })).toHaveCount(0);
  await expect(mine).toHaveAttribute("aria-current", "page");
});

test("a long conversation title never covers the reply editor while scrolling", async ({ page }) => {
  const subject = "A long but valid subject for the review. ".repeat(24).trim();
  const agents = [{ id: "ag_claude", name: "claude-laptop", client: "claude", host: "laptop", repository_id: "rp_iris", worktree: "/work/iris", branch: "main", state: "active", last_seen: new Date().toISOString(), unread: 0 }];
  await serve(page, { repositories: [], agents, claims: [], human_unread: 0 }, []);
  await page.route("**/v1/ui/threads**", (route) => {
    const detail = { id: "ms_long", messages: [{ id: "ms_long", sender: "claude-laptop", recipients: [{ name: "human", state: "acked" }], subject, body: "A detailed update that needs a response.\n\n".repeat(30), created_at: "2026-11-08T10:00:00Z" }] };
    return route.fulfill({ json: new URL(route.request().url()).pathname.endsWith("ms_long") ? detail : { threads: [{ id: "ms_long", subject, participants: ["claude-laptop", "human"], count: 1, last_at: "2026-11-08T10:00:00Z", unread: 0 }] } });
  });
  await page.goto("/");
  const row = page.locator(".thread-row");
  await expect(row).toBeVisible();
  const rowBounds = (await row.boundingBox())!;
  expect(rowBounds.y + rowBounds.height).toBeLessThanOrEqual(page.viewportSize()!.height);
  await row.click();
  await expect(page.locator("#thread-title")).toHaveText(subject);
  const editor = page.locator(".thread-reader textarea");
  await editor.scrollIntoViewIfNeeded();
  await expect(page.getByRole("button", { name: "Back to threads", exact: true })).toBeInViewport();
  // Visibility alone misses an opaque sticky heading laid on top of the editor.
  expect(await editor.evaluate((element) => {
    const bounds = element.getBoundingClientRect();
    return document.elementFromPoint(bounds.x + bounds.width / 2, bounds.y + bounds.height / 2) === element;
  })).toBe(true);
  await editor.fill("My reply remains reachable.");
  await expect(editor).toHaveValue("My reply remains reachable.");
  await expectLayoutToFit(page);
});

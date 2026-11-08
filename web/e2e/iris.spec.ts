import { expect, test, type Page } from "@playwright/test";

const silentCodex = {
  client: "codex",
  version: "0.9.0",
  status: "silent",
  detail: "No hook has arrived since install.",
  last_hook_at: "",
};

async function serve(page: Page, overview: object, clients: object[], messages: object[] = []) {
  await page.route("**/v1/ui/overview", (route) => route.fulfill({ json: overview }));
  await page.route("**/v1/ui/doctor", (route) =>
    route.fulfill({ json: { hosts: [{ host: "laptop", checked_at: new Date().toISOString(), clients }] } }),
  );
  await page.route("**/v1/ui/threads**", (route) => route.fulfill({ json: { threads: [] } }));
  await page.route("**/v1/ui/deliveries**", (route) =>
    route.fulfill({ json: { messages, window_s: 604800, wait_s: 900 } }),
  );
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
  await page.goto("/");
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
  await expect(page.getByText(claims[0]!.path)).toBeAttached();
  const deliveries = page.getByRole("region", { name: "Delivery problems" });
  await expect(deliveries.getByRole("button").filter({ hasText: subject })).toBeVisible();
  await expect(deliveries).toContainText(`${long}: no live session (lost)`);
  await expect(deliveries).toContainText(`${long}-waiting: not accepted after 15 minutes`);

  const layout = await page.evaluate(() => {
    const width = document.documentElement.clientWidth;
    const panels = [...document.querySelectorAll<HTMLElement>(".content-grid > .panel")];
    // A panel may hold more than it shows only where a table scrolls.
    const cut = [...document.querySelectorAll<HTMLElement>(".panel")].filter((panel) => {
      const tables = [...panel.querySelectorAll<HTMLElement>(":scope > table")];
      const scrolls = tables.every((table) => getComputedStyle(table).overflowX === "auto");
      return panel.scrollWidth > panel.clientWidth + 1 && !(tables.length > 0 && scrolls);
    });
    const overlaps = [...document.querySelectorAll<HTMLElement>(".host-report .message-row")].filter((row) => {
      const status = row.querySelector(".status")!.getBoundingClientRect();
      const detail = row.querySelector("p")!.getBoundingClientRect();
      return status.right > detail.left && status.left < detail.right && status.bottom > detail.top && status.top < detail.bottom;
    });
    return {
      width,
      page: document.documentElement.scrollWidth,
      columns: new Set(panels.map((panel) => Math.round(panel.getBoundingClientRect().left))).size,
      cut: cut.map((panel) => panel.querySelector("h1, h2")?.textContent),
      overlaps: overlaps.length,
      clippedDeliveryRows: [...document.querySelectorAll<HTMLElement>("#delivery-problems .message-row")]
        .filter((row) => {
          const panel = row.closest(".panel")!.getBoundingClientRect();
          const bounds = row.getBoundingClientRect();
          return row.scrollWidth > row.clientWidth + 1 || bounds.right > panel.right + 1 || bounds.left < panel.left - 1;
        }).length,
      clippedDeliveryRecipients: [...document.querySelectorAll<HTMLElement>("#delivery-problems li li")]
        .filter((recipient) => recipient.scrollWidth > recipient.clientWidth + 1).length,
    };
  });

  expect(layout.page).toBeLessThanOrEqual(layout.width);
  expect(layout.cut).toEqual([]);
  expect(layout.overlaps).toBe(0);
  expect(layout.clippedDeliveryRows).toBe(0);
  expect(layout.clippedDeliveryRecipients).toBe(0);
  if (layout.width <= 900) {
    expect(layout.columns).toBe(1);
  }
});

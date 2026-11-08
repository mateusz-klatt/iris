import { expect, test, type Page } from "@playwright/test";

const silentCodex = {
  client: "codex",
  version: "0.9.0",
  status: "silent",
  detail: "No hook has arrived since install.",
  last_hook_at: "",
};

async function serve(page: Page, overview: object, clients: object[]) {
  await page.route("**/v1/ui/overview", (route) => route.fulfill({ json: overview }));
  await page.route("**/v1/ui/doctor", (route) =>
    route.fulfill({ json: { hosts: [{ host: "laptop", checked_at: new Date().toISOString(), clients }] } }),
  );
  await page.route("**/v1/ui/threads**", (route) => route.fulfill({ json: { threads: [] } }));
}

test("waits for the first session beside a silent client", async ({ page }) => {
  await serve(page, { repositories: [], agents: [], claims: [], human_unread: 0 }, [silentCodex]);

  await page.goto("/");

  await expect(page.getByRole("status")).toHaveText("Waiting for the first session.");
  await expect(page.getByText("No hook has arrived since install.")).toBeVisible();
  await expect(page.getByText("Cause:")).toBeVisible();
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

  await page.goto("/");
  await expect(page.getByText(claims[0]!.path)).toBeAttached();

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
    };
  });

  expect(layout.page).toBeLessThanOrEqual(layout.width);
  expect(layout.cut).toEqual([]);
  expect(layout.overlaps).toBe(0);
  if (layout.width <= 900) {
    expect(layout.columns).toBe(1);
  }
});

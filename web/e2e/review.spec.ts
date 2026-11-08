import { expect, test } from "@playwright/test";

// The review before a send lists every recipient, the subject and the rendered body. On a phone
// it must fit the screen, with long names and a wide code block, and only its button sends.
test("the review before a send fits a phone and sends only on Send", async ({ page }) => {
  const sent: unknown[] = [];
  await page.route("**/v1/ui/overview", (route) =>
    route.fulfill({
      json: {
        repositories: [{ id: "rp_iris", name: "iris", aliases: [] }],
        agents: [
          {
            id: "ag_claude",
            name: "claude-host",
            client: "claude",
            host: "local",
            repository_id: "rp_iris",
            worktree: "/work/iris",
            branch: "main",
            state: "active",
            last_seen: "2026-11-08T10:00:00Z",
            unread: 0,
          },
          {
            id: "ag_codex",
            name: "codex-a-rather-long-agent-name-on-another-host",
            client: "codex",
            host: "another-host",
            repository_id: "rp_iris",
            worktree: "/work/iris",
            branch: "main",
            state: "active",
            last_seen: "2026-11-08T10:00:00Z",
            unread: 0,
          },
        ],
        claims: [],
        human_unread: 0,
      },
    }),
  );
  await page.route("**/v1/ui/doctor", (route) => route.fulfill({ json: { hosts: [] } }));
  await page.routeWebSocket("**/v1/ui/watch", () => {});
  await page.route("**/v1/ui/deliveries**", (route) => route.fulfill({ json: { messages: [], more: false, window_s: 604800, wait_s: 900 } }));
  await page.route("**/v1/ui/threads**", (route) => route.fulfill({ json: { threads: [] } }));
  await page.route("**/v1/ui/messages", async (route) => {
    sent.push(route.request().postDataJSON());
    await route.fulfill({ status: 204 });
  });

  await page.goto("/");
  await page.getByRole("button", { name: "+ New message", exact: true }).click();
  const compose = page.locator("#compose");
  const search = compose.getByRole("searchbox", { name: "Find recipients" });
  await search.fill("claude");
  await compose.getByRole("checkbox", { name: /claude-host/ }).check();
  await search.fill("another-host");
  await compose.getByRole("checkbox", { name: /codex-a-rather-long/ }).check();
  await search.clear();
  await expect(compose.getByRole("checkbox", { name: /claude-host/ })).toBeChecked();
  await compose.getByRole("checkbox", { name: /^human/ }).check();
  await compose.getByRole("textbox", { name: "Subject", exact: true }).fill("Rollout");
  await compose
    .getByRole("textbox", { name: "Message", exact: true })
    .fill("**Ready?**\n\n```\nmake test && make check-all && go test -race ./internal/store/... ./internal/hub/...\n```");

  await compose.getByRole("button", { name: "Preview", exact: true }).click();
  await expect(compose.getByRole("region", { name: "Preview", exact: true }).locator("strong")).toHaveText("Ready?");
  await compose.getByRole("button", { name: "Edit", exact: true }).click();

  await compose.getByRole("button", { name: "Review", exact: true }).click();
  const review = compose.getByRole("region", { name: "Check before sending", exact: true });
  await expect(review.getByRole("listitem")).toHaveText(["claude-host (ag_claude)", "codex-a-rather-long-agent-name-on-another-host (ag_codex)", "human"]);
  await expect(review.locator("strong")).toHaveText("Ready?");
  expect(sent).toHaveLength(0);

  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  expect(overflow).toBeLessThanOrEqual(0);
  const box = await review.boundingBox();
  expect(box).not.toBeNull();
  expect((box?.x ?? 0) + (box?.width ?? 0)).toBeLessThanOrEqual(page.viewportSize()?.width ?? 0);

  await review.getByRole("button", { name: "Send", exact: true }).click();
  await expect(page.getByText("Sent.")).toBeVisible();
  expect(sent).toHaveLength(1);
  expect(sent[0]).toMatchObject({ to: ["ag_claude", "ag_codex", "human"] });
});

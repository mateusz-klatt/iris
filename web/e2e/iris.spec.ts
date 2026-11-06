import { expect, test } from "@playwright/test";

test("waits for the first session beside a silent client", async ({ page }) => {
  await page.route("**/v1/ui/overview", (route) =>
    route.fulfill({
      json: {
        repositories: [],
        agents: [],
        claims: [],
        human_unread: 0,
      },
    }),
  );
  await page.route("**/v1/ui/doctor", (route) =>
    route.fulfill({
      json: {
        checked_at: "2026-11-06T09:00:00Z",
        clients: [
          {
            client: "codex",
            version: "0.9.0",
            status: "silent",
            detail: "No hook has arrived since install.",
            last_hook_at: "",
          },
        ],
      },
    }),
  );
  await page.route("**/v1/ui/threads**", (route) =>
    route.fulfill({ json: { threads: [] } }),
  );

  await page.goto("/");

  await expect(page.getByRole("status")).toHaveText("Waiting for the first session.");
  await expect(page.getByText("No hook has arrived since install.")).toBeVisible();
  await expect(page.getByText("Cause:")).toBeVisible();
});

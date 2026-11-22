import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { server, sessionOverview } from "./test/server";

function assertEveryFormControlHasUniqueIdAndName(container: HTMLElement) {
  const controls = [...container.querySelectorAll<HTMLInputElement | HTMLSelectElement | HTMLTextAreaElement>("input, select, textarea")];
  expect(controls.length).toBeGreaterThan(0);
  const seen = new Set<string>();
  for (const control of controls) {
    expect(control.id, `missing id on <${control.tagName.toLowerCase()}>`).not.toBe("");
    expect(control.name, `missing name on <${control.tagName.toLowerCase()} id="${control.id}">`).not.toBe("");
    expect(seen.has(control.id), `duplicate id "${control.id}"`).toBe(false);
    seen.add(control.id);
  }
}

describe("console form controls", () => {
  it("gives every mounted input, select and textarea a unique id and name across all operator views", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/v1/ui/accounts", () =>
        HttpResponse.json({
          accounts: [
            { id: "ac_op", username: "operator", display_name: "Operator", role: "operator", disabled: false, edit_revision: 1, grants: [] },
            { id: "ac_mem", username: "alice", display_name: "Alice", role: "member", disabled: false, edit_revision: 2, grants: [{ repository_id: "rp_iris", mode: "rw" }] },
          ],
        }),
      ),
    );
    const { container } = render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();

    // Open All activity and a thread so mail filters, reply form, fleet, claims and compose are all mounted together.
    await user.click(screen.getByRole("button", { name: "All activity" }));
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    expect(await screen.findByRole("heading", { name: "Reply" })).toBeVisible();
    assertEveryFormControlHasUniqueIdAndName(container);

    // Account view (display name, language select, password inputs).
    await user.click(screen.getByRole("button", { name: "Account" }));
    expect(await screen.findByRole("heading", { level: 1, name: "Account" })).toBeVisible();
    assertEveryFormControlHasUniqueIdAndName(container);

    // Access view (role select, disabled checkbox, and member repository grant selects).
    await user.click(screen.getByRole("button", { name: "Access" }));
    await user.click(await screen.findByRole("button", { name: /alice/i }));
    expect(await screen.findByRole("combobox", { name: /iris/i })).toBeVisible();
    assertEveryFormControlHasUniqueIdAndName(container);
  });

  it("keeps unique ids when member reply and compose both mount a recipient picker", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/v1/ui/session", () =>
        HttpResponse.json({
          account: { id: "ac_mem", username: "alice", display_name: "Alice", role: "member", ui_locale: "en", correspondence_locale: "en", edit_revision: 1 },
          created_ms: 1_700_000_000_000,
          expires_ms: 1_700_043_200_000,
          csrf: "member-csrf",
        }),
      ),
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...sessionOverview,
          repositories: sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "rw" as const })),
        }),
      ),
    );
    const { container } = render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    expect(await screen.findByRole("heading", { name: "Reply" })).toBeVisible();
    assertEveryFormControlHasUniqueIdAndName(container);
  });
});

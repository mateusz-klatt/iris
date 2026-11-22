import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import type { DoctorReports } from "./api";
import { doctorReport, emptyOverview, server } from "./test/server";

// ADR 007: each host keeps its own report. A run on one machine no longer replaces another's,
// an old report fades instead of looking healthy forever, and an older doctor's report is
// shown as such.
describe("doctor reports per host", () => {
  it("shows each host apart, greys an old report and names an older doctor's", async () => {
    const fresh = new Date(Date.now() - 60 * 60 * 1000).toISOString();
    const old = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString();
    server.use(
      http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)),
      http.get("*/v1/ui/doctor", () =>
        HttpResponse.json({
          hosts: [
            { ...doctorReport, host: "mac", checked_at: fresh },
            { ...doctorReport, host: "pc", checked_at: old },
            { ...doctorReport, host: "unknown", checked_at: null },
          ],
        } satisfies DoctorReports),
      ),
    );
    render(<App connect={() => () => {}} />);
    const mac = await screen.findByRole("region", { name: "mac" });
    const pc = screen.getByRole("region", { name: "pc" });
    const unknown = screen.getByRole("region", { name: "unknown" });
    expect(within(mac).getByRole("heading", { name: "mac" })).toBeVisible();
    expect(mac.dataset.stale).toBe("no");
    expect(within(mac).queryByText(/more than a day ago/)).toBeNull();
    expect(pc.dataset.stale).toBe("yes");
    expect(within(pc).getByText(/more than a day ago/)).toBeVisible();
    expect(within(unknown).getByRole("heading", { name: "A doctor too old to name its machine" })).toBeVisible();
    expect(unknown.dataset.stale).toBe("yes");
    // No check time to show, only the note that the report is too old to trust.
    expect(within(unknown).queryByText(/^Checked (?!more)/)).toBeNull();
  });

  it("says no report is stored when no host has reported", async () => {
    server.use(
      http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)),
      http.get("*/v1/ui/doctor", () => HttpResponse.json({ hosts: [] } satisfies DoctorReports)),
    );
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(/No doctor report is stored yet/)).toBeVisible();
    expect(document.querySelector(".host-report")).toBeNull();
  });
});

// ADR 007: beside each session, live Execution telemetry stays separate from the host's last
// doctor report, which shows its check time, fades when stale, and distinguishes 24 h fail-open
// warnings from silent clients.
describe("a session's health", () => {
  it("shows live session activity separately from host doctor reports with age, staleness and fail-open distinction", async () => {
    const fresh = new Date(Date.now() - 10 * 60 * 1000).toISOString();
    const old = new Date(Date.now() - 3 * 24 * 60 * 60 * 1000).toISOString();
    const nowIso = new Date().toISOString();
    const agent = { id: "a", name: "", client: "claude", host: "", repository_id: "rp_iris", worktree: "/w", branch: "main", state: "active", last_seen: "", unread: 0 };
    server.use(
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...emptyOverview,
          repositories: [{ id: "rp_iris", name: "iris", aliases: [] }],
          agents: [
            { ...agent, id: "a1", name: "on-mac", host: "mac", last_seen: nowIso, hook_seen: nowIso, watch_seen: nowIso },
            { ...agent, id: "a2", name: "on-pc", host: "pc", last_seen: nowIso, hook_seen: nowIso },
            { ...agent, id: "a3", name: "on-mac-codex", host: "mac", client: "codex" },
            { ...agent, id: "a4", name: "on-stale-kimi", host: "holzera", client: "kimi", last_seen: nowIso, hook_seen: nowIso },
            { ...agent, id: "a5", name: "on-unknown-host", host: "unknown", client: "claude" },
          ],
        }),
      ),
      http.get("*/v1/ui/doctor", () =>
        HttpResponse.json({
          hosts: [
            { host: "mac", checked_at: fresh, clients: [{ client: "claude", version: "", status: "ok", detail: "", last_hook_at: "" }] },
            {
              host: "pc",
              checked_at: fresh,
              clients: [
                {
                  client: "claude",
                  version: "",
                  status: "silent",
                  detail: "hook failed open in the last 24 h; edits may have gone unclaimed: UserPromptSubmit session: hook deadline exceeded",
                  last_hook_at: nowIso,
                },
              ],
            },
            {
              host: "holzera",
              checked_at: old,
              clients: [{ client: "kimi", version: "2.7.2", status: "needs_human", detail: "configuration differs from what iris integrate installs", last_hook_at: nowIso }],
            },
            {
              host: "unknown",
              checked_at: null,
              clients: [{ client: "claude", version: "", status: "silent", detail: "", last_hook_at: "" }],
            },
          ],
        } satisfies DoctorReports),
      ),
    );
    render(<App connect={() => () => {}} />);
    await userEvent.setup().click(await screen.findByRole("button", { name: /^Fleet/ }));
    expect(screen.getByRole("columnheader", { name: "Session activity" })).toBeVisible();
    expect(screen.getByRole("columnheader", { name: "Host doctor" })).toBeVisible();
    const row = async (name: string) => (await screen.findByRole("rowheader", { name })).closest("tr") as HTMLElement;

    const macRow = await row("on-mac");
    expect(within(macRow).getByText(/Seen .* · Hook .* · Watch /)).toBeVisible();
    const macDoctor = macRow.querySelector("td[data-health='ok']") as HTMLElement;
    expect(macDoctor).toHaveTextContent("Hook received");
    expect(macDoctor).toHaveTextContent(/Checked /);
    expect(macDoctor.dataset.stale).toBe("no");
    expect(macDoctor.getAttribute("title")).toBeNull();

    // Active agent on a host with a 24 h fail-open warning shows live Hook telemetry AND a
    // truthful fail-open label (not "Silent" / "No response") with detail tooltip.
    const pcRow = await row("on-pc");
    expect(within(pcRow).getByText(/Seen .* · Hook /)).toBeVisible();
    const pcDoctor = pcRow.querySelector("td[data-health='silent']") as HTMLElement;
    expect(pcDoctor).toHaveTextContent("Hook failed open (24 h)");
    expect(pcDoctor.getAttribute("title")).toContain("hook failed open in the last 24 h");
    expect(pcDoctor.dataset.stale).toBe("no");

    const codexRow = await row("on-mac-codex");
    expect(within(codexRow).getByText("No activity recorded")).toBeVisible();
    expect(within(codexRow).getByText("No report from this host")).toBeVisible();

    // Active Kimi print-mode delegate shows live Seen + Hook telemetry; stale host doctor report
    // is dimmed and marked stale with its check time and detail tooltip.
    const kimiRow = await row("on-stale-kimi");
    expect(within(kimiRow).getByText(/Seen .* · Hook /)).toBeVisible();
    const kimiDoctor = kimiRow.querySelector("td[data-health='needs_human']") as HTMLElement;
    expect(kimiDoctor).toHaveTextContent("Needs a human step");
    expect(kimiDoctor).toHaveTextContent("Stale (>24 h)");
    expect(kimiDoctor.dataset.stale).toBe("yes");
    expect(kimiDoctor.getAttribute("title")).toContain("configuration differs");

    // Report without checked_at is marked stale and shows genuinely Silent when detail is empty.
    const unknownRow = await row("on-unknown-host");
    const unknownDoctor = unknownRow.querySelector("td[data-health='silent']") as HTMLElement;
    expect(unknownDoctor).toHaveTextContent("Silent");
    expect(unknownDoctor).toHaveTextContent("Stale (>24 h)");
    expect(unknownDoctor.dataset.stale).toBe("yes");
    expect(unknownDoctor).not.toHaveTextContent(/Checked /);

    // Diagnostics tab uses the same fail-open label too.
    await userEvent.setup().click(screen.getByRole("button", { name: "Diagnostics" }));
    const pcRegion = await screen.findByRole("region", { name: "pc" });
    expect(within(pcRegion).getByText("Hook failed open (24 h)")).toBeVisible();
  });
});

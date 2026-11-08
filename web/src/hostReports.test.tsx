import { render, screen, within } from "@testing-library/react";
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

// ADR 007: beside each session, the health its own host reported for its client, never another
// machine's report for the same client.
describe("a session's health", () => {
  it("comes from its own host's report, or says there is none", async () => {
    const agent = { id: "a", name: "", client: "claude", host: "", repository_id: "rp_iris", worktree: "/w", branch: "main", state: "active", last_seen: "", unread: 0 };
    server.use(
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...emptyOverview,
          repositories: [{ id: "rp_iris", name: "iris", aliases: [] }],
          agents: [
            { ...agent, id: "a1", name: "on-mac", host: "mac" },
            { ...agent, id: "a2", name: "on-pc", host: "pc" },
            { ...agent, id: "a3", name: "on-mac-codex", host: "mac", client: "codex" },
          ],
        }),
      ),
      http.get("*/v1/ui/doctor", () =>
        HttpResponse.json({
          hosts: [
            { host: "mac", checked_at: new Date().toISOString(), clients: [{ client: "claude", version: "", status: "ok", detail: "", last_hook_at: "" }] },
            { host: "pc", checked_at: new Date().toISOString(), clients: [{ client: "claude", version: "", status: "silent", detail: "", last_hook_at: "" }] },
          ],
        } satisfies DoctorReports),
      ),
    );
    render(<App connect={() => () => {}} />);
    const row = async (name: string) => (await screen.findByRole("rowheader", { name })).closest("tr") as HTMLElement;
    expect(within(await row("on-mac")).getByText("Hook received")).toBeVisible();
    expect(within(await row("on-pc")).getByText("Silent")).toBeVisible();
    expect(within(await row("on-mac-codex")).getByText("No report from this host")).toBeVisible();
  });
});

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

import { render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { doctorReport, emptyOverview, server } from "./test/server";

// "slow" is doctor's grade for a client whose hooks run but whose shell reconciliation often
// passes its budget, so changes go unclaimed. The console labels it and shows its cause.
describe("doctor's slow grade", () => {
  it("labels a slow client and shows why", async () => {
    server.use(
      http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)),
      http.get("*/v1/ui/doctor", () =>
        HttpResponse.json({
          hosts: [{ ...doctorReport, clients: [{ client: "claude", version: "2.1.0", status: "slow", detail: "11 shell reconciliations ran past their budget", last_hook_at: "" }] }],
        }),
      ),
    );
    render(<App connect={() => () => {}} />);
    const slow = await screen.findByText("Slow");
    expect(slow).toBeVisible();
    const row = document.querySelector("[data-status='slow']");
    expect(row).not.toBeNull();
    expect(row).toHaveTextContent("Cause:");
    expect(row).toHaveTextContent("11 shell reconciliations ran past their budget");
  });
});

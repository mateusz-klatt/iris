import { render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";

import { App } from "./App";
import { doctorReport, emptyOverview, server } from "./test/server";

// The hub omits a client's empty version, detail and last hook time. The console must still
// render the row instead of failing on the missing field.
describe("a doctor row with omitted fields", () => {
  it("shows the client and says it has no version", async () => {
    server.use(
      http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)),
      http.get("*/v1/ui/doctor", () =>
        HttpResponse.json({ hosts: [{ ...doctorReport, clients: [{ client: "cursor", status: "not_configured" }] }] }),
      ),
    );
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("cursor")).toBeVisible();
    const row = document.querySelector("[data-client='cursor']");
    expect(row).not.toBeNull();
    expect(row).toHaveTextContent("Version: None yet.");
  });
});

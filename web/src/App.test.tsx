import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import type { WatchFrame, WatchStatus } from "./api";
import i18n from "./i18n";
import { soundPreferenceKey } from "./notificationSound";
import { uiLocalePreferenceKey } from "./preferences";
import { emptyOverview, server, setHumanUnread } from "./test/server";

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      values.set(key, value);
    },
    read: (key: string) => values.get(key) ?? null,
  };
}

beforeEach(async () => {
  document.body.dataset.humanUnread = "";
  delete document.body.dataset.humanUnread;
  document.documentElement.lang = "en";
  await i18n.changeLanguage("en");
});

afterEach(() => {
  vi.unstubAllGlobals();
});

async function showSession() {
  render(<App connect={() => () => {}} />);
  expect(await screen.findByRole("heading", { name: "Clients" })).toBeVisible();
}

describe("Iris operator console", () => {
  it("shows repositories, agents, and claims from the overview", async () => {
    await showSession();
    expect(screen.getByRole("rowheader", { name: "iris" })).toBeVisible();
    expect(screen.getByRole("rowheader", { name: "claude-host" })).toBeVisible();
    expect(screen.getByRole("rowheader", { name: "docs/api.md" })).toBeVisible();
    expect(screen.getByText("Hook received.")).toBeVisible();
    expect(screen.getAllByText("1 unread").length).toBeGreaterThan(0);
  });

  it("shows a live wait and the silent client's cause when nothing is in session", async () => {
    server.use(http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)));
    render(<App connect={() => () => {}} />);

    expect(await screen.findByRole("status")).toHaveTextContent("Waiting for the first session.");
    const silent = document.querySelector("[data-status='silent']");
    expect(silent).not.toBeNull();
    expect(silent).toHaveTextContent("Cause:");
    expect(silent).toHaveTextContent("No hook has arrived since install.");
    expect(screen.queryByRole("rowheader", { name: "claude-host" })).toBeNull();
    expect(screen.getByText("Not configured")).toBeVisible();
    expect(screen.getByText("Needs a human step")).toBeVisible();
  });

  it("points an unsigned browser at iris ui", async () => {
    server.use(
      http.get("*/v1/ui/overview", () => new HttpResponse(null, { status: 401 })),
      http.get("*/v1/ui/doctor", () => new HttpResponse(null, { status: 401 })),
    );
    render(<App connect={() => () => {}} />);

    expect(await screen.findByRole("heading", { name: "Iris" })).toBeVisible();
    expect(screen.getByText(/run iris ui and open the link/)).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Clients" })).toBeNull();
  });

  it("revokes a claim and drops it from the next overview", async () => {
    const user = userEvent.setup();
    await showSession();
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => {
      expect(screen.queryByRole("rowheader", { name: "docs/api.md" })).toBeNull();
    });
  });

  it("searches threads, reads one, and keeps message bodies as text", async () => {
    const user = userEvent.setup();
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Rollout check" });

    expect(within(detail).getByRole("heading", { name: "Release" })).toBeVisible();
    expect(document.querySelector("script")).toBeNull();
    expect(within(detail).getByRole("link", { name: "note" })).toHaveAttribute(
      "href",
      "https://example.test/note",
    );
    expect(within(detail).getByText("blocked").closest("a")).toBeNull();
    expect(detail.querySelector("script")).toBeNull();
    expect(within(detail).getAllByText(/<script>alert\(1\)<\/script>/).length).toBeGreaterThan(0);

    await user.click(screen.getByRole("button", { name: "Back to threads" }));
    await user.type(screen.getByRole("textbox", { name: "Search threads" }), "missing");
    await user.click(screen.getByRole("button", { name: "Search" }));
    expect(await screen.findByText("No threads in this view.")).toBeVisible();
  }, 20000);

  it("replies and sends as human", async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Rollout check" });
    const replyBody = within(detail).getByRole("textbox", { name: "Message" });
    await user.type(replyBody, "Noted.");
    await user.click(screen.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({
      to: ["claude-host"],
      subject: "Re: Rollout check",
      body: "Noted.",
      reply_to: "ms_root",
    });
    expect(bodies[0]).toMatchObject({ key: expect.stringMatching(/^human:/) });

    const compose = document.getElementById("compose");
    if (compose === null) {
      throw new Error("missing compose");
    }
    const view = within(compose);
    await user.type(view.getByRole("textbox", { name: "Recipients" }), "claude-host, human");
    await user.type(view.getByRole("textbox", { name: "Subject" }), "Check in");
    await user.type(view.getByRole("textbox", { name: "Message" }), "Are you there?");
    await user.click(view.getByRole("button", { name: "Send" }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toMatchObject({
      to: ["claude-host", "human"],
      subject: "Check in",
      body: "Are you there?",
      reply_to: "",
    });
    expect(await screen.findByText("Sent.")).toBeVisible();
  }, 20000);

  it("refreshes overview and the open thread when the watch reports a change", async () => {
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    const connect = (frame: (value: WatchFrame) => void, status?: (value: WatchStatus) => void) => {
      onFrame = frame;
      status?.("live");
      return () => undefined;
    };
    render(<App connect={connect} />);
    expect(await screen.findByText("Live updates connected")).toBeVisible();
    expect(screen.getAllByText("1 unread").length).toBeGreaterThan(0);

    setHumanUnread(3);
    onFrame?.({ cursor: 124, kind: "changed" });
    expect(await screen.findByText("3 unread")).toBeVisible();
  });

  it("renders the hub's real empty report and claims with no expiry", async () => {
    server.use(
      http.get("*/v1/ui/doctor", () => HttpResponse.json({ checked_at: null, clients: [] })),
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...emptyOverview,
          claims: [
            {
              fence: 1,
              repository_id: "rp_1",
              path: "docs/",
              kind: "prefix",
              holder: "codex-host-api",
              origin: "explicit",
              expires_at: null,
            },
          ],
        }),
      ),
    );
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("docs/")).toBeVisible();
    expect(screen.getByText(/No doctor report is stored yet. Run iris doctor/)).toBeVisible();
  });

  it("keeps the doctor hint when the report is missing", async () => {
    server.use(http.get("*/v1/ui/doctor", () => new HttpResponse(null, { status: 404 })));
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(/No doctor report is stored yet. Run iris doctor/)).toBeVisible();
    expect(screen.getByText(/Run iris doctor on this computer to refresh it/)).toBeVisible();
  });

  it("remembers the interface language and the notification sound on this computer", async () => {
    const user = userEvent.setup();
    const stored = memoryStorage();
    vi.stubGlobal("localStorage", stored);
    vi.stubGlobal("AudioContext", undefined);
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "Clients" });

    await user.click(
      screen.getByRole("button", { name: "Notification sound off. Click to unmute." }),
    );
    expect(stored.read(soundPreferenceKey)).toBe("on");

    await user.click(screen.getByRole("button", { name: /Choose language/ }));
    await user.click(screen.getByRole("option", { name: "Use Polski" }));
    await waitFor(() => {
      expect(stored.read(uiLocalePreferenceKey)).toBe("pl");
      expect(document.documentElement.lang).toBe("pl");
    });
    vi.unstubAllGlobals();
    await i18n.changeLanguage("en");
  });
});

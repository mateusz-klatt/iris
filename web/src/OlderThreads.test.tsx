import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { describe, expect, it } from "vitest";
import { App } from "./App";
import type { ThreadSummary, ThreadsPage, WatchFrame, WatchStatus } from "./api";
import { server } from "./test/server";

function summary(id: string): ThreadSummary {
  return { id, subject: `Subject ${id}`, participants: ["claude-host", "human"], count: 1, last_at: "2026-11-06T10:05:00Z", unread: 0 };
}

// serveThreads pages the hub's thread list two at a time, newest first, as the hub does with
// its cursor; `order` can change between requests, as new mail would change it.
function serveThreads(initial: string[]) {
  const state = { order: initial, requests: [] as string[] };
  server.use(
    http.get("*/v1/ui/threads", ({ request }) => {
      const url = new URL(request.url);
      const before = url.searchParams.get("before") ?? "";
      state.requests.push(before);
      const start = before === "" ? 0 : state.order.indexOf(before) + 1;
      const ids = state.order.slice(start, start + 2);
      const page: ThreadsPage = { threads: ids.map(summary) };
      if (start + 2 < state.order.length) {
        page.next = ids[ids.length - 1];
      }
      return HttpResponse.json(page);
    }),
  );
  return state;
}

function shown(): string[] {
  return screen.queryAllByRole("button", { name: /^Subject t\d/ }).map((button) => within(button).getByText(/^Subject/).textContent?.replace("Subject ", "") ?? "");
}

function watched() {
  const feed: { send?: (frame: WatchFrame) => void } = {};
  const connect = (frame: (value: WatchFrame) => void, status?: (value: WatchStatus) => void) => {
    feed.send = frame;
    status?.("live");
    return () => undefined;
  };
  return { feed, connect };
}

// Gap review A15: older history stays reachable by browsing, and live refreshes leave no gap.
describe("older threads", () => {
  it("opens older pages until there are none", async () => {
    const user = userEvent.setup({ delay: null });
    serveThreads(["t1", "t2", "t3", "t4", "t5"]);
    render(<App connect={() => () => {}} />);
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    await user.click(screen.getByRole("button", { name: "Older threads" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2", "t3", "t4"]));
    await user.click(screen.getByRole("button", { name: "Older threads" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2", "t3", "t4", "t5"]));
    expect(screen.queryByRole("button", { name: "Older threads" })).toBeNull();
  });

  it("refreshes every opened page, so new mail pushes no thread into a gap", async () => {
    const user = userEvent.setup({ delay: null });
    const hub = serveThreads(["t1", "t2", "t3", "t4", "t5"]);
    const { feed, connect } = watched();
    render(<App connect={connect} />);
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    await user.click(screen.getByRole("button", { name: "Older threads" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2", "t3", "t4"]));
    // New mail lifts t4 to the top: t2 now ends the first page and t3 the second.
    hub.order = ["t4", "t1", "t2", "t3", "t5"];
    feed.send?.({ cursor: 1, kind: "changed" });
    await waitFor(() => expect(shown()).toEqual(["t4", "t1", "t2", "t3"]));
    // With fewer threads than the opened pages hold, the walk stops at the last one.
    hub.order = ["t1", "t2"];
    const before = hub.requests.length;
    feed.send?.({ cursor: 2, kind: "changed" });
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    expect(hub.requests.slice(before)).toEqual([""]);
    expect(screen.queryByRole("button", { name: "Older threads" })).toBeNull();
  });

  it("keeps a thread once when it moves up between two pages", async () => {
    const user = userEvent.setup({ delay: null });
    const hub = serveThreads(["t1", "t2", "t3", "t4"]);
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        const before = new URL(request.url).searchParams.get("before") ?? "";
        hub.requests.push(before);
        // The cursor's thread got mail after the first page: the second repeats t1.
        if (before === "t2") {
          return HttpResponse.json({ threads: [summary("t1"), summary("t3")] } satisfies ThreadsPage);
        }
        return HttpResponse.json({ threads: [summary("t1"), summary("t2")], next: "t2" } satisfies ThreadsPage);
      }),
    );
    render(<App connect={() => () => {}} />);
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    await user.click(screen.getByRole("button", { name: "Older threads" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2", "t3"]));
  });

  it("goes back to one page when the filters change", async () => {
    const user = userEvent.setup({ delay: null });
    const hub = serveThreads(["t1", "t2", "t3", "t4", "t5"]);
    render(<App connect={() => () => {}} />);
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    await user.click(screen.getByRole("button", { name: "Older threads" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2", "t3", "t4"]));
    await user.type(screen.getByRole("textbox", { name: "Search threads" }), "rollout");
    await user.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(shown()).toEqual(["t1", "t2"]));
    expect(hub.requests.at(-1)).toBe("");
  });
});

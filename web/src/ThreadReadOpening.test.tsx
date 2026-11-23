import { act, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import { App } from "./App";
import * as api from "./api";
import type { ThreadDetail, WatchFrame } from "./api";
import { englishTranslation, loadLocale } from "./i18n";
import { server, sessionOverview, threadDetail, threadsPage } from "./test/server";

let changeVisibility: (state: DocumentVisibilityState) => void;

beforeEach(async () => {
  await loadLocale("en");
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  changeVisibility = (state) => {
    visibility.mockReturnValue(state);
    act(() => document.dispatchEvent(new Event("visibilitychange")));
  };
});

afterEach(() => { vi.restoreAllMocks(); });

const original: ThreadDetail = {
  ...threadDetail,
  messages: [{ ...threadDetail.messages[0]!, body: "Original detail response." }],
};
const refreshed: ThreadDetail = {
  ...original,
  high_water_message_id: "ms_new",
  // The insertion-order watermark need not be the last message in display order.
  messages: [
    { ...original.messages[0]!, id: "ms_new", reply_to: "ms_root", body: "Fresh detail response." },
    ...original.messages,
  ],
};
const other: ThreadDetail = {
  id: "ms_other",
  high_water_message_id: "ms_other",
  messages: [{ ...original.messages[0]!, id: "ms_other", subject: "Other thread", body: "Other thread response." }],
};

function deferredDetail() {
  let resolve!: (detail: ThreadDetail) => void;
  let reject!: (error: Error) => void;
  const promise = new Promise<ThreadDetail>((done, fail) => { resolve = done; reject = fail; });
  return { promise, resolve, reject };
}

function show() {
  const requests: ReturnType<typeof deferredDetail>[] = [];
  const reads: { id: string; body: unknown }[] = [];
  const renderedReads: { watermark: string; text: string | null | undefined; visibility: DocumentVisibilityState }[] = [];
  const readThread = api.readThread;
  const readCalls = vi.spyOn(api, "readThread").mockImplementation((...args) => {
    renderedReads.push({ watermark: args[2], text: document.querySelector(".thread-reader")?.textContent, visibility: document.visibilityState });
    return readThread(...args);
  });
  let onFrame!: (frame: WatchFrame) => void;
  let cursor = 0;
  vi.spyOn(api, "getThread").mockImplementation(() => {
    const request = deferredDetail();
    requests.push(request);
    return request.promise;
  });
  server.use(
    http.get("*/v1/ui/threads", () => HttpResponse.json({ threads: [
      ...threadsPage.threads,
      { ...threadsPage.threads[0]!, id: "ms_other", subject: "Other thread" },
    ] })),
    http.post("*/v1/ui/threads/:id/read/:message", async ({ params, request }) => {
      const body = await request.json();
      expect(body).toMatchObject({ last_message_id: params.message });
      reads.push({ id: String(params.id), body });
      return HttpResponse.json({ read: 0 });
    }),
  );
  const { unmount } = render(<App refreshGap={0} connect={(frame) => { onFrame = frame; return () => {}; }} />);
  return {
    requests,
    reads,
    readCalls,
    renderedReads,
    unmount,
    refresh: () => { act(() => onFrame({ cursor: ++cursor, kind: "changed" })); },
    finish: async (index: number, detail: ThreadDetail) => {
      const request = requests[index]!;
      await act(async () => {
        request.resolve(detail);
        await request.promise;
      });
    },
    fail: async (index: number) => {
      const request = requests[index]!;
      await act(async () => {
        request.reject(new Error("Detail request failed"));
        await request.promise.catch(() => {});
      });
    },
  };
}

it.each([
  ["newer", "opening"],
  ["newer", "refresh"],
  ["older", "opening"],
  ["older", "refresh"],
] as const)("acknowledges the rendered snapshot on resume when the opening is %s and %s responds first", async (openingAge, first) => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  changeVisibility("hidden");

  const details = openingAge === "newer" ? [refreshed, original] : [original, refreshed];
  const firstIndex = first === "opening" ? 0 : 1;
  await view.finish(firstIndex, details[firstIndex]!);
  await view.finish(1 - firstIndex, details[1 - firstIndex]!);
  const rendered = details[1 - firstIndex]!;
  expect(screen.queryByText("Fresh detail response.") !== null).toBe(rendered === refreshed);
  expect(view.readCalls).not.toHaveBeenCalled();

  changeVisibility("visible");
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: rendered.high_water_message_id } },
  ]));
  expect(view.renderedReads).toEqual([
    { watermark: rendered.high_water_message_id, text: expect.stringContaining("Original detail response."), visibility: "visible" },
  ]);
  expect(view.renderedReads[0]!.text?.includes("Fresh detail response.")).toBe(rendered === refreshed);

  // The resume's refresh and further visibility events cannot acknowledge again.
  await waitFor(() => expect(view.requests).toHaveLength(3));
  await view.finish(2, refreshed);
  changeVisibility("hidden");
  changeVisibility("visible");
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it("acknowledges visible detail only after its reader commits, once per opening", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  await view.finish(0, refreshed);
  await waitFor(() => expect(view.reads).toHaveLength(1));
  expect(view.renderedReads).toEqual([
    { watermark: "ms_new", text: expect.stringContaining("Fresh detail response."), visibility: "visible" },
  ]);

  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await view.finish(1, original);
  expect(screen.queryByText("Fresh detail response.")).toBeNull();
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it("acknowledges only the final snapshot when visible responses are batched into one render", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await act(async () => {
    view.requests[0]!.resolve(refreshed);
    view.requests[1]!.resolve(original);
    await Promise.all(view.requests.map((request) => request.promise));
  });
  expect(screen.getByText("Original detail response.")).toBeVisible();
  expect(screen.queryByText("Fresh detail response.")).toBeNull();
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_root" } },
  ]));
  expect(view.renderedReads).toEqual([
    { watermark: "ms_root", text: expect.stringContaining("Original detail response."), visibility: "visible" },
  ]);
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it.each(["original", "replacement"] as const)("acknowledges the first successful response for an opening when the %s request finishes first", async (first) => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));

  const firstIndex = first === "original" ? 0 : 1;
  const expectedWatermark = first === "original" ? "ms_root" : "ms_new";
  await view.finish(firstIndex, firstIndex === 0 ? original : refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: expectedWatermark } },
  ]));
  await view.finish(1 - firstIndex, firstIndex === 0 ? refreshed : original);
  expect(screen.getByText("Fresh detail response.")).toBeVisible();
  expect(view.readCalls).toHaveBeenCalledTimes(1);

  // A later passive refresh updates the display but cannot advance this opening's watermark.
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(3));
  await view.finish(2, {
    ...refreshed,
    high_water_message_id: "ms_later",
    messages: [...refreshed.messages, { ...original.messages[0]!, id: "ms_later", body: "Later passive response." }],
  });
  expect(screen.getByText("Later passive response.")).toBeVisible();
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it("keeps opening authorization when an older request fails after a replacement starts", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await view.fail(0);
  expect(view.readCalls).not.toHaveBeenCalled();
  expect(screen.queryByRole("alert")).toBeNull();
  await view.finish(1, refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_new" } },
  ]));
});

it("does not acknowledge a pending opening after the operator opens another thread", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  await user.click(screen.getByRole("button", { name: /Open thread: Other thread/ }));
  await waitFor(() => expect(view.requests).toHaveLength(2));

  await view.finish(0, original);
  expect(view.readCalls).not.toHaveBeenCalled();
  expect(screen.queryByText("Original detail response.")).toBeNull();
  await view.finish(1, other);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_other", body: { repository_id: "", last_message_id: "ms_other" } },
  ]));
  expect(screen.getByText("Other thread response.")).toBeVisible();
});

it("does not let an earlier generation consume authorization when the same thread is reopened", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  const firstRow = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(firstRow);
  await waitFor(() => expect(view.requests).toHaveLength(1));
  await user.click(screen.getByRole("button", { name: /Open thread: Other thread/ }));
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await user.click(firstRow);
  await waitFor(() => expect(view.requests).toHaveLength(3));

  await view.finish(0, original);
  await view.finish(1, other);
  expect(view.readCalls).not.toHaveBeenCalled();
  expect(screen.queryByText("Original detail response.")).toBeNull();
  expect(screen.queryByText("Other thread response.")).toBeNull();
  await view.finish(2, refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_new" } },
  ]));
});

it.each(["back", "view", "unmount"] as const)("cancels a pending opening when closed through %s", async (close) => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  if (close === "back") {
    await user.click(screen.getByRole("button", { name: "Back to threads" }));
  } else if (close === "view") {
    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
  } else {
    view.unmount();
  }

  await view.finish(0, original);
  expect(view.readCalls).not.toHaveBeenCalled();
  expect(screen.queryByText("Original detail response.")).toBeNull();
});

it("retains authorization when tab resume refreshes a still-pending opening", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  changeVisibility("hidden");
  changeVisibility("visible");
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await view.finish(1, refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_new" } },
  ]));
  await view.finish(0, original);
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it("retains authorization after empty detail until a response supplies a high-water message", async () => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  await view.finish(0, { id: "ms_root", messages: [] });
  expect(view.readCalls).not.toHaveBeenCalled();
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  await view.finish(1, refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_new" } },
  ]));
});

it.each([undefined, ""])("retains authorization when the opening first receives a missing watermark (%s)", async (highWater) => {
  const user = userEvent.setup({ delay: null });
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));
  await view.finish(0, { ...original, high_water_message_id: highWater });
  expect(screen.getByRole("alert")).toHaveTextContent(englishTranslation.operator.readReload);
  expect(view.readCalls).not.toHaveBeenCalled();

  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  expect(screen.getByRole("alert")).toHaveTextContent(englishTranslation.operator.readReload);
  await view.finish(1, refreshed);
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_new" } },
  ]));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

it("defers a superseded opening response's watermark until overview recovery reveals the reader", async () => {
  const user = userEvent.setup({ delay: null });
  let overviewFails = false;
  server.use(http.get("*/v1/ui/overview", () => overviewFails
    ? new HttpResponse(null, { status: 500 })
    : HttpResponse.json(sessionOverview)));
  const view = show();
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await waitFor(() => expect(view.requests).toHaveLength(1));

  overviewFails = true;
  view.refresh();
  await waitFor(() => expect(view.requests).toHaveLength(2));
  expect(await screen.findByRole("alert")).toHaveTextContent("could not load");
  await view.finish(0, original);
  expect(view.readCalls).not.toHaveBeenCalled();
  expect(screen.queryByText("Original detail response.")).toBeNull();

  overviewFails = false;
  await user.click(screen.getByRole("button", { name: "Try again" }));
  await waitFor(() => expect(view.requests).toHaveLength(3));
  expect(screen.getByText("Original detail response.")).toBeVisible();
  await waitFor(() => expect(view.reads).toEqual([
    { id: "ms_root", body: { repository_id: "", last_message_id: "ms_root" } },
  ]));

  await view.finish(1, refreshed);
  expect(screen.queryByText("Fresh detail response.")).toBeNull();
  await view.finish(2, {
    ...refreshed,
    high_water_message_id: "ms_later",
    messages: [...refreshed.messages, { ...original.messages[0]!, id: "ms_later", body: "Recovered detail response." }],
  });
  expect(screen.getByText("Recovered detail response.")).toBeVisible();
  expect(view.readCalls).toHaveBeenCalledTimes(1);
});

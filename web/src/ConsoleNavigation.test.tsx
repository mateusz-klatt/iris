import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { StrictMode } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { App } from "./App";
import type { AccountSession } from "./accountApi";
import type { DeliveryProblems, WatchFrame } from "./api";
import i18n from "./i18n";
import { doctorReport, emptyOverview, operatorSession, server, sessionOverview, threadDetail, threadsPage } from "./test/server";

beforeEach(async () => { await i18n.changeLanguage("en"); });
afterEach(() => { vi.restoreAllMocks(); });

function mockVisibility(initial: DocumentVisibilityState) {
  const visibility = vi.spyOn(document, "visibilityState", "get").mockReturnValue(initial);
  return (next: DocumentVisibilityState) => {
    visibility.mockReturnValue(next);
    act(() => document.dispatchEvent(new Event("visibilitychange")));
  };
}

function observeThreadReads(fetch = vi.spyOn(globalThis, "fetch")) {
  // Observe dispatch synchronously while MSW still handles the requests.
  return () => fetch.mock.calls.filter(([url, init]) => init?.method === "POST" && /\/read\/[^/]+$/.test(String(url)));
}

it("starts with operator conversations, keeps fleet activity separate and exposes shortcuts", async () => {
  const user = userEvent.setup();
  const requests: string[] = [];
  server.use(http.get("*/v1/ui/threads", ({ request }) => {
    requests.push(new URL(request.url).searchParams.get("agent") ?? "");
    return HttpResponse.json(threadsPage);
  }));
  render(<App connect={() => () => {}} />);
  expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
  await waitFor(() => expect(requests).toContain("human"));
  expect(screen.queryByRole("heading", { name: "Clients" })).toBeNull();
  expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull();
  await user.click(screen.getByRole("button", { name: "All activity" }));
  await waitFor(() => expect(requests.at(-1)).toBe(""));
  await user.click(screen.getByRole("button", { name: /Unread for you/ }));
  await waitFor(() => expect(requests.at(-1)).toBe("human"));
  await user.click(screen.getByRole("button", { name: /Active agents/ }));
  expect(screen.getByRole("heading", { name: "Fleet" })).toBeVisible();
  await user.click(screen.getByRole("link", { name: "Iris" }));
  expect(screen.getByRole("heading", { name: "My messages" })).toBeVisible();
  await user.click(screen.getByRole("button", { name: /File reservations/ }));
  expect(screen.getByText("docs/api.md")).toBeVisible();
});

it("filters the fleet by activity and searchable context without losing inactive records", async () => {
  const user = userEvent.setup();
  server.use(http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, agents: [
    ...sessionOverview.agents,
    { ...sessionOverview.agents[0], id: "ag_old", name: "codex-laptop", state: "ended", host: "laptop", branch: "archive" },
  ] })));
  render(<App connect={() => () => {}} />);
  await user.click(await screen.findByRole("button", { name: /Fleet/ }));
  const table = screen.getByRole("table", { name: "Agents" });
  expect(within(table).getByRole("rowheader", { name: "claude-host" })).toBeVisible();
  expect(within(table).queryByRole("rowheader", { name: "codex-laptop" })).toBeNull();
  await user.click(screen.getByRole("checkbox", { name: "Include inactive agents" }));
  expect(within(table).getByRole("rowheader", { name: "codex-laptop" })).toBeVisible();
  const search = screen.getByRole("searchbox", { name: "Find an agent" });
  await user.type(search, "archive");
  expect(within(table).queryByRole("rowheader", { name: "claude-host" })).toBeNull();
  expect(within(table).getByRole("rowheader", { name: "codex-laptop" })).toBeVisible();
  await user.type(search, " missing");
  expect(screen.getByText("No agents match these filters.")).toBeVisible();
  await user.click(screen.getByText("Technical details"));
  expect(screen.getByText("iris-wt")).toBeVisible();
});

it("retries a failed conversation and returns keyboard focus to its list row", async () => {
  const user = userEvent.setup();
  let fails = true;
  server.use(http.get("*/v1/ui/threads/:id", () => fails
    ? new HttpResponse(null, { status: 500 }) : HttpResponse.json(threadDetail)));
  render(<App connect={() => () => {}} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  expect(await screen.findByRole("alert")).toHaveTextContent("could not load");
  fails = false;
  await user.click(screen.getByRole("button", { name: "Try again" }));
  expect(await screen.findByRole("heading", { name: "Rollout check" })).toHaveFocus();
  await user.click(screen.getByRole("button", { name: "Back to threads" }));
  expect(row).toHaveFocus();
  expect(screen.getByText("Choose a conversation")).toBeVisible();
});

it("keeps the conversation, edited reply and review when its selected row is opened again", async () => {
  const user = userEvent.setup();
  render(<App connect={() => () => {}} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  const heading = await screen.findByRole("heading", { name: "Rollout check" });
  const body = screen.getByRole("textbox", { name: "Message" });
  await user.type(body, "Please keep these notes.");
  await user.click(screen.getByRole("button", { name: "Review" }));
  expect(screen.getByRole("button", { name: "Reply" })).toHaveFocus();
  await user.click(row);
  expect(heading).toHaveFocus();
  expect(body).toHaveValue("Please keep these notes.");
  expect(screen.getByRole("heading", { name: "Check before sending" })).toBeVisible();
});

it("keeps focus in an edited reply when a live refresh fails and recovers", async () => {
  const user = userEvent.setup();
  let fails = false;
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  server.use(http.get("*/v1/ui/threads/:id", () => fails
    ? new HttpResponse(null, { status: 500 }) : HttpResponse.json(threadDetail)));
  render(<App refreshGap={0} connect={(frame) => { onFrame = frame; return () => {}; }} />);
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  const body = await screen.findByRole("textbox", { name: "Message" });
  await user.type(body, "Keep typing through live updates");
  fails = true;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  expect(await screen.findByRole("alert")).toHaveTextContent("could not load");
  expect(body).toHaveFocus();
  fails = false;
  act(() => onFrame?.({ cursor: 3, kind: "changed" }));
  await waitFor(() => expect(screen.queryByRole("alert")).toBeNull());
  expect(body).toHaveFocus();
  expect(body).toHaveValue("Keep typing through live updates");
});

it("leaves mail unread when a watch refresh recovers a failed open", async () => {
  const user = userEvent.setup({ delay: null });
  mockVisibility("visible");
  const reads = observeThreadReads();
  let fails = true;
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  const arrived = { ...threadDetail.messages[0]!, id: "ms_reply", body: "Mail arrived after the failed open." };
  server.use(http.get("*/v1/ui/threads/:id", () => fails
    ? new HttpResponse(null, { status: 500 })
    : HttpResponse.json({ ...threadDetail, messages: [...threadDetail.messages, arrived] })));
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  expect(await screen.findByRole("alert")).toHaveTextContent("could not load");
  expect(reads()).toHaveLength(0);

  fails = false;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  expect(await screen.findByText(arrived.body)).toBeVisible();
  expect(reads()).toHaveLength(0);

  await user.click(row);
  await waitFor(() => expect(reads()).toHaveLength(1));
});

it("leaves mail unread when a watch refresh supersedes a failing open", async () => {
  const user = userEvent.setup({ delay: null });
  mockVisibility("visible");
  const fetch = vi.spyOn(globalThis, "fetch");
  const reads = observeThreadReads(fetch);
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let finishOpen!: () => void;
  let finishRefresh!: () => void;
  const openReady = new Promise<void>((resolve) => { finishOpen = resolve; });
  const refreshReady = new Promise<void>((resolve) => { finishRefresh = resolve; });
  const arrived = { ...threadDetail.messages[0]!, id: "ms_reply", body: "Unread mail from the watch refresh." };
  let detailRequests = 0;
  server.use(http.get("*/v1/ui/threads/:id", async () => {
    detailRequests++;
    if (detailRequests === 1) {
      await openReady;
      return new HttpResponse(null, { status: 500 });
    }
    await refreshReady;
    return HttpResponse.json({ ...threadDetail, messages: [...threadDetail.messages, arrived] });
  }));
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  await waitFor(() => expect(detailRequests).toBe(1));
  const openIndex = fetch.mock.calls.findIndex(([url]) => String(url).endsWith(`/threads/${threadDetail.id}`));
  const openResponse = fetch.mock.results[openIndex]!.value as Promise<Response>;

  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(detailRequests).toBe(2));
  await act(async () => {
    finishOpen();
    expect((await openResponse).status).toBe(500);
  });
  expect(screen.queryByRole("alert")).toBeNull();

  finishRefresh();
  expect(await screen.findByText(arrived.body)).toBeVisible();
  expect(reads()).toHaveLength(0);

  await user.click(row);
  await waitFor(() => expect(reads()).toHaveLength(1));
});

it("blocks sending a reviewed reply after a same-row reopen fails", async () => {
  const user = userEvent.setup({ delay: null });
  const fetch = vi.spyOn(globalThis, "fetch");
  let fails = false;
  server.use(http.get("*/v1/ui/threads/:id", () => fails
    ? new HttpResponse(null, { status: 500 }) : HttpResponse.json(threadDetail)));
  render(<App connect={() => () => {}} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  const body = await screen.findByRole("textbox", { name: "Message" });
  await user.type(body, "This draft should survive.");
  await user.click(screen.getByRole("button", { name: "Review" }));
  const reply = screen.getByRole("button", { name: "Reply" });
  expect(reply).toBeEnabled();

  fails = true;
  await user.click(row);
  expect(await screen.findByRole("alert")).toHaveTextContent("could not load");
  fails = false;
  await user.click(reply);
  const sends = fetch.mock.calls.filter(([url, init]) => init?.method === "POST" && String(url).endsWith("/messages"));
  expect(sends).toHaveLength(0);
  expect(reply).toBeDisabled();

  await user.click(screen.getByRole("button", { name: "Back to the draft" }));
  const review = screen.getByRole("button", { name: "Review" });
  expect(review).toBeDisabled();
  await user.type(body, "{Control>}{Enter}{/Control}");
  expect(screen.queryByRole("button", { name: "Reply" })).toBeNull();

  await user.click(row);
  await waitFor(() => expect(review).toBeEnabled());
  expect(screen.queryByRole("alert")).toBeNull();
  expect(body).toHaveValue("This draft should survive.");
  await user.click(review);
  expect(screen.getByRole("button", { name: "Reply" })).toBeEnabled();
});

const otherViews = [
  ["fleet", /^Fleet/],
  ["claims", /^Claims/],
  ["diagnostics", "Diagnostics"],
  ["compose", "+ New message"],
  ["account", "Account"],
  ["access", "Access"],
] as const;

const consoleViews = [["messages", /^My messages/], ...otherViews] as const;

it.each(consoleViews)("keeps the %s view selected across a watch refresh", async (view, buttonName) => {
  const user = userEvent.setup();
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let unread = 1;
  server.use(
    http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, human_unread: unread })),
    http.get("*/v1/ui/accounts", () => HttpResponse.json({ accounts: [] })),
  );
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  await user.click(await screen.findByRole("button", { name: buttonName }));
  expect(screen.getByRole("main")).toHaveAttribute("data-view", view);

  unread = 7;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  expect(await screen.findByRole("button", { name: /^My messages\s*7$/ })).toBeVisible();
  expect(screen.getByRole("main")).toHaveAttribute("data-view", view);
});

it("keeps the initial diagnostics view when the first session arrives", async () => {
  let empty = true;
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  server.use(http.get("*/v1/ui/overview", () => {
    const value = empty ? emptyOverview : sessionOverview;
    empty = false;
    return HttpResponse.json(value);
  }));
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  expect(await screen.findByText("Waiting for the first session.")).toBeVisible();
  expect(screen.getByRole("main")).toHaveAttribute("data-view", "diagnostics");

  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(screen.getByRole("button", { name: /^My messages/ })).toHaveTextContent(/1$/));
  expect(screen.getByRole("main")).toHaveAttribute("data-view", "diagnostics");
  expect(screen.queryByText("Waiting for the first session.")).toBeNull();
});

it("neither selects nor reads a new thread when a refresh changes the list", async () => {
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let refreshed = false;
  let overviews = 0;
  let reads = 0;
  let details = 0;
  server.use(
    http.get("*/v1/ui/overview", () => {
      overviews++;
      return HttpResponse.json(sessionOverview);
    }),
    http.get("*/v1/ui/threads", () => HttpResponse.json(refreshed ? {
      threads: [{ ...threadsPage.threads[0], id: "ms_new", subject: "Newest thread" }],
    } : threadsPage)),
    http.get("*/v1/ui/threads/:id", () => {
      details++;
      return HttpResponse.json({ ...threadDetail, id: "ms_new" });
    }),
    http.post("*/v1/ui/threads/:id/read/:message", () => {
      reads++;
      return HttpResponse.json({ read: 1 });
    }),
  );
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  expect(await screen.findByRole("button", { name: /Open thread: Rollout check/ })).toBeVisible();
  expect(screen.getByText("Choose a conversation")).toBeVisible();

  const before = overviews;
  refreshed = true;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(overviews).toBe(before + 1));
  expect(await screen.findByRole("button", { name: /Open thread: Newest thread/ })).toBeVisible();
  expect(screen.getByText("Choose a conversation")).toBeVisible();
  expect(screen.queryByRole("article", { name: "Newest thread" })).toBeNull();
  expect(details).toBe(0);
  expect(reads).toBe(0);
});

it("sends exactly one read for an explicit visible open under StrictMode and none for later frames", async () => {
  const user = userEvent.setup({ delay: null });
  mockVisibility("visible");
  const reads = observeThreadReads();
  const readBodies: unknown[] = [];
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  const firstMessage = threadDetail.messages[0]!;
  let messages = threadDetail.messages;
  server.use(
    http.get("*/v1/ui/threads/:id", () => HttpResponse.json({ ...threadDetail, messages })),
    http.post("*/v1/ui/threads/:id/read/:message", async ({ request }) => {
      readBodies.push(await request.json());
      messages = messages.map((message) => ({ ...message, recipients: [{ name: "human", state: "acked" }] }));
      return HttpResponse.json({ read: 1 });
    }),
  );
  render(
    <StrictMode>
      <App connect={(frame) => { onFrame = frame; return () => {}; }} />
    </StrictMode>,
  );
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(1);
  await waitFor(() => expect(readBodies).toEqual([{ repository_id: "", last_message_id: "ms_root" }]));

  messages = [...messages, { ...firstMessage, id: "ms_reply", body: "New rollout details." }];
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  const arrived = await screen.findByText("New rollout details.");
  expect(arrived.closest("article")).toHaveTextContent("To: human (pending)");
  expect(reads()).toHaveLength(1);
  expect(readBodies).toHaveLength(1);
});

it("defers a hidden open under StrictMode until visible and cancels it on Back", async () => {
  const user = userEvent.setup({ delay: null });
  const changeVisibility = mockVisibility("hidden");
  const reads = observeThreadReads();
  render(
    <StrictMode>
      <App connect={() => () => {}} />
    </StrictMode>,
  );
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(0);
  changeVisibility("hidden");
  expect(reads()).toHaveLength(0);

  await user.click(screen.getByRole("button", { name: "Back to threads" }));
  expect(screen.getByText("Choose a conversation")).toBeVisible();
  changeVisibility("visible");
  expect(reads()).toHaveLength(0);

  changeVisibility("hidden");
  await user.click(row);
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(0);
  changeVisibility("visible");
  expect(reads()).toHaveLength(1);
  changeVisibility("visible");
  expect(reads()).toHaveLength(1);
});

it.each(otherViews)("cancels a deferred read when switching from the thread to %s", async (view, buttonName) => {
  const user = userEvent.setup({ delay: null });
  const changeVisibility = mockVisibility("hidden");
  const reads = observeThreadReads();
  server.use(http.get("*/v1/ui/accounts", () => HttpResponse.json({ accounts: [] })));
  render(<App connect={() => () => {}} />);
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(0);

  await user.click(screen.getByRole("button", { name: buttonName }));
  expect(screen.getByRole("main")).toHaveAttribute("data-view", view);
  expect(screen.queryByRole("article", { name: "Rollout check" })).toBeNull();
  changeVisibility("visible");
  expect(reads()).toHaveLength(0);
});

it("cancels a deferred read when an overview error replaces the reader", async () => {
  const user = userEvent.setup({ delay: null });
  const changeVisibility = mockVisibility("visible");
  const reads = observeThreadReads();
  let deferFailure = false;
  let failureInFlight = false;
  let overviewFails = false;
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let finishOverviewFailure!: () => void;
  const overviewFailureReady = new Promise<void>((resolve) => { finishOverviewFailure = resolve; });
  let finishDetail!: () => void;
  const detailReady = new Promise<void>((resolve) => { finishDetail = resolve; });
  const retryDetail = {
    ...threadDetail,
    messages: [{ ...threadDetail.messages[0]!, id: "ms_retry", body: "Fresh detail after retry." }],
  };
  let serveRetryDetail = false;
  server.use(
    http.get("*/v1/ui/overview", async () => {
      if (deferFailure) {
        deferFailure = false;
        failureInFlight = true;
        await overviewFailureReady;
        return new HttpResponse(null, { status: 500 });
      }
      return overviewFails ? new HttpResponse(null, { status: 500 }) : HttpResponse.json(sessionOverview);
    }),
    http.get("*/v1/ui/threads/:id", async () => {
      await detailReady;
      return HttpResponse.json(serveRetryDetail ? retryDetail : threadDetail);
    }),
  );
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });

  // Start a frame-triggered overview reload before hiding; an in-flight reload continues
  // after hiding even though hidden tabs accept no new refreshes.
  deferFailure = true;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(failureInFlight).toBe(true));
  await user.click(row);
  expect(screen.queryByText("All checks passed.")).toBeNull();
  changeVisibility("hidden");
  finishDetail();
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(0);

  overviewFails = true;
  finishOverviewFailure();
  const error = await screen.findByRole("alert");
  expect(error).toHaveTextContent("could not load");
  expect(screen.queryByRole("article", { name: "Rollout check" })).toBeNull();
  changeVisibility("visible");
  expect(reads()).toHaveLength(0);

  overviewFails = false;
  serveRetryDetail = true;
  await user.click(within(error).getByRole("button", { name: "Try again" }));
  expect(await screen.findByText("Fresh detail after retry.")).toBeVisible();
  expect(reads()).toHaveLength(0);
});

it.each(["visible", "hidden"] as const)("reads newly arrived mail once when reopening the selected row while %s", async (visibility) => {
  const user = userEvent.setup({ delay: null });
  const changeVisibility = mockVisibility("visible");
  const reads = observeThreadReads();
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let unread = 2;
  const firstMessage = threadDetail.messages[0]!;
  let messages = threadDetail.messages;
  server.use(
    http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, human_unread: unread })),
    http.get("*/v1/ui/threads/:id", () => HttpResponse.json({ ...threadDetail, messages })),
    http.post("*/v1/ui/threads/:id/read/:message", () => {
      unread--;
      return HttpResponse.json({ read: 1 });
    }),
  );
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  expect(await screen.findByText("All checks passed.")).toBeVisible();
  expect(reads()).toHaveLength(1);
  expect(await screen.findByRole("button", { name: /^My messages\s*1$/ })).toBeVisible();

  messages = [
    { ...firstMessage, recipients: [{ name: "human", state: "acked" }] },
    { ...firstMessage, id: "ms_reply", body: "One more rollout check passed." },
  ];
  changeVisibility(visibility);
  expect(row).toHaveAttribute("aria-current", "true");
  await user.click(row);
  expect(await screen.findByText("One more rollout check passed.")).toBeVisible();
  expect(reads()).toHaveLength(visibility === "visible" ? 2 : 1);

  changeVisibility("visible");
  expect(reads()).toHaveLength(2);
  expect(await screen.findByRole("button", { name: /^My messages\s*0$/ })).toBeVisible();
  changeVisibility("visible");
  expect(reads()).toHaveLength(2);

  messages = [...messages, { ...firstMessage, id: "ms_latest", body: "Notes from the next rollout." }];
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  expect(await screen.findByText("Notes from the next rollout.")).toBeVisible();
  expect(reads()).toHaveLength(2);
});

describe("console timestamp locale", () => {
  it("formats every console timestamp in Polish and updates them when switching locale", async () => {
    const user = userEvent.setup();
    const session = structuredClone(operatorSession);
    session.account.ui_locale = "pl";
    const createdAt = "2026-11-22T12:23:00Z";
    const plThread = { ...threadsPage.threads[0]!, last_at: createdAt };
    const plDetail = { ...threadDetail, messages: [{ ...threadDetail.messages[0]!, created_at: createdAt }] };
    const plAgent = { ...sessionOverview.agents[0]!, last_seen: createdAt };
    const plDoctor = { ...doctorReport, checked_at: createdAt };
    const plProblems: DeliveryProblems = {
      window_s: 7 * 86400,
      wait_s: 15 * 60,
      more: false,
      messages: [{
        id: "ms_del",
        thread_id: "ms_root",
        subject: "Deliver check",
        created_at: createdAt,
        recipients: [{ name: "claude-host", state: "pending", reason: "waiting", execution: "active" }],
      }],
    };
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(session)),
      http.patch("*/v1/ui/profile", async ({ request }) => {
        const input = (await request.json()) as { ui_locale: string; display_name: string; correspondence_locale: string };
        session.account = { ...session.account, ...input, edit_revision: session.account.edit_revision + 1 };
        return HttpResponse.json({ account: session.account });
      }),
      http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, agents: [plAgent] })),
      http.get("*/v1/ui/doctor", () => HttpResponse.json({ hosts: [plDoctor] })),
      http.get("*/v1/ui/threads", () => HttpResponse.json({ threads: [plThread] })),
      http.get("*/v1/ui/threads/:id", () => HttpResponse.json(plDetail)),
      http.get("*/v1/ui/deliveries", () => HttpResponse.json(plProblems)),
    );

    const options: Intl.DateTimeFormatOptions = { dateStyle: "medium", timeStyle: "short" };
    const expectedPl = new Intl.DateTimeFormat("pl", options).format(new Date(createdAt));
    const expectedEn = new Intl.DateTimeFormat("en", options).format(new Date(createdAt));
    expect(expectedPl).toContain("lis");
    expect(expectedEn).toContain("Nov");

    render(<App connect={() => () => {}} />);
    await waitFor(() => expect(document.documentElement.lang).toBe("pl"));
    const row = await screen.findByRole("button", { name: /Rollout check/ });
    expect(within(row).getByText(expectedPl)).toBeVisible();
    expect(screen.queryByText(expectedEn)).toBeNull();

    // Delivery problem timestamp is in Polish.
    const deliverySection = screen.getByRole("region", { name: "Problemy z dostarczaniem" });
    await user.click(deliverySection.querySelector("summary")!);
    expect(within(deliverySection).getByText(expectedPl)).toBeVisible();

    // Opened thread message timestamp is in Polish.
    await user.click(row);
    const reader = await screen.findByRole("article", { name: "Rollout check" });
    expect(within(reader).getByText(expectedPl)).toBeVisible();

    // Fleet session activity & host doctor timestamps are in Polish.
    await user.click(screen.getByRole("button", { name: /^Flota/ }));
    const agentsTable = screen.getByRole("table", { name: "Agenci" });
    expect(within(agentsTable).getAllByText(new RegExp(expectedPl.replace(/[.*+?^${}()|[\]\\]/g, "\\$&"))).length).toBeGreaterThanOrEqual(2);

    // Diagnostics host report timestamp is in Polish.
    await user.click(screen.getByRole("button", { name: "Diagnostyka" }));
    expect(within(screen.getByRole("region", { name: "local" })).getByText(new RegExp(expectedPl.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeVisible();

    // Switch language to English via LocalePicker and verify timestamps switch too.
    await user.click(screen.getByRole("button", { name: /Polski/ }));
    await user.click(screen.getByRole("option", { name: /English/ }));
    await waitFor(() => expect(document.documentElement.lang).toBe("en"));
    expect(within(screen.getByRole("region", { name: "local" })).getByText(new RegExp(expectedEn.replace(/[.*+?^${}()|[\]\\]/g, "\\$&")))).toBeVisible();
    await user.click(screen.getByRole("button", { name: /^My messages/ }));
    const rowEn = await screen.findByRole("button", { name: /Rollout check/ });
    expect(within(rowEn).getByText(expectedEn)).toBeVisible();
  }, 30000);
});

describe("console view URL routing", () => {
  // The route effect writes the hash after the view commits, and back, forward
  // and hashchange apply the view after the URL has already changed. Reading
  // either synchronously can observe the previous commit.
  const hashIs = (hash: string) => waitFor(() => expect(window.location.hash).toBe(hash));
  const viewIs = (view: string) => waitFor(() => expect(screen.getByRole("main")).toHaveAttribute("data-view", view));

  const allRoutes = [
    { hash: "#/messages", button: /^My messages/, heading: "My messages", view: "messages", navName: /^My messages/ },
    { hash: "#/activity", button: "All activity", heading: "All activity", view: "messages", navName: "All activity" },
    { hash: "#/fleet", button: /^Fleet/, heading: "Fleet", view: "fleet", navName: /^Fleet/ },
    { hash: "#/claims", button: /^Claims/, heading: "File reservations", view: "claims", navName: /^Claims/ },
    { hash: "#/diagnostics", button: "Diagnostics", heading: "Diagnostics", view: "diagnostics", navName: "Diagnostics" },
    { hash: "#/compose", button: "+ New message", heading: "New message", view: "compose", navName: null },
    { hash: "#/account", button: "Account", heading: "Account", view: "account", navName: "Account" },
    { hash: "#/access", button: "Access", heading: "Access", view: "access", navName: "Access" },
  ] as const;

  it.each(allRoutes)("updates the URL for $hash and restores it across reload", async ({ hash, button, heading, view, navName }) => {
    const user = userEvent.setup();
    const threadAgents: string[] = [];
    server.use(
      http.get("*/v1/ui/accounts", () => HttpResponse.json({ accounts: [{ ...operatorSession.account, grants: [] }] })),
      http.get("*/v1/ui/threads", ({ request }) => {
        threadAgents.push(new URL(request.url).searchParams.get("agent") ?? "");
        return HttpResponse.json(threadsPage);
      }),
    );

    const first = render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: button }));
    await hashIs(hash);
    await viewIs(view);
    expect(await screen.findByRole("heading", { level: 1, name: heading })).toBeVisible();
    first.unmount();

    // Simulate a browser reload preserving window.location.hash.
    threadAgents.length = 0;
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { level: 1, name: heading })).toBeVisible();
    await viewIs(view);
    await hashIs(hash);
    if (navName !== null) {
      expect(within(screen.getByRole("navigation")).getByRole("button", { name: navName })).toHaveAttribute("aria-current", "page");
    }
    if (hash === "#/activity") {
      await waitFor(() => expect(threadAgents.at(-1)).toBe(""));
    }
  });

  it("lets a URL view win over the initial diagnostics latch while keeping the latch when URL has none", async () => {
    server.use(http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)));

    window.location.hash = "#/messages";
    const withUrl = render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    await viewIs("messages");
    await hashIs("#/messages");
    withUrl.unmount();

    window.history.replaceState(null, "", "/");
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("Waiting for the first session.")).toBeVisible();
    expect(screen.getByRole("main")).toHaveAttribute("data-view", "diagnostics");
    // The latch commits the diagnostics view in render. The hash is written
    // afterwards, in the route effect, so findBy can return while it is still empty.
    await waitFor(() => expect(window.location.hash).toBe("#/diagnostics"));
  });

  it.each(["#/unknown", "#content", "#/threads/ms_root", "#/messages/ms_root"])("falls back safely from invalid hash %s without opening any thread", async (badHash) => {
    const threadGets = vi.fn();
    const threadReads = vi.fn();
    server.use(
      http.get("*/v1/ui/threads/:id", () => { threadGets(); return HttpResponse.json(threadDetail); }),
      http.post("*/v1/ui/threads/:id/read/:message", () => { threadReads(); return HttpResponse.json({ read: 1 }); }),
    );
    window.location.hash = badHash;
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    await viewIs("messages");
    await hashIs("#/messages");
    expect(screen.getByText("Choose a conversation")).toBeVisible();
    expect(threadGets).not.toHaveBeenCalled();
    expect(threadReads).not.toHaveBeenCalled();
  });

  it.each(["#/diagnostics", "#/access", "#/activity"])("falls back safely when a member loads unauthorized hash %s", async (restrictedHash) => {
    const memberSession: AccountSession = {
      ...operatorSession,
      account: { ...operatorSession.account, id: "ca_member", username: "member", role: "member" },
    };
    const forbiddenDoctor = vi.fn();
    const forbiddenAccounts = vi.fn();
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(memberSession)),
      http.get("*/v1/ui/overview", () => HttpResponse.json({
        ...sessionOverview,
        repositories: sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "rw" as const })),
      })),
      http.get("*/v1/ui/doctor", () => { forbiddenDoctor(); return HttpResponse.json({ hosts: [] }); }),
      http.get("*/v1/ui/accounts", () => { forbiddenAccounts(); return HttpResponse.json({ accounts: [] }); }),
    );
    window.location.hash = restrictedHash;
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    await viewIs("messages");
    await hashIs("#/messages");
    expect(screen.queryByRole("combobox", { name: "Agent" })).toBeNull();
    expect(forbiddenDoctor).not.toHaveBeenCalled();
    expect(forbiddenAccounts).not.toHaveBeenCalled();
  });

  it("restores Compose on reload for a member with write access without rewriting the hash before permissions load", async () => {
    const user = userEvent.setup();
    const memberSession: AccountSession = {
      ...operatorSession,
      account: { ...operatorSession.account, id: "ca_member", username: "member", role: "member" },
    };
    const rwOverview = {
      ...sessionOverview,
      repositories: [
        { id: "rp_ro", name: "readonly", aliases: [], console_access: "ro" as const },
        { id: "rp_iris", name: "iris", aliases: [], console_access: "rw" as const },
      ],
    };
    let overviewRequested = false;
    let resolveOverview!: (response: Response) => void;
    const overviewPromise = new Promise<Response>((resolve) => {
      resolveOverview = resolve;
    });
    let deferOverview = false;
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(memberSession)),
      http.get("*/v1/ui/overview", () => {
        if (deferOverview) {
          overviewRequested = true;
          return overviewPromise;
        }
        return HttpResponse.json(rwOverview);
      }),
    );

    const first = render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(screen.getByRole("button", { name: "+ New message" }));
    await hashIs("#/compose");
    first.unmount();

    deferOverview = true;
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("Loading messages…")).toBeVisible();
    await waitFor(() => expect(overviewRequested).toBe(true));
    await hashIs("#/compose");

    await act(async () => {
      resolveOverview(HttpResponse.json(rwOverview));
    });
    expect(await screen.findByRole("heading", { level: 1, name: "New message" })).toBeVisible();
    await viewIs("compose");
    await hashIs("#/compose");
    const compose = within(document.getElementById("compose")!);
    expect(compose.getByRole("combobox", { name: "Send from repository" })).toHaveValue("rp_iris");
    expect(compose.getByRole("checkbox", { name: /claude-host/ })).toBeInTheDocument();
  });

  it.each([
    ["ro" as const, sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "ro" as const }))],
    ["none" as const, []],
  ])("falls back to Messages on reload when a member on #/compose has %s repository access", async (_label, repositories) => {
    const memberSession: AccountSession = {
      ...operatorSession,
      account: { ...operatorSession.account, id: "ca_member", username: "member", role: "member" },
    };
    let overviewRequested = false;
    let resolveOverview!: (response: Response) => void;
    const overviewPromise = new Promise<Response>((resolve) => {
      resolveOverview = resolve;
    });
    server.use(
      http.get("*/v1/ui/session", () => HttpResponse.json(memberSession)),
      http.get("*/v1/ui/overview", () => {
        overviewRequested = true;
        return overviewPromise;
      }),
    );

    window.location.hash = "#/compose";
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("Loading messages…")).toBeVisible();
    await waitFor(() => expect(overviewRequested).toBe(true));
    await hashIs("#/compose");

    await act(async () => {
      resolveOverview(HttpResponse.json({ ...sessionOverview, repositories }));
    });
    expect(await screen.findByRole("heading", { name: "Messages" })).toBeVisible();
    await viewIs("messages");
    await hashIs("#/messages");
    expect(document.getElementById("compose")).toBeNull();
  });

  it("never puts an open thread in the URL and never opens or reads a thread on reload", async () => {
    const user = userEvent.setup();
    let detailCalls = 0;
    let readCalls = 0;
    server.use(
      http.get("*/v1/ui/threads/:id", () => {
        detailCalls++;
        return HttpResponse.json(threadDetail);
      }),
      http.post("*/v1/ui/threads/:id/read/:message", () => {
        readCalls++;
        return HttpResponse.json({ read: 1 });
      }),
    );

    const first = render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: "All activity" }));
    await hashIs("#/activity");
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    expect(await screen.findByRole("heading", { name: "Rollout check" })).toBeVisible();
    await hashIs("#/activity");
    expect(detailCalls).toBe(1);
    expect(readCalls).toBe(1);
    first.unmount();

    detailCalls = 0;
    readCalls = 0;
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "All activity" })).toBeVisible();
    expect(screen.getByText("Choose a conversation")).toBeVisible();
    expect(screen.queryByRole("article", { name: "Rollout check" })).toBeNull();
    expect(detailCalls).toBe(0);
    expect(readCalls).toBe(0);
  });

  it("supports browser back and forward, runtime hashchange fallbacks, and skip-to-content without losing the view", async () => {
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });
    await hashIs("#/messages");

    await user.click(screen.getByRole("button", { name: "All activity" }));
    await hashIs("#/activity");
    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    await hashIs("#/fleet");
    await user.click(screen.getByRole("link", { name: "Skip to content" }));
    expect(screen.getByRole("main")).toHaveFocus();
    await hashIs("#/fleet");
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    await hashIs("#/claims");

    window.history.back();
    await waitFor(() => expect(screen.getByRole("main")).toHaveAttribute("data-view", "fleet"));
    window.history.back();
    await waitFor(() => expect(within(screen.getByRole("navigation")).getByRole("button", { name: "All activity" })).toHaveAttribute("aria-current", "page"));
    window.history.back();
    await waitFor(() => expect(within(screen.getByRole("navigation")).getByRole("button", { name: /^My messages/ })).toHaveAttribute("aria-current", "page"));
    window.history.forward();
    await waitFor(() => expect(within(screen.getByRole("navigation")).getByRole("button", { name: "All activity" })).toHaveAttribute("aria-current", "page"));

    // An invalid hashchange while mounted falls back cleanly, and clearing hash restores the latched view.
    act(() => {
      window.location.hash = "#/not-a-view";
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await hashIs("#/messages");
    await viewIs("messages");

    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    act(() => {
      window.history.replaceState(null, "", "/");
      window.dispatchEvent(new HashChangeEvent("hashchange"));
    });
    await hashIs("#/messages");
    await viewIs("messages");

    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    await hashIs("#/fleet");
    await user.click(screen.getByRole("link", { name: "Iris" }));
    await hashIs("#/messages");
    await viewIs("messages");
  });

  it("never triggers a thread read when navigating back to messages after leaving a deferred hidden open", async () => {
    const user = userEvent.setup({ delay: null });
    const changeVisibility = mockVisibility("hidden");
    const reads = observeThreadReads();
    render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    expect(await screen.findByText("All checks passed.")).toBeVisible();
    await hashIs("#/messages");

    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    await hashIs("#/fleet");
    changeVisibility("visible");
    window.history.back();
    await waitFor(() => expect(screen.getByRole("main")).toHaveAttribute("data-view", "messages"));
    expect(await screen.findByText("All checks passed.")).toBeVisible();
    expect(reads()).toHaveLength(0);
  });
});

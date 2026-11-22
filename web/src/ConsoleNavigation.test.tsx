import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, expect, it } from "vitest";
import { App } from "./App";
import type { WatchFrame } from "./api";
import i18n from "./i18n";
import { emptyOverview, server, sessionOverview, threadDetail, threadsPage } from "./test/server";

beforeEach(async () => { await i18n.changeLanguage("en"); });

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
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
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


const consoleViews = [
  ["messages", /^My messages/],
  ["fleet", /^Fleet/],
  ["claims", /^Claims/],
  ["diagnostics", "Diagnostics"],
  ["compose", "+ New message"],
  ["account", "Account"],
  ["access", "Access"],
] as const;

it.each(consoleViews)("keeps the %s view selected across a watch refresh", async (view, buttonName) => {
  const user = userEvent.setup();
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let overviews = 0;
  server.use(http.get("*/v1/ui/overview", () => {
    overviews++;
    return HttpResponse.json(sessionOverview);
  }));
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  await user.click(await screen.findByRole("button", { name: buttonName }));
  expect(screen.getByRole("main")).toHaveAttribute("data-view", view);

  const before = overviews;
  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(overviews).toBe(before + 1));
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
    http.post("*/v1/ui/threads/:id/read", () => {
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

it("sends exactly one read for an explicit visible open and none for later frames", async () => {
  const user = userEvent.setup({ delay: null });
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  let reads = 0;
  let details = 0;
  server.use(
    http.get("*/v1/ui/threads/:id", () => {
      details++;
      return HttpResponse.json(threadDetail);
    }),
    http.post("*/v1/ui/threads/:id/read", () => {
      reads++;
      return HttpResponse.json({ read: 1 });
    }),
  );
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
  await screen.findByRole("article", { name: "Rollout check" });
  await waitFor(() => expect(reads).toBe(1));

  act(() => onFrame?.({ cursor: 2, kind: "changed" }));
  await waitFor(() => expect(details).toBe(2));
  await act(async () => { await Promise.resolve(); });
  expect(reads).toBe(1);
});

it("defers an explicit hidden open until the document becomes visible", async () => {
  const original = Object.getOwnPropertyDescriptor(document, "visibilityState");
  let visibility: DocumentVisibilityState = "hidden";
  Object.defineProperty(document, "visibilityState", { configurable: true, get: () => visibility });
  try {
    const user = userEvent.setup({ delay: null });
    let reads = 0;
    server.use(http.post("*/v1/ui/threads/:id/read", () => {
      reads++;
      return HttpResponse.json({ read: 1 });
    }));
    render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /Open thread: Rollout check/ }));
    await screen.findByRole("article", { name: "Rollout check" });
    await act(async () => { await Promise.resolve(); });
    expect(reads).toBe(0);

    act(() => document.dispatchEvent(new Event("visibilitychange")));
    expect(reads).toBe(0);
    visibility = "visible";
    act(() => document.dispatchEvent(new Event("visibilitychange")));
    await waitFor(() => expect(reads).toBe(1));
    act(() => document.dispatchEvent(new Event("visibilitychange")));
    expect(reads).toBe(1);
  } finally {
    if (original === undefined) Reflect.deleteProperty(document, "visibilityState");
    else Object.defineProperty(document, "visibilityState", original);
  }
});

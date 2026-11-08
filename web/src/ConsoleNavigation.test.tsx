import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, expect, it } from "vitest";
import { App } from "./App";
import type { WatchFrame } from "./api";
import i18n from "./i18n";
import { server, sessionOverview, threadDetail, threadsPage } from "./test/server";

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

it("keeps the conversation and edited reply when its selected row is opened again", async () => {
  const user = userEvent.setup();
  render(<App connect={() => () => {}} />);
  const row = await screen.findByRole("button", { name: /Open thread: Rollout check/ });
  await user.click(row);
  const heading = await screen.findByRole("heading", { name: "Rollout check" });
  const body = screen.getByRole("textbox", { name: "Message" });
  await user.type(body, "Please keep these notes.");
  await user.click(row);
  expect(heading).toHaveFocus();
  expect(body).toHaveValue("Please keep these notes.");
  expect(screen.getByRole("button", { name: "Review" })).toBeEnabled();
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

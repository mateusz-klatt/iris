import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, expect, it } from "vitest";
import { App } from "./App";
import type { ThreadDetail, ThreadsPage } from "./api";
import i18n from "./i18n";
import { reviewAndSend } from "./test/send";
import { server, sessionOverview, threadDetail, threadsPage } from "./test/server";

beforeEach(async () => { await i18n.changeLanguage("en"); });

function conversations() {
  const details: Record<string, ThreadDetail> = {
    ms_root: threadDetail,
    ms_second: {
      id: "ms_second",
      messages: [{ ...threadDetail.messages[0]!, id: "ms_second", sender: "codex-host", subject: "Second conversation" }],
    },
    ms_empty: { id: "ms_empty", messages: [] },
  };
  server.use(
    http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, repositories: [
      ...sessionOverview.repositories,
      { id: "rp_other", name: "other", aliases: [] },
    ] })),
    http.get("*/v1/ui/threads", () => HttpResponse.json({ threads: [
      ...threadsPage.threads,
      { ...threadsPage.threads[0]!, id: "ms_second", subject: "Second conversation" },
      { ...threadsPage.threads[0]!, id: "ms_empty", subject: "Empty conversation", count: 0, unread: 0 },
    ] } satisfies ThreadsPage)),
    http.get("*/v1/ui/threads/:id", ({ params }) => HttpResponse.json(details[String(params.id)])),
  );
  render(<App connect={() => () => {}} />);
}

async function open(user: ReturnType<typeof userEvent.setup>, subject: string) {
  await user.click(await screen.findByRole("button", { name: new RegExp(`Open thread: ${subject}`) }));
  return within(await screen.findByRole("article", { name: subject }));
}

it("keeps independent reply drafts while moving between conversations and mailbox views", async () => {
  const user = userEvent.setup({ delay: null });
  conversations();
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "My unfinished first reply");
  await user.clear(first.getByRole("textbox", { name: "Recipients" }));
  await user.type(first.getByRole("textbox", { name: "Recipients" }), "reviewer");
  await user.clear(first.getByRole("textbox", { name: "Subject" }));
  await user.type(first.getByRole("textbox", { name: "Subject" }), "My own subject");
  let second = await open(user, "Second conversation");
  expect(second.getByRole("textbox", { name: "Message" })).toHaveValue("");
  expect(second.getByRole("textbox", { name: "Recipients" })).toHaveValue("codex-host");
  await user.type(second.getByRole("textbox", { name: "Message" }), "My second reply");
  await user.click(screen.getByRole("button", { name: "All activity" }));
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("My unfinished first reply");
  expect(first.getByRole("textbox", { name: "Recipients" })).toHaveValue("reviewer");
  expect(first.getByRole("textbox", { name: "Subject" })).toHaveValue("My own subject");
  second = await open(user, "Second conversation");
  expect(second.getByRole("textbox", { name: "Message" })).toHaveValue("My second reply");
});

it.each(["conversation", "fleet", "list"])("returns to the conversation heading with review closed after visiting another %s", async (destination) => {
  const user = userEvent.setup({ delay: null });
  conversations();
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Keep my reviewed reply");
  await user.click(first.getByRole("button", { name: "Review" }));
  expect(first.getByRole("button", { name: "Reply" })).toHaveFocus();
  if (destination === "conversation") {
    await open(user, "Second conversation");
  } else if (destination === "fleet") {
    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    await user.click(screen.getByRole("button", { name: /^My messages/ }));
  } else {
    await user.click(screen.getByRole("button", { name: "Back to threads" }));
  }
  first = await open(user, "Rollout check");
  expect(first.getByRole("heading", { name: "Rollout check" })).toHaveFocus();
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("Keep my reviewed reply");
  expect(first.queryByRole("heading", { name: "Check before sending" })).toBeNull();
  expect(first.getByRole("button", { name: "Review" })).toBeEnabled();
});

it("keeps the same conversation's drafts separate in each repository view and through search", async () => {
  const user = userEvent.setup({ delay: null });
  conversations();
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "All repositories reply");
  await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_other");
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Other repository reply");
  await user.type(screen.getByRole("searchbox", { name: "Search threads" }), "Rollout");
  await user.click(screen.getByRole("button", { name: "Search" }));
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("Other repository reply");
  await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "");
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("All repositories reply");
});

it("never offers another conversation's reply in an empty thread", async () => {
  const user = userEvent.setup({ delay: null });
  conversations();
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Keep me in my conversation");
  await user.click(screen.getByRole("button", { name: /Open thread: Empty conversation/ }));
  expect(await screen.findByRole("heading", { name: "ms_empty" })).toBeVisible();
  expect(screen.queryByRole("textbox", { name: "Message" })).toBeNull();
  expect(screen.queryByRole("button", { name: "Review" })).toBeNull();
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("Keep me in my conversation");
});

it("retains the retry identity after leaving a reply whose send outcome is uncertain", async () => {
  const user = userEvent.setup({ delay: null });
  const bodies: { key: string; body: string; reply_to: string }[] = [];
  conversations();
  server.use(http.post("*/v1/ui/messages", async ({ request }) => {
    bodies.push(await request.json() as typeof bodies[number]);
    return bodies.length === 1 ? HttpResponse.error() : new HttpResponse(null, { status: 204 });
  }));
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Send this once");
  await reviewAndSend(user, first, "Reply");
  expect(await screen.findByRole("alert")).toHaveTextContent("within a day of the first attempt is safe");
  const second = await open(user, "Second conversation");
  await user.type(second.getByRole("textbox", { name: "Message" }), "Still working here");
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("Send this once");
  expect(first.getByRole("heading", { name: "Rollout check" })).toHaveFocus();
  expect(first.queryByRole("heading", { name: "Check before sending" })).toBeNull();
  await reviewAndSend(user, first, "Reply");
  expect(await screen.findByText("Sent.")).toBeVisible();
  expect(bodies).toHaveLength(2);
  expect(bodies[1]).toEqual(bodies[0]);
});

it("finishes only the submitted conversation when another reply is edited during the send", async () => {
  const user = userEvent.setup({ delay: null });
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>((resolve) => { finish = resolve; });
  conversations();
  server.use(http.post("*/v1/ui/messages", () => pending));
  let first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Sending first reply");
  await reviewAndSend(user, first, "Reply");
  expect(first.getByRole("button", { name: "Reply" })).toBeDisabled();
  const second = await open(user, "Second conversation");
  await user.type(second.getByRole("textbox", { name: "Message" }), "Keep this second draft");
  await act(async () => { finish(new HttpResponse(null, { status: 204 })); });
  expect(await screen.findByText("Sent.")).toBeVisible();
  expect(second.getByRole("textbox", { name: "Message" })).toHaveValue("Keep this second draft");
  first = await open(user, "Rollout check");
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("");
});

it("keeps a reply edited after submission and gives its next send a new identity", async () => {
  const user = userEvent.setup({ delay: null });
  let finish!: (response: Response) => void;
  const pending = new Promise<Response>((resolve) => { finish = resolve; });
  const bodies: { key: string; body: string }[] = [];
  conversations();
  server.use(http.post("*/v1/ui/messages", async ({ request }) => {
    bodies.push(await request.json() as typeof bodies[number]);
    return bodies.length === 1 ? pending : new HttpResponse(null, { status: 204 });
  }));
  const first = await open(user, "Rollout check");
  await user.type(first.getByRole("textbox", { name: "Message" }), "Initial reply");
  await reviewAndSend(user, first, "Reply");
  await user.type(first.getByRole("textbox", { name: "Message" }), " with another thought");
  await act(async () => { finish(new HttpResponse(null, { status: 204 })); });
  expect(await screen.findByText("Sent.")).toBeVisible();
  expect(first.getByRole("textbox", { name: "Message" })).toHaveValue("Initial reply with another thought");
  await reviewAndSend(user, first, "Reply");
  await waitFor(() => expect(bodies).toHaveLength(2));
  expect(bodies[1]?.key).not.toBe(bodies[0]?.key);
  expect(bodies[1]?.body).toBe("Initial reply with another thought");
});

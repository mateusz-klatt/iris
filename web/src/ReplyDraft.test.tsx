import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { beforeEach, describe, expect, it } from "vitest";
import { App } from "./App";
import type {
  ThreadDetail,
  ThreadMessage,
  WatchFrame,
  WatchStatus,
} from "./api";
import i18n from "./i18n";
import { server, threadDetail } from "./test/server";

beforeEach(async () => {
  document.documentElement.lang = "en";
  await i18n.changeLanguage("en");
});

function message(
  id: string,
  sender: string,
  to: string[],
  subject = "Rollout check",
): ThreadMessage {
  return {
    id,
    sender,
    recipients: to.map((name) => ({ name, state: "pending" })),
    subject,
    body: id,
    created_at: "2026-11-06T10:05:00Z",
  };
}

// serveThread answers the thread with the messages it holds at each request, and counts them.
function serveThread(messages: ThreadMessage[]) {
  const state = { messages, loads: 0 };
  server.use(
    http.get("*/v1/ui/threads/ms_root", () => {
      state.loads++;
      const detail: ThreadDetail = { id: "ms_root", messages: state.messages };
      return HttpResponse.json(detail);
    }),
  );
  return state;
}

function watched() {
  const feed: { send?: (frame: WatchFrame) => void } = {};
  const connect = (
    frame: (value: WatchFrame) => void,
    status?: (value: WatchStatus) => void,
  ) => {
    feed.send = frame;
    status?.("live");
    return () => undefined;
  };
  return { feed, connect };
}

async function openThread(
  connect: NonNullable<Parameters<typeof App>[0]>["connect"],
) {
  render(<App connect={connect} />);
  await userEvent
    .setup({ delay: null })
    .click(await screen.findByRole("button", { name: /Rollout check/ }));
  return within(await screen.findByRole("article", { name: "Rollout check" }));
}

// Gap review A14: a live refresh must not take the reply from under the operator.
describe("console reply draft", () => {
  it("keeps what the operator typed when an unrelated event refreshes the thread", async () => {
    const user = userEvent.setup({ delay: null });
    const thread = serveThread(threadDetail.messages);
    const { feed, connect } = watched();
    const detail = await openThread(connect);
    await user.clear(detail.getByRole("textbox", { name: "Recipients" }));
    await user.type(
      detail.getByRole("textbox", { name: "Recipients" }),
      "codex-host",
    );
    await user.type(
      detail.getByRole("textbox", { name: "Message" }),
      "Half a thought",
    );
    const loads = thread.loads;
    thread.messages = [
      ...threadDetail.messages,
      message("ms_next", "claude-host", ["human"]),
    ];
    feed.send?.({ cursor: 200, kind: "changed" });
    await waitFor(() => expect(thread.loads).toBeGreaterThan(loads));
    await screen.findByText("ms_next");
    expect(detail.getByRole("textbox", { name: "Message" })).toHaveValue(
      "Half a thought",
    );
    expect(detail.getByRole("textbox", { name: "Recipients" })).toHaveValue(
      "codex-host",
    );
  });

  it("answers the newest message on refresh while the operator has not started a reply", async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: unknown[] = [];
    server.use(
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const thread = serveThread(threadDetail.messages);
    const { feed, connect } = watched();
    const detail = await openThread(connect);
    thread.messages = [
      ...threadDetail.messages,
      message("ms_next", "claude-host", ["human"]),
    ];
    feed.send?.({ cursor: 200, kind: "changed" });
    await screen.findByText("ms_next");
    await user.type(
      detail.getByRole("textbox", { name: "Message" }),
      "Thanks.",
    );
    await user.click(detail.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({
      to: ["claude-host"],
      reply_to: "ms_next",
    });

    // After a send nothing of the operator's is left, so the next refresh answers the newest.
    thread.messages = [
      ...thread.messages,
      message("ms_mine", "human", ["claude-host"]),
      message("ms_third", "claude-host", ["human"]),
    ];
    feed.send?.({ cursor: 201, kind: "changed" });
    await screen.findByText("ms_third");
    await user.type(detail.getByRole("textbox", { name: "Message" }), "Again.");
    await user.click(detail.getByRole("button", { name: "Reply" }));
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toMatchObject({
      to: ["claude-host"],
      reply_to: "ms_third",
    });
  });

  it("addresses a follow-up to the other party, not back to the operator", async () => {
    serveThread([
      ...threadDetail.messages,
      message("ms_mine", "human", ["claude-host"]),
    ]);
    const detail = await openThread(watched().connect);
    expect(detail.getByRole("textbox", { name: "Recipients" })).toHaveValue(
      "claude-host",
    );
  });

  it("addresses a thread only the operator wrote to whoever they wrote to", async () => {
    serveThread([
      message("ms_root", "human", ["claude-host", "human", "codex-host"]),
    ]);
    const detail = await openThread(watched().connect);
    expect(detail.getByRole("textbox", { name: "Recipients" })).toHaveValue(
      "claude-host, codex-host",
    );
  });
});

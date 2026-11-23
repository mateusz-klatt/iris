import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { StrictMode } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { App } from "./App";
import * as api from "./api";
import type { ThreadDetail, WatchFrame } from "./api";
import { loadLocale } from "./i18n";
import pl from "./locales/pl";
import { doctorReport, operatorSession, server, sessionOverview, threadDetail } from "./test/server";

afterEach(async () => {
  vi.restoreAllMocks();
  await loadLocale("en");
});

it("renders translated protocol labels, reply subjects and the unknown-host accessible name", async () => {
  const user = userEvent.setup();
  server.use(
    http.get("*/v1/ui/session", () => HttpResponse.json({
      ...operatorSession, account: { ...operatorSession.account, ui_locale: "pl" },
    })),
    http.get("*/v1/ui/doctor", () => HttpResponse.json({
      hosts: [{ ...doctorReport, host: "unknown" }],
    })),
  );
  render(<App connect={() => () => {}} />);
  await user.click(await screen.findByRole("button", { name: /^Rollout check/ }));
  const detail = within(await screen.findByRole("article", { name: "Rollout check" }));
  expect(detail.getByText(`${pl.message.to}: human (${pl.operator.deliveryStatePending})`)).toBeVisible();
  expect(detail.getByRole("textbox", { name: pl.operator.subject })).toHaveValue(
    pl.operator.replySubject.replace("{{subject}}", threadDetail.messages[0]!.subject),
  );

  await user.click(screen.getByRole("button", { name: new RegExp(`^${pl.console.fleet}`) }));
  const agent = sessionOverview.agents[0]!;
  expect(screen.getByRole("rowheader", { name: agent.name }).closest("tr"))
    .toHaveTextContent(`${agent.host} · ${agent.client} · ${pl.operator.sessionStateActive}`);

  await user.click(screen.getByRole("button", { name: new RegExp(`^${pl.operator.claims}`) }));
  expect(screen.getByText("docs/api.md").closest("li"))
    .toHaveTextContent(pl.operator.claimKindExact);

  await user.click(screen.getByRole("button", { name: pl.console.diagnostics }));
  expect(screen.getByRole("region", { name: pl.operator.doctorUnknownHost })).toBeVisible();
});

it("acknowledges an in-flight opening once when saving the language emits a revision frame on the existing watch", async () => {
  const user = userEvent.setup();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  let finishOriginal!: (detail: ThreadDetail) => void;
  let finishReplacement!: (detail: ThreadDetail) => void;
  const original = new Promise<ThreadDetail>((resolve) => { finishOriginal = resolve; });
  const replacement = new Promise<ThreadDetail>((resolve) => { finishReplacement = resolve; });
  const detailRequests = vi.spyOn(api, "getThread")
    .mockReturnValueOnce(original)
    .mockReturnValueOnce(replacement);
  const readCalls = vi.spyOn(api, "readThread");
  let overviewRequests = 0;
  const readBodies: unknown[] = [];
  const stopped = vi.fn();
  let onFrame!: (value: WatchFrame) => void;
  const connect = vi.fn((frame: (value: WatchFrame) => void) => {
    onFrame = frame;
    // The real hub emits an initial frame on every connection, including reconnects.
    frame({ cursor: 1, kind: "changed" });
    return stopped;
  });
  server.use(
    http.get("*/v1/ui/overview", () => HttpResponse.json({
      ...sessionOverview, human_unread: ++overviewRequests,
    })),
    http.patch("*/v1/ui/profile", async ({ request }) => {
      const input = await request.json() as { ui_locale: string };
      // Saving the profile bumps ConsoleRevision; the already connected watch
      // reports it while the original detail request is still pending.
      onFrame({ cursor: 2, kind: "changed" });
      return HttpResponse.json({
        account: { ...operatorSession.account, ...input, edit_revision: 2 },
      });
    }),
    http.post("*/v1/ui/threads/:id/read/:message", async ({ request }) => {
      readBodies.push(await request.json());
      return HttpResponse.json({ read: 0 });
    }),
  );
  render(<StrictMode><App connect={connect} refreshGap={0} /></StrictMode>);
  // Settle both StrictMode mount loads and the live watch's initial-frame refresh
  // before opening the thread; the unread count confirms the refresh was rendered.
  await waitFor(() => expect(document.body.dataset.humanUnread).toBe("3"));
  const connectionsBeforeLocale = connect.mock.calls.length;
  const stopsBeforeLocale = stopped.mock.calls.length;
  await user.click(await screen.findByRole("button", { name: /^Rollout check/ }));
  await waitFor(() => expect(detailRequests).toHaveBeenCalledTimes(1));
  await user.click(screen.getByRole("button", { name: "Choose language. Current language: English" }));
  await user.click(screen.getByRole("option", { name: "Use Polski" }));
  await waitFor(() => expect(document.documentElement.lang).toBe("pl"));
  await waitFor(() => expect(detailRequests).toHaveBeenCalledTimes(2));
  expect(connect).toHaveBeenCalledTimes(connectionsBeforeLocale);
  expect(stopped).toHaveBeenCalledTimes(stopsBeforeLocale);
  expect(readCalls).not.toHaveBeenCalled();
  await act(async () => {
    finishReplacement(threadDetail);
    await replacement;
  });
  const detail = within(await screen.findByRole("article", { name: "Rollout check" }));
  expect(detail.getByRole("textbox", { name: pl.operator.subject })).toHaveValue("Odp: Rollout check");
  await waitFor(() => expect(readBodies).toEqual([{ repository_id: "", last_message_id: "ms_root" }]));
  await act(async () => {
    finishOriginal(threadDetail);
    await original;
  });
  expect(detailRequests).toHaveBeenCalledTimes(2);
  expect(readCalls).toHaveBeenCalledTimes(1);
});

it.each([
  ["Odp: parser test plan", "", "Re: Odp: parser test plan"],
  ["RE: Rollout", "ms_root", "RE: Rollout"],
  ["Re:Rollout", "ms_root", "Re:Rollout"],
  ["oDp: Rollout", "ms_root", "oDp: Rollout"],
])("uses message reply metadata when suggesting a subject for %s", async (subject, replyTo, expected) => {
  const user = userEvent.setup();
  server.use(http.get("*/v1/ui/threads/:id", () => HttpResponse.json({
    ...threadDetail,
    messages: [{ ...threadDetail.messages[0], id: replyTo === "" ? "ms_root" : "ms_reply", reply_to: replyTo, subject }],
  })));
  render(<App connect={() => () => {}} />);
  await user.click(await screen.findByRole("button", { name: /^Rollout check/ }));
  const detail = within(await screen.findByRole("article", { name: subject }));
  expect(detail.getByRole("textbox", { name: "Subject" })).toHaveValue(expected);
});

it("keeps a reply sendable when the Polish prefix would exceed the subject byte limit", async () => {
  const user = userEvent.setup();
  const original = "x".repeat(1020);
  const sent: unknown[] = [];
  server.use(
    http.get("*/v1/ui/session", () => HttpResponse.json({
      ...operatorSession, account: { ...operatorSession.account, ui_locale: "pl" },
    })),
    http.get("*/v1/ui/threads/:id", () => HttpResponse.json({
      ...threadDetail, messages: [{ ...threadDetail.messages[0], subject: original }],
    })),
    http.post("*/v1/ui/messages", async ({ request }) => {
      sent.push(await request.json());
      return new HttpResponse(null, { status: 204 });
    }),
  );
  render(<App connect={() => () => {}} />);
  await user.click(await screen.findByRole("button", { name: /^Rollout check/ }));
  const detail = within(await screen.findByRole("article", { name: original }));
  const expected = `Odp: ${original.slice(0, 1019)}`;
  expect(detail.getByRole("textbox", { name: pl.operator.subject })).toHaveValue(expected);
  expect(new TextEncoder().encode(expected)).toHaveLength(1024);
  expect(detail.queryByRole("alert")).toBeNull();
  await user.type(detail.getByRole("textbox", { name: pl.operator.body }), "Dziękuję.");
  const review = detail.getByRole("button", { name: pl.operator.review });
  expect(review).toBeEnabled();
  await user.click(review);
  const send = detail.getByRole("button", { name: pl.operator.reply });
  expect(send).toBeEnabled();
  await user.click(send);
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]).toMatchObject({ subject: expected, body: "Dziękuję.", reply_to: "ms_root" });
});

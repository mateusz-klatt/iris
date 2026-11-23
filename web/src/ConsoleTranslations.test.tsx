import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { StrictMode } from "react";
import { afterEach, expect, it, vi } from "vitest";

import { App } from "./App";
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

it("acknowledges an in-flight thread open exactly once when the language changes", async () => {
  const user = userEvent.setup();
  vi.spyOn(document, "visibilityState", "get").mockReturnValue("visible");
  let finishDetail!: () => void;
  const detailReady = new Promise<void>((resolve) => { finishDetail = resolve; });
  let detailRequests = 0;
  const readBodies: unknown[] = [];
  server.use(
    http.get("*/v1/ui/threads/:id", async () => {
      detailRequests++;
      await detailReady;
      return HttpResponse.json(threadDetail);
    }),
    http.post("*/v1/ui/threads/:id/read", async ({ request }) => {
      readBodies.push(await request.json());
      return HttpResponse.json({ read: 1 });
    }),
  );
  render(<StrictMode><App connect={() => () => {}} /></StrictMode>);
  await user.click(await screen.findByRole("button", { name: /^Rollout check/ }));
  await waitFor(() => expect(detailRequests).toBe(1));
  await user.click(screen.getByRole("button", { name: "Choose language. Current language: English" }));
  await user.click(screen.getByRole("option", { name: "Use Polski" }));
  await waitFor(() => expect(document.documentElement.lang).toBe("pl"));
  finishDetail();
  const detail = within(await screen.findByRole("article", { name: "Rollout check" }));
  expect(detail.getByRole("textbox", { name: pl.operator.subject })).toHaveValue("Odp: Rollout check");
  await waitFor(() => expect(readBodies).toEqual([{ repository_id: "", last_message_id: "ms_root" }]));
  // Changing language did not replace the request that owned the read authorization.
  expect(detailRequests).toBe(1);
  expect(readBodies).toHaveLength(1);
});

it.each([
  ["Odp: parser test plan", "", "Re: Odp: parser test plan"],
  ["RE: Rollout", "ms_root", "RE: Rollout"],
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

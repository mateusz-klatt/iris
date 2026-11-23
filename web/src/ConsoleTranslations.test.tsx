import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, expect, it } from "vitest";

import { App } from "./App";
import { loadLocale } from "./i18n";
import pl from "./locales/pl";
import { doctorReport, operatorSession, server, sessionOverview, threadDetail } from "./test/server";

afterEach(async () => { await loadLocale("en"); });

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

import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { expect, it } from "vitest";
import { App } from "./App";
import type { OutgoingMessage, WatchFrame } from "./api";
import { server, sessionOverview } from "./test/server";

it("requires a new selection when a reviewed recipient is recreated under the same name", async () => {
  let directory = sessionOverview;
  let onFrame: ((frame: WatchFrame) => void) | undefined;
  const sent: OutgoingMessage[] = [];
  server.use(
    http.get("*/v1/ui/overview", () => HttpResponse.json(directory)),
    http.post("*/v1/ui/messages", async ({ request }) => {
      sent.push(await request.json() as OutgoingMessage);
      return new HttpResponse(null, { status: 204 });
    }),
  );
  const user = userEvent.setup();
  render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
  await screen.findByRole("heading", { name: "Clients" });
  const compose = within(document.getElementById("compose")!);
  await user.click(compose.getByRole("checkbox", { name: /claude-host/ }));
  await user.type(compose.getByRole("textbox", { name: "Subject" }), "Identity check");
  await user.type(compose.getByRole("textbox", { name: "Message" }), "Original recipient only");
  await user.click(compose.getByRole("button", { name: "Review" }));
  expect(compose.getByText("claude-host (ag_claude)")).toBeVisible();
  directory = { ...sessionOverview, agents: [{ ...sessionOverview.agents[0]!, id: "ag_new" }] };
  onFrame?.({ cursor: 1, kind: "changed" });
  await waitFor(() => expect(compose.getByRole("alert")).toBeVisible());
  expect(compose.getByRole("button", { name: "Send" })).toBeDisabled();
  expect(compose.getByRole("checkbox", { name: /claude-host/ })).not.toBeChecked();
  expect(sent).toHaveLength(0);
  await user.click(compose.getByRole("button", { name: "Clear selection" }));
  expect(compose.queryByRole("heading", { name: "Check before sending" })).toBeNull();
  await user.click(compose.getByRole("checkbox", { name: /claude-host/ }));
  await user.click(compose.getByRole("button", { name: "Review" }));
  await user.click(compose.getByRole("button", { name: "Send" }));
  await waitFor(() => expect(sent).toHaveLength(1));
  expect(sent[0]?.to).toEqual(["ag_new"]);
});

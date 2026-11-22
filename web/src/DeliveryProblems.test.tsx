import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, describe, expect, it } from "vitest";

import { App } from "./App";
import { DeliveryProblems as DeliveryProblemsPanel } from "./DeliveryProblems";
import type { DeliveryProblems } from "./api";
import { formatWhen, loadLocale } from "./i18n";
import { server } from "./test/server";

const problems: DeliveryProblems = {
  window_s: 7 * 86400,
  wait_s: 15 * 60,
  more: false,
  messages: [
    {
      id: "ms_1",
      thread_id: "ms_root",
      subject: "Review the guard",
      created_at: "2026-11-06T10:00:00Z",
      recipients: [
        { name: "codex-gone", state: "pending", reason: "unreachable", execution: "ended" },
        { name: "codex-lost", state: "inflight", reason: "unreachable", execution: "lost" },
        { name: "codex-never", state: "pending", reason: "unreachable", execution: "none" },
        { name: "claude-busy", state: "pending", reason: "waiting", execution: "active" },
      ],
    },
  ],
};

describe("delivery problems", () => {
  it.each(["At-send display name", ""])("shows the immutable person beside the shared human sender (%s)", async (displayName) => {
    server.use(http.get("*/v1/ui/deliveries", () => HttpResponse.json({ ...problems, messages: [{ ...problems.messages[0], sender_account_id: "ca_author", sender_username: "at-send-username", sender_display_name: displayName }] })));
    render(<DeliveryProblemsPanel repositoryId="rp_iris" revision={0} onOpenThread={() => {}} />);
    const panel = await screen.findByRole("region", { name: "Delivery problems" });
    await userEvent.click(within(panel).getByRole("heading", { name: "Delivery problems" }));
    expect(within(panel).getByText(`${displayName || "at-send-username"} via human`)).toBeVisible();
  });
  it("names each recipient in trouble and why, states the thresholds, and offers no resend", async () => {
    const user = userEvent.setup({ delay: null });
    const seen: string[] = [];
    server.use(
      http.get("*/v1/ui/deliveries", ({ request }) => {
        seen.push(new URL(request.url).search);
        return HttpResponse.json(problems);
      }),
    );
    render(<App connect={() => () => {}} />);
    const panel = await screen.findByRole("region", { name: "Delivery problems" });
    expect(within(panel).getByRole("heading", { name: "Delivery problems" })).toBeVisible();
    expect(within(panel).queryByText(/Only the newest/)).toBeNull();
    await user.click(within(panel).getByRole("heading", { name: "Delivery problems" }));
    expect(panel).toHaveTextContent("the last 7 days");
    expect(panel).toHaveTextContent("15 minutes have passed");
    expect(panel).toHaveTextContent("does not send it again");
    const items = within(panel).getAllByRole("listitem").map((item) => item.textContent);
    expect(items).toContain("codex-gone: no live session (ended)");
    expect(items).toContain("codex-lost: no live session (lost)");
    expect(items).toContain("codex-never: no live session (none yet)");
    expect(items).toContain("claude-busy: not accepted after 15 minutes");
    expect(within(panel).queryByRole("button", { name: /send/i })).toBeNull();
    // The message opens its thread.
    const row = within(panel).getByRole("button", { name: /Review the guard/ });
    await user.click(row);
    expect(await screen.findByRole("heading", { name: "Rollout check" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Back to threads" }));
    expect(row).toHaveFocus();
    expect(seen[0]).toBe("");
  }, 20000);

  it("shows when older messages are omitted, even while the list is collapsed", async () => {
    const user = userEvent.setup({ delay: null });
    const messages = Array.from({ length: 200 }, (_, index) => ({
      ...problems.messages[0]!,
      id: `ms_${index}`,
    }));
    server.use(
      http.get("*/v1/ui/deliveries", () => HttpResponse.json({ ...problems, messages, more: true })),
    );
    render(<App connect={() => () => {}} />);
    const panel = await screen.findByRole("region", { name: "Delivery problems" });
    expect(within(panel).getByText("Only the newest 200 messages are shown; more exist.")).toBeVisible();
    expect(panel.querySelector("details")).not.toHaveAttribute("open");
    expect(within(panel).getAllByRole("button", { name: /Review the guard/ })[0]).not.toBeVisible();
    await user.click(within(panel).getByRole("heading", { name: "Delivery problems" }));
    expect(within(panel).getAllByRole("button", { name: /Review the guard/ })).toHaveLength(200);
    expect(within(panel).getByText("Only the newest 200 messages are shown; more exist.")).toBeVisible();
  });

  it("shows nothing when no delivery is in trouble", async () => {
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    expect(screen.queryByRole("region", { name: "Delivery problems" })).toBeNull();
  });

  it("says so when the list cannot be read, and signs out on a refused session", async () => {
    server.use(http.get("*/v1/ui/deliveries", () => new HttpResponse(null, { status: 500 })));
    const first = render(<App connect={() => () => {}} />);
    const panel = await screen.findByRole("region", { name: "Delivery problems" });
    expect(within(panel).getByRole("alert")).toHaveTextContent("Iris could not check your messages' delivery.");
    first.unmount();
    server.use(http.get("*/v1/ui/deliveries", () => new HttpResponse(null, { status: 401 })));
    render(<App connect={() => () => {}} />);
    expect(await screen.findByLabelText("Username")).toBeVisible();
  });
});

describe("delivery problem timestamp locale", () => {
  afterEach(async () => {
    await loadLocale("en");
  });

  it("formats delivery timestamps in Polish and updates when the locale changes", async () => {
    server.use(http.get("*/v1/ui/deliveries", () => HttpResponse.json(problems)));
    await loadLocale("pl");
    render(<DeliveryProblemsPanel repositoryId="" revision={0} onOpenThread={() => {}} />);

    const time = await screen.findByText(formatWhen(problems.messages[0]!.created_at, "pl"));
    expect(time).toHaveTextContent("lis");

    await act(async () => {
      await loadLocale("en");
    });
    expect(time).toHaveTextContent(formatWhen(problems.messages[0]!.created_at, "en"));
    expect(time).toHaveTextContent("Nov");
  });
});

// The hub's revision can move faster than this read takes. Aborting it on each move meant the
// list never arrived, so a move waits for the read and asks for one more; another repository
// still aborts it.
describe("delivery problems across revisions", () => {
  function held() {
    const reads: { search: string; aborted: () => boolean; answer: (response: Response) => void }[] = [];
    server.use(
      http.get("*/v1/ui/deliveries", ({ request }) => {
        let answer: (response: Response) => void = () => undefined;
        const response = new Promise<Response>((resolve) => {
          answer = resolve;
        });
        reads.push({ search: new URL(request.url).search, aborted: () => request.signal.aborted, answer });
        return response;
      }),
    );
    return reads;
  }

  it("finishes a read when the revision moves, then reads once more", async () => {
    const reads = held();
    const view = render(<DeliveryProblemsPanel repositoryId="rp_iris" revision={0} onOpenThread={() => {}} />);
    await waitFor(() => expect(reads).toHaveLength(1));
    view.rerender(<DeliveryProblemsPanel repositoryId="rp_iris" revision={1} onOpenThread={() => {}} />);
    view.rerender(<DeliveryProblemsPanel repositoryId="rp_iris" revision={2} onOpenThread={() => {}} />);
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(reads).toHaveLength(1);
    expect(reads[0]!.aborted()).toBe(false);
    await act(async () => {
      reads[0]!.answer(HttpResponse.json(problems));
    });
    await waitFor(() => expect(reads).toHaveLength(2));
    await act(async () => {
      reads[1]!.answer(HttpResponse.json(problems));
    });
    expect(await screen.findByRole("region", { name: "Delivery problems" })).toBeVisible();
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(reads).toHaveLength(2);
    // With no read running, a move reads at once.
    view.rerender(<DeliveryProblemsPanel repositoryId="rp_iris" revision={3} onOpenThread={() => {}} />);
    await waitFor(() => expect(reads).toHaveLength(3));
  });

  it("aborts the read for a repository it no longer shows, without saying it failed", async () => {
    const reads = held();
    const view = render(<DeliveryProblemsPanel repositoryId="rp_iris" revision={0} onOpenThread={() => {}} />);
    await waitFor(() => expect(reads).toHaveLength(1));
    view.rerender(<DeliveryProblemsPanel repositoryId="rp_other" revision={1} onOpenThread={() => {}} />);
    await waitFor(() => expect(reads).toHaveLength(2));
    expect(reads[0]!.aborted()).toBe(true);
    expect(reads[1]!.search).toContain("rp_other");
    await act(async () => {
      reads[1]!.answer(HttpResponse.json({ ...problems, messages: [] }));
    });
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(reads).toHaveLength(2);
    expect(screen.queryByRole("alert")).toBeNull();
  });
});

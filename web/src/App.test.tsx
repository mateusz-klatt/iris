import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import type { WatchFrame, WatchStatus } from "./api";
import i18n from "./i18n";
import { reviewAndSend } from "./test/send";
import { soundPreferenceKey } from "./notificationSound";
import { uiLocalePreferenceKey } from "./preferences";
import {
  doctorReport,
  emptyOverview,
  server,
  sessionOverview,
  setHumanUnread,
  threadDetail,
  threadsPage,
} from "./test/server";

function deferred<T>() {
  let resolve: (value: T) => void = () => undefined;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function abortError(): Error {
  const error = new Error("The operation was aborted.");
  error.name = "AbortError";
  return error;
}

function rejectFetch(match: (url: string) => boolean, error: Error) {
  const original = window.fetch.bind(window);
  vi.spyOn(window, "fetch").mockImplementation((input: RequestInfo | URL, init?: RequestInit) => {
    const url = input instanceof Request ? input.url : String(input);
    if (match(url)) {
      return Promise.reject(error);
    }
    return original(input, init);
  });
}

function blockLocalStorage(): () => void {
  const descriptor = Object.getOwnPropertyDescriptor(globalThis, "localStorage");
  Object.defineProperty(globalThis, "localStorage", {
    configurable: true,
    get() {
      throw new DOMException("storage is blocked", "SecurityError");
    },
  });
  return () => {
    if (descriptor === undefined) {
      Reflect.deleteProperty(globalThis, "localStorage");
      return;
    }
    Object.defineProperty(globalThis, "localStorage", descriptor);
  };
}

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      values.set(key, value);
    },
    read: (key: string) => values.get(key) ?? null,
  };
}

beforeEach(async () => {
  document.body.dataset.humanUnread = "";
  delete document.body.dataset.humanUnread;
  document.documentElement.lang = "en";
  await i18n.changeLanguage("en");
});

afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

async function showSession() {
  render(<App connect={() => () => {}} />);
  expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
}

async function fillCompose(user: ReturnType<typeof userEvent.setup>, body: string) {
  await user.click(screen.getByRole("button", { name: "+ New message" }));
  const compose = document.getElementById("compose");
  if (compose === null) {
    throw new Error("missing compose");
  }
  const view = within(compose);
  await user.click(view.getByRole("checkbox", { name: /claude-host/ }));
  await user.type(view.getByRole("textbox", { name: "Subject" }), "Check in");
  await user.type(view.getByRole("textbox", { name: "Message" }), body);
  return view;
}

// Gap review A9: a send keeps one retry key from the draft's start until it succeeds.
describe("console send retries", () => {
  it("retries a send whose response was lost with the same key", async () => {
    const user = userEvent.setup({ delay: null });
    const keys: string[] = [];
    let lose = true;
    server.use(
      http.post("*/v1/ui/messages", async ({ request }) => {
        keys.push(((await request.json()) as { key: string }).key);
        if (lose) {
          lose = false;
          return HttpResponse.error();
        }
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    const view = await fillCompose(user, "Are you there?");
    await reviewAndSend(user, view, "Send");
    // The response was lost, so the message may be stored: the person is told a retry is safe.
    expect(await screen.findByRole("alert")).toHaveTextContent("within a day of the first attempt is safe");
    await reviewAndSend(user, view, "Send");
    expect(await screen.findByText("Sent.")).toBeVisible();
    expect(keys).toHaveLength(2);
    expect(keys[1]).toBe(keys[0]);
  }, 20000);

  it("promises a safe retry only within a day of the first attempt", async () => {
    const user = userEvent.setup({ delay: null });
    let now = Date.parse("2026-11-08T10:00:00Z");
    const clock = vi.spyOn(Date, "now").mockImplementation(() => now);
    try {
      server.use(http.post("*/v1/ui/messages", () => HttpResponse.error()));
      await showSession();
      const view = await fillCompose(user, "Still there?");
      await reviewAndSend(user, view, "Send");
      expect(await screen.findByRole("alert")).toHaveTextContent("within a day of the first attempt");
      // The hub forgets a key a day after it stored it, so the same retry then promises nothing.
      now += 24 * 60 * 60 * 1000 + 1;
      await reviewAndSend(user, view, "Send");
      expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
    } finally {
      clock.mockRestore();
    }
  }, 20000);

  it("says a retry is safe after a 5xx, and not after a refusal", async () => {
    const user = userEvent.setup({ delay: null });
    let status = 502;
    server.use(http.post("*/v1/ui/messages", () => new HttpResponse(null, { status })));
    await showSession();
    const view = await fillCompose(user, "Anyone?");
    await reviewAndSend(user, view, "Send");
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not tell whether this message was sent.");
    status = 400;
    await reviewAndSend(user, view, "Send");
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
  }, 20000);

  it("sends once when Send is clicked again while the first is pending", async () => {
    const user = userEvent.setup({ delay: null });
    const pending = deferred<Response>();
    let calls = 0;
    server.use(
      http.post("*/v1/ui/messages", async () => {
        calls++;
        return pending.promise;
      }),
    );
    await showSession();
    const view = await fillCompose(user, "Once only");
    await user.click(view.getByRole("button", { name: "Review" }));
    const send = view.getByRole("button", { name: "Send" });
    await user.click(send);
    await waitFor(() => expect(send).toBeDisabled());
    await user.click(send);
    expect(calls).toBe(1);
    pending.resolve(new HttpResponse(null, { status: 204 }));
    expect(await screen.findByText("Sent.")).toBeVisible();
    expect(calls).toBe(1);
  }, 20000);

  it("sends once when Send is clicked twice before React re-renders", async () => {
    const user = userEvent.setup({ delay: null });
    const pending = deferred<Response>();
    let calls = 0;
    server.use(
      http.post("*/v1/ui/messages", async () => {
        calls++;
        return pending.promise;
      }),
    );
    await showSession();
    const view = await fillCompose(user, "Twice in one tick");
    await user.click(view.getByRole("button", { name: "Review" }));
    const send = view.getByRole("button", { name: "Send" });
    act(() => {
      send.click();
      send.click();
    });
    await waitFor(() => expect(calls).toBe(1));
    pending.resolve(new HttpResponse(null, { status: 204 }));
    expect(await screen.findByText("Sent.")).toBeVisible();
    expect(calls).toBe(1);
  }, 20000);

  it("keeps compose edits made while the previous version is being sent", async () => {
    const user = userEvent.setup({ delay: null });
    const pending = deferred<Response>();
    server.use(http.post("*/v1/ui/messages", () => pending.promise));
    await showSession();
    const view = await fillCompose(user, "First version");
    await reviewAndSend(user, view, "Send");
    await user.type(view.getByRole("textbox", { name: "Message" }), " with an extra detail");
    pending.resolve(new HttpResponse(null, { status: 204 }));
    expect(await screen.findByText("Sent.")).toBeVisible();
    expect(view.getByRole("textbox", { name: "Message" })).toHaveValue("First version with an extra detail");
    expect(view.getByRole("textbox", { name: "Subject" })).toHaveValue("Check in");
    expect(view.getByRole("checkbox", { name: /claude-host/ })).toBeChecked();
    expect(view.getByRole("button", { name: "Review" })).toBeEnabled();
  });

  it("gives a new key to the same message sent again after success, and to an edited draft", async () => {
    const user = userEvent.setup({ delay: null });
    const keys: string[] = [];
    let fail = false;
    server.use(
      http.post("*/v1/ui/messages", async ({ request }) => {
        keys.push(((await request.json()) as { key: string }).key);
        return fail ? HttpResponse.error() : new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    let view = await fillCompose(user, "ping");
    await reviewAndSend(user, view, "Send");
    expect(await screen.findByText("Sent.")).toBeVisible();
    view = await fillCompose(user, "ping");
    fail = true;
    await reviewAndSend(user, view, "Send");
    expect(await screen.findByRole("alert")).toBeVisible();
    await user.type(view.getByRole("textbox", { name: "Message" }), "!");
    await reviewAndSend(user, view, "Send");
    await waitFor(() => expect(keys).toHaveLength(3));
    expect(new Set(keys).size).toBe(3);
  }, 30000);
});

describe("Iris operator console", () => {
  // The watch learns of a refused session only by asking over HTTP; it then stops, and the
  // console shows how to sign in again rather than a live badge that never comes back.
  it("shows how to sign in when the watch says the session was refused", async () => {
    let report: ((status: WatchStatus) => void) | undefined;
    render(
      <App
        connect={(_, onStatus) => {
          report = onStatus;
          return () => {};
        }}
      />,
    );
    await screen.findByRole("heading", { name: "My messages" });
    act(() => report?.("live"));
    expect(screen.getByText("Live updates connected")).toBeVisible();
    act(() => report?.("unauthorized"));
    expect(await screen.findByText(/run iris ui and open the link/)).toBeVisible();
  });

  it("shows repositories, agents, and claims from the overview", async () => {
    const user = userEvent.setup();
    await showSession();
    await user.click(screen.getByRole("button", { name: /^Fleet/ }));
    expect(screen.getByRole("rowheader", { name: "iris" })).toBeVisible();
    expect(screen.getByRole("rowheader", { name: "claude-host" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    expect(screen.getByText("docs/api.md")).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Diagnostics" }));
    expect(screen.getByText("Hook received.")).toBeVisible();
    expect(screen.getAllByText("1 unread").length).toBeGreaterThan(0);
  });

  it("shows a live wait and the silent client's cause when nothing is in session", async () => {
    server.use(http.get("*/v1/ui/overview", () => HttpResponse.json(emptyOverview)));
    render(<App connect={() => () => {}} />);

    expect(await screen.findByRole("status")).toHaveTextContent("Waiting for the first session.");
    const silent = document.querySelector("[data-status='silent']");
    expect(silent).not.toBeNull();
    expect(silent).toHaveTextContent("Cause:");
    expect(silent).toHaveTextContent("No hook has arrived since install.");
    expect(screen.queryByRole("rowheader", { name: "claude-host" })).toBeNull();
    expect(screen.getByText("Not configured")).toBeVisible();
    expect(screen.getByText("Needs a human step")).toBeVisible();
  });

  it("points an unsigned browser at iris ui", async () => {
    server.use(
      http.get("*/v1/ui/overview", () => new HttpResponse(null, { status: 401 })),
      http.get("*/v1/ui/doctor", () => new HttpResponse(null, { status: 401 })),
    );
    render(<App connect={() => () => {}} />);

    expect(await screen.findByRole("heading", { name: "Iris" })).toBeVisible();
    expect(screen.getByText(/run iris ui and open the link/)).toBeVisible();
    expect(screen.queryByRole("heading", { name: "Clients" })).toBeNull();
  });

  it("revokes a claim and drops it from the next overview", async () => {
    const user = userEvent.setup();
    await showSession();
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await waitFor(() => {
      expect(screen.queryByText("docs/api.md")).toBeNull();
    });
  });

  it("searches threads, reads one, and keeps message bodies as text", async () => {
    const user = userEvent.setup();
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Rollout check" });

    expect(within(detail).getByRole("heading", { name: "Release" })).toBeVisible();
    expect(document.querySelector("script")).toBeNull();
    expect(within(detail).getByRole("link", { name: "note" })).toHaveAttribute(
      "href",
      "https://example.test/note",
    );
    expect(within(detail).getByText("blocked").closest("a")).toBeNull();
    expect(detail.querySelector("script")).toBeNull();
    expect(within(detail).getAllByText(/<script>alert\(1\)<\/script>/).length).toBeGreaterThan(0);

    await user.click(screen.getByRole("button", { name: "Back to threads" }));
    await user.type(screen.getByRole("searchbox", { name: "Search threads" }), "missing");
    await user.click(screen.getByRole("button", { name: "Search" }));
    expect(await screen.findByText("No threads in this view.")).toBeVisible();
  }, 20000);

  it("reads a thread from a repository's view, names another repository, and replies from that view", async () => {
    const user = userEvent.setup({ delay: null });
    const threadURLs: string[] = [];
    const bodies: unknown[] = [];
    server.use(
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...sessionOverview,
          repositories: [...sessionOverview.repositories, { id: "rp_sample", name: "sample", aliases: [] }],
        }),
      ),
      http.get("*/v1/ui/threads/:id", ({ request }) => {
        threadURLs.push(request.url);
        return HttpResponse.json({
          ...threadDetail,
          messages: [{ ...threadDetail.messages[0], repository_id: "rp_sample" }],
        });
      }),
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    await user.selectOptions(await within(await screen.findByRole("region", { name: "Threads" })).findByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Rollout check" });
    expect(within(detail).getByText("sample")).toBeVisible();
    expect(threadURLs.at(-1)).toContain("repository_id=rp_iris");
    await user.type(within(detail).getByRole("textbox", { name: "Message" }), "Seen.");
    await reviewAndSend(user, within(detail), "Reply");
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ repository_id: "rp_iris", reply_to: "ms_root" });
  }, 40000);

  it("gives a retry from another repository's view a new key", async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: { key: string; repository_id?: string }[] = [];
    server.use(
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...sessionOverview,
          repositories: [...sessionOverview.repositories, { id: "rp_sample", name: "sample", aliases: [] }],
        }),
      ),
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push((await request.json()) as { key: string; repository_id?: string });
        return bodies.length === 1 ? new HttpResponse(null, { status: 500 }) : new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    await user.click(screen.getByRole("button", { name: "+ New message" }));
    const compose = document.getElementById("compose");
    if (compose === null) {
      throw new Error("missing compose");
    }
    const view = within(compose);
    await user.click(view.getByRole("checkbox", { name: /claude-host/ }));
    await user.type(view.getByRole("textbox", { name: "Subject" }), "Status");
    await user.type(view.getByRole("textbox", { name: "Message" }), "Status?");
    await reviewAndSend(user, view, "Send");
    await waitFor(() => expect(bodies).toHaveLength(1));
    await user.selectOptions(view.getByRole("combobox", { name: "Send from repository" }), "rp_sample");
    await reviewAndSend(user, view, "Send");
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]?.repository_id).toBe("rp_sample");
    expect(bodies[1]?.key).not.toBe(bodies[0]?.key);
  }, 40000);

  it("reads the operator's mail in a thread when it is opened, and survives a failed read", async () => {
    const user = userEvent.setup({ delay: null });
    const reads: { url: string; body: unknown }[] = [];
    server.use(
      http.post("*/v1/ui/threads/:id/read", async ({ request }) => {
        reads.push({ url: request.url, body: await request.json() });
        return reads.length === 1 ? new HttpResponse(null, { status: 204 }) : new HttpResponse(null, { status: 500 });
      }),
    );
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await screen.findByRole("article", { name: "Rollout check" });
    await waitFor(() => expect(reads).toHaveLength(1));
    expect(reads[0]?.url).toContain("/v1/ui/threads/ms_root/read");
    expect(reads[0]?.body).toEqual({ repository_id: "" });
    await user.click(screen.getByRole("button", { name: "Back to threads" }));
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await waitFor(() => expect(reads).toHaveLength(2));
    expect(await screen.findByRole("article", { name: "Rollout check" })).toBeVisible();
  }, 40000);

  it("replies and sends as human", async () => {
    const user = userEvent.setup({ delay: null });
    const bodies: unknown[] = [];
    server.use(
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Rollout check" });
    const replyBody = within(detail).getByRole("textbox", { name: "Message" });
    await user.type(replyBody, "Noted.");
    await reviewAndSend(user, within(detail), "Reply");
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({
      to: ["claude-host"],
      subject: "Re: Rollout check",
      body: "Noted.",
      reply_to: "ms_root",
    });
    expect(bodies[0]).toMatchObject({ key: expect.stringMatching(/^human:/) });

    await user.click(screen.getByRole("button", { name: "+ New message" }));
    const compose = document.getElementById("compose");
    if (compose === null) {
      throw new Error("missing compose");
    }
    const view = within(compose);
    await user.click(view.getByRole("checkbox", { name: /claude-host/ }));
    await user.click(view.getByRole("checkbox", { name: /^human/ }));
    await user.type(view.getByRole("textbox", { name: "Subject" }), "Check in");
    await user.type(view.getByRole("textbox", { name: "Message" }), "Are you there?");
    await reviewAndSend(user, view, "Send");
    await waitFor(() => expect(bodies).toHaveLength(2));
    expect(bodies[1]).toMatchObject({
      to: ["ag_claude", "human"],
      subject: "Check in",
      body: "Are you there?",
      reply_to: "",
    });
    expect(await screen.findByText("Sent.")).toBeVisible();
  }, 40000);

  it("refreshes overview and the open thread when the watch reports a change", async () => {
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    const connect = (frame: (value: WatchFrame) => void, status?: (value: WatchStatus) => void) => {
      onFrame = frame;
      status?.("live");
      return () => undefined;
    };
    render(<App connect={connect} />);
    expect(await screen.findByText("Live updates connected")).toBeVisible();
    expect(screen.getAllByText("1 unread").length).toBeGreaterThan(0);

    setHumanUnread(3);
    onFrame?.({ cursor: 124, kind: "changed" });
    await waitFor(() => expect(screen.getByRole("button", { name: /^My messages/ })).toHaveTextContent(/^My messages\s*3$/));
  });

  it("reloads the overview after a read, a send, a revoke and a refresh without reconnecting", async () => {
    const user = userEvent.setup({ delay: null });
    let connects = 0;
    let stops = 0;
    let overviews = 0;
    server.events.on("request:start", ({ request }) => {
      if (new URL(request.url).pathname === "/v1/ui/overview") {
        overviews++;
      }
    });
    const connect = (_frame: (value: WatchFrame) => void, status?: (value: WatchStatus) => void) => {
      connects++;
      status?.("live");
      return () => {
        stops++;
      };
    };
    render(<App connect={connect} />);
    expect(await screen.findByText("Live updates connected")).toBeVisible();
    const reloaded = (after: number) => waitFor(() => expect(overviews).toBeGreaterThan(after));

    let before = overviews;
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await reloaded(before);
    before = overviews;
    await user.click(screen.getByRole("button", { name: "Back to threads" }));
    await reviewAndSend(user, await fillCompose(user, "Status?"), "Send");
    expect(await screen.findByText("Sent.")).toBeVisible();
    await reloaded(before);
    before = overviews;
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    await reloaded(before);
    before = overviews;
    await user.click(screen.getByRole("button", { name: "Diagnostics" }));
    await user.click(screen.getByRole("button", { name: "Reload the last doctor report" }));
    await reloaded(before);
    server.events.removeAllListeners();
    expect(connects).toBe(1);
    expect(stops).toBe(0);
  }, 40000);

  // The hub's socket sends a frame as soon as it opens. When a read reconnected it, that frame
  // reloaded the open thread, which read it again: about 75 reconnects a second in production.
  it("stays bounded with a thread open when the watch speaks on connect", async () => {
    const user = userEvent.setup({ delay: null });
    let connects = 0;
    let reads = 0;
    server.use(
      http.post("*/v1/ui/threads/:id/read", () => {
        reads++;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const connect = (frame: (value: WatchFrame) => void, status?: (value: WatchStatus) => void) => {
      connects++;
      status?.("live");
      queueMicrotask(() => frame({ cursor: connects, kind: "changed" }));
      return () => undefined;
    };
    render(<App connect={connect} />);
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await waitFor(() => expect(reads).toBeGreaterThan(0));
    await new Promise((resolve) => setTimeout(resolve, 500));
    expect(connects).toBe(1);
    expect(reads).toBeLessThanOrEqual(2);
  }, 40000);

  // Every watch frame reloads the open thread. With nothing unread for the operator, that GET
  // is all it costs: no read, and no second overview.
  it("reads only a thread with unread operator mail, and reloads only after a read that read", async () => {
    const user = userEvent.setup({ delay: null });
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    let reads = 0;
    let overviews = 0;
    let read = 0;
    let state = "acked";
    server.events.on("request:start", ({ request }) => {
      if (new URL(request.url).pathname === "/v1/ui/overview") {
        overviews++;
      }
    });
    server.use(
      http.get("*/v1/ui/threads/:id", () =>
        HttpResponse.json({ ...threadDetail, messages: threadDetail.messages.map((message) => ({ ...message, recipients: [{ name: "human", state }] })) }),
      ),
      http.post("*/v1/ui/threads/:id/read", () => {
        reads++;
        return HttpResponse.json({ read });
      }),
    );
    render(
      <App
        connect={(frame, status) => {
          onFrame = frame;
          status?.("live");
          return () => undefined;
        }}
      />,
    );
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await screen.findByRole("article", { name: "Rollout check" });
    const settled = overviews;
    onFrame?.({ cursor: 1, kind: "changed" });
    await waitFor(() => expect(overviews).toBe(settled + 1));
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(reads).toBe(0);

    // Unread, but the hub reads nothing (another repository's human, say): no reload.
    state = "pending";
    onFrame?.({ cursor: 2, kind: "changed" });
    await waitFor(() => expect(reads).toBe(1));
    await new Promise((resolve) => setTimeout(resolve, 100));
    expect(overviews).toBe(settled + 2);

    // A read that read reloads the overview once.
    read = 1;
    onFrame?.({ cursor: 3, kind: "changed" });
    await waitFor(() => expect(reads).toBe(2));
    await waitFor(() => expect(overviews).toBe(settled + 4));
    server.events.removeAllListeners();
  }, 40000);

  it("renders the hub's real empty report and claims with no expiry", async () => {
    server.use(
      http.get("*/v1/ui/doctor", () => HttpResponse.json({ hosts: [] })),
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...emptyOverview,
          claims: [
            {
              fence: 1,
              repository_id: "rp_1",
              path: "docs/",
              kind: "prefix",
              holder: "codex-host-api",
              origin: "explicit",
              expires_at: null,
            },
          ],
        }),
      ),
    );
    const user = userEvent.setup();
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(/No doctor report is stored yet. Run iris doctor/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    expect(screen.getByText("docs/")).toBeVisible();
  });

  it("keeps the doctor hint when the report is missing", async () => {
    server.use(http.get("*/v1/ui/doctor", () => new HttpResponse(null, { status: 404 })));
    render(<App connect={() => () => {}} />);
    await userEvent.setup().click(await screen.findByRole("button", { name: "Diagnostics" }));
    expect(screen.getByText(/No doctor report is stored yet. Run iris doctor/)).toBeVisible();
    expect(screen.getByText(/Run iris doctor on this computer to refresh it/)).toBeVisible();
  });

  it("remembers the interface language and the notification sound on this computer", async () => {
    const user = userEvent.setup();
    const stored = memoryStorage();
    vi.stubGlobal("localStorage", stored);
    vi.stubGlobal("AudioContext", undefined);
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });

    await user.click(
      screen.getByRole("button", { name: "Notification sound off. Click to unmute." }),
    );
    expect(stored.read(soundPreferenceKey)).toBe("on");
    await user.click(
      screen.getByRole("button", { name: "Notification sound on. Click to mute." }),
    );
    expect(stored.read(soundPreferenceKey)).toBe("off");
    expect(screen.getByRole("button", { name: "Notification sound off. Click to unmute." })).toHaveAttribute(
      "aria-pressed",
      "false",
    );

    await user.click(screen.getByRole("button", { name: /Choose language/ }));
    await user.click(screen.getByRole("option", { name: "Use Polski" }));
    await waitFor(() => {
      expect(stored.read(uiLocalePreferenceKey)).toBe("pl");
      expect(document.documentElement.lang).toBe("pl");
    });
    vi.unstubAllGlobals();
    await i18n.changeLanguage("en");
  });

  it("restores a saved language and still opens when storage is blocked", async () => {
    const stored = memoryStorage({ [uiLocalePreferenceKey]: "pl" });
    vi.stubGlobal("localStorage", stored);
    const saved = render(<App connect={() => () => {}} />);
    await waitFor(() => expect(document.documentElement.lang).toBe("pl"));
    expect(screen.getByRole("button", { name: /Polski/ })).toBeVisible();
    saved.unmount();
    vi.unstubAllGlobals();
    await i18n.changeLanguage("en");

    const restore = blockLocalStorage();
    try {
      render(<App connect={() => () => {}} />);
      expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
      expect(
        screen.getAllByRole("button", { name: "Notification sound off. Click to unmute." }).at(-1),
      ).toHaveAttribute("aria-pressed", "false");
    } finally {
      restore();
      await i18n.changeLanguage("en");
    }
  });

  it("shows an unreadable timestamp as given and an unknown repository id", async () => {
    const user = userEvent.setup();
    let doctorCalls = 0;
    server.use(
      http.get("*/v1/ui/doctor", () => {
        doctorCalls += 1;
        return HttpResponse.json({ hosts: [{ ...doctorReport, checked_at: doctorCalls === 1 ? "" : "soon" }] });
      }),
      http.get("*/v1/ui/overview", () =>
        HttpResponse.json({
          ...sessionOverview,
          agents: [{ ...sessionOverview.agents[0], repository_id: "rp_missing", name: "orphan-agent" }],
          claims: [
            { ...sessionOverview.claims[0], fence: 1, path: "empty-expiry", expires_at: "" },
            { ...sessionOverview.claims[0], fence: 2, path: "raw-expiry", expires_at: "soon" },
          ],
        }),
      ),
    );
    render(<App connect={() => () => {}} />);

    await user.click(await screen.findByRole("button", { name: /^Fleet/ }));
    expect(screen.getByRole("rowheader", { name: "orphan-agent" })).toBeVisible();
    expect(screen.getByRole("cell", { name: "rp_missing" })).toBeVisible();
    await user.click(screen.getByRole("button", { name: "Diagnostics" }));
    expect(screen.getByText(/^Checked\s*$/)).toBeVisible();
    await user.click(screen.getByRole("button", { name: /^Claims/ }));
    // A time the console cannot read is shown as given, never as a countdown or as expired.
    const raw = screen.getByText("raw-expiry").closest("li")!;
    expect(within(raw).getByText("soon")).toBeVisible();
    const blank = screen.getByText("empty-expiry").closest("li")!;
    expect(blank.querySelector(".claim-left")).toHaveTextContent(/^$/);

    await user.click(screen.getByRole("button", { name: "Diagnostics" }));
    await user.click(screen.getByRole("button", { name: "Reload the last doctor report" }));
    expect(await screen.findByText("Checked soon")).toBeVisible();
  });

  it("offers a retry when the overview fails for a reason other than signing in", async () => {
    const user = userEvent.setup();
    let calls = 0;
    server.use(
      http.get("*/v1/ui/overview", () => {
        calls += 1;
        if (calls === 1) {
          return new HttpResponse(null, { status: 500 });
        }
        return HttpResponse.json(sessionOverview);
      }),
    );
    render(<App connect={() => () => {}} />);

    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
    await user.click(screen.getByRole("button", { name: "Try again" }));
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
  });

  it("keeps the newer overview when an older request finishes afterwards", async () => {
    let calls = 0;
    const first = deferred<Response>();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    server.use(
      http.get("*/v1/ui/overview", () => {
        calls += 1;
        if (calls === 1) {
          return first.promise;
        }
        return HttpResponse.json(emptyOverview);
      }),
    );
    render(
      <App
        connect={(frame) => {
          onFrame = frame;
          return () => undefined;
        }}
      />,
    );

    await waitFor(() => expect(calls).toBe(1));
    onFrame?.({ cursor: 2, kind: "changed" });
    expect(await screen.findByRole("status")).toHaveTextContent("Waiting for the first session.");
    await act(async () => {
      first.resolve(HttpResponse.json(sessionOverview));
    });
    expect(screen.getByRole("status")).toHaveTextContent("Waiting for the first session.");
  });

  it("ignores an older overview failure once a newer request has succeeded", async () => {
    let calls = 0;
    const first = deferred<Response>();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    server.use(
      http.get("*/v1/ui/overview", () => {
        calls += 1;
        if (calls === 1) {
          return first.promise;
        }
        return HttpResponse.json(sessionOverview);
      }),
    );
    render(
      <App
        connect={(frame) => {
          onFrame = frame;
          return () => undefined;
        }}
      />,
    );

    await waitFor(() => expect(calls).toBe(1));
    onFrame?.({ cursor: 2, kind: "changed" });
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    await act(async () => {
      first.resolve(new HttpResponse(null, { status: 500 }));
    });
    expect(screen.getByRole("heading", { name: "My messages" })).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not refresh for an empty watch kind and shows a reconnecting feed", async () => {
    let calls = 0;
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    server.use(
      http.get("*/v1/ui/overview", () => {
        calls += 1;
        return HttpResponse.json(sessionOverview);
      }),
    );
    const { unmount } = render(
      <App
        connect={(frame) => {
          onFrame = frame;
          return () => undefined;
        }}
      />,
    );
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    const seen = calls;
    onFrame?.({ cursor: 1, kind: "" });
    await act(async () => {
      await Promise.resolve();
    });
    expect(calls).toBe(seen);
    onFrame?.({ cursor: 2, kind: "changed" });
    await waitFor(() => expect(calls).toBeGreaterThan(seen));
    unmount();

    render(
      <App
        connect={(_frame, status) => {
          status?.("reconnecting");
          return () => undefined;
        }}
      />,
    );
    expect(await screen.findByText("Reconnecting live updates…")).toBeVisible();
  });

  it("stays on the loading panel when the overview request is aborted", async () => {
    rejectFetch((url) => url.includes("/v1/ui/overview"), abortError());
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText("Loading messages…")).toBeVisible();
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.getByText("Loading messages…")).toBeVisible();
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Clients" })).toBeNull();
  });

  it("sends the reader to sign in when the thread list is no longer authorized", async () => {
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        if (new URL(request.url).pathname !== "/v1/ui/threads") {
          return undefined;
        }
        return new HttpResponse(null, { status: 401 });
      }),
    );
    render(<App connect={() => () => {}} />);
    expect(await screen.findByText(/run iris ui and open the link/)).toBeVisible();
  });

  it("reports a thread list failure and ignores an aborted list", async () => {
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        if (new URL(request.url).pathname !== "/v1/ui/threads") {
          return undefined;
        }
        return new HttpResponse(null, { status: 500 });
      }),
    );
    const { unmount } = render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
    expect(screen.getByText("No conversations with you yet")).toBeVisible();
    unmount();

    rejectFetch((url) => /\/v1\/ui\/threads\?/.test(url), abortError());
    render(<App connect={() => () => {}} />);
    expect(await screen.findByRole("heading", { name: "My messages" })).toBeVisible();
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByText("No conversations with you yet")).toBeVisible();
  });

  it("ignores a thread list failure from a query the reader already replaced", async () => {
    const user = userEvent.setup();
    const first = deferred<Response>();
    let staleStarted = false;
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        const url = new URL(request.url);
        if (url.pathname !== "/v1/ui/threads") {
          return undefined;
        }
        const q = url.searchParams.get("q") ?? "";
        if (q === "stale") {
          staleStarted = true;
          return first.promise;
        }
        if (q === "fresh") {
          return HttpResponse.json({
            threads: [{ ...threadsPage.threads[0], id: "ms_fresh", subject: "Fresh thread" }],
          });
        }
        return HttpResponse.json(threadsPage);
      }),
    );
    await showSession();
    const search = screen.getByRole("searchbox", { name: "Search threads" });
    await user.type(search, "stale");
    await user.click(screen.getByRole("button", { name: "Search" }));
    await waitFor(() => expect(staleStarted).toBe(true));
    await user.clear(search);
    await user.type(search, "fresh");
    await user.click(screen.getByRole("button", { name: "Search" }));
    expect(await screen.findByRole("button", { name: /Fresh thread/ })).toBeVisible();
    await act(async () => {
      first.resolve(new HttpResponse(null, { status: 500 }));
    });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.getByRole("button", { name: /Fresh thread/ })).toBeVisible();
  });

  it("filters threads by repository and agent", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        const url = new URL(request.url);
        if (url.pathname !== "/v1/ui/threads") {
          return undefined;
        }
        if (url.searchParams.get("repository_id") === "rp_iris" && url.searchParams.get("agent") === "claude-host") {
          return HttpResponse.json({
            threads: [{ ...threadsPage.threads[0], id: "ms_filtered", subject: "Filtered only" }],
          });
        }
        return HttpResponse.json(threadsPage);
      }),
    );
    await showSession();
    await user.selectOptions(within(screen.getByRole("region", { name: "Threads" })).getByRole("combobox", { name: "Repository" }), "rp_iris");
    await user.click(screen.getByRole("button", { name: "All activity" }));
    await user.selectOptions(screen.getByRole("combobox", { name: "Agent" }), "claude-host");
    expect(await screen.findByRole("button", { name: /Filtered only/ })).toBeVisible();
  });

  it("drops a thread that arrives after the reader has left it", async () => {
    const user = userEvent.setup();
    const pending = deferred<Response>();
    server.use(http.get("*/v1/ui/threads/:id", () => pending.promise));
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await user.click(screen.getByRole("button", { name: "Search" }));
    await act(async () => {
      pending.resolve(HttpResponse.json(threadDetail));
    });
    expect(screen.queryByRole("heading", { name: "Rollout check" })).toBeNull();
  });

  it("does not surface a thread error after the reader has left it", async () => {
    const user = userEvent.setup();
    const pending = deferred<Response>();
    server.use(http.get("*/v1/ui/threads/:id", () => pending.promise));
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await user.click(screen.getByRole("button", { name: "Search" }));
    await act(async () => {
      pending.resolve(new HttpResponse(null, { status: 500 }));
    });
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("shows a thread load failure, an expired session, and an aborted open as distinct outcomes", async () => {
    const user = userEvent.setup();
    server.use(http.get("*/v1/ui/threads/:id", () => new HttpResponse(null, { status: 500 })));
    const failed = render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
    expect(screen.queryByRole("article", { name: "Rollout check" })).toBeNull();
    failed.unmount();

    server.use(http.get("*/v1/ui/threads/:id", () => new HttpResponse(null, { status: 401 })));
    const expired = render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    expect(await screen.findByText(/run iris ui and open the link/)).toBeVisible();
    expired.unmount();

    rejectFetch((url) => url.includes("/v1/ui/threads/"), abortError());
    render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    await act(async () => {
      await Promise.resolve();
    });
    expect(screen.queryByRole("alert")).toBeNull();
    expect(screen.queryByRole("heading", { name: "Rollout check" })).toBeNull();
  });

  it("names an empty thread by its id", async () => {
    const user = userEvent.setup();
    server.use(
      http.get("*/v1/ui/threads/:id", () => HttpResponse.json({ id: "ms_root", messages: [] })),
    );
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "ms_root" });
    expect(within(detail).queryByRole("textbox", { name: "Recipients" })).toBeNull();
    expect(within(detail).queryByRole("button", { name: "Review" })).toBeNull();
  });

  it("keeps a subject that already starts with Re: and sends the edited reply", async () => {
    const user = userEvent.setup();
    const bodies: unknown[] = [];
    server.use(
      http.get("*/v1/ui/threads/:id", () =>
        HttpResponse.json({
          ...threadDetail,
          messages: [{ ...threadDetail.messages[0], subject: "Re: Rollout check" }],
        }),
      ),
      http.post("*/v1/ui/messages", async ({ request }) => {
        bodies.push(await request.json());
        return new HttpResponse(null, { status: 204 });
      }),
    );
    await showSession();
    await user.click(await screen.findByRole("button", { name: /Rollout check/ }));
    const detail = await screen.findByRole("article", { name: "Re: Rollout check" });
    const recipients = within(detail).getByRole("textbox", { name: "Recipients" });
    const subject = within(detail).getByRole("textbox", { name: "Subject" });
    expect(subject).toHaveValue("Re: Rollout check");
    await user.clear(recipients);
    await user.type(recipients, "human");
    await user.clear(subject);
    await user.type(subject, "Re: Noted");
    await user.type(within(detail).getByRole("textbox", { name: "Message" }), "Ok.");
    await reviewAndSend(user, within(detail), "Reply");
    await waitFor(() => expect(bodies).toHaveLength(1));
    expect(bodies[0]).toMatchObject({ to: ["human"], subject: "Re: Noted", body: "Ok.", reply_to: "ms_root" });
  }, 20000);

  it("keeps the draft when sending fails and signs out on an expired session", async () => {
    const user = userEvent.setup();
    const compose = async () => {
      await user.click(screen.getByRole("button", { name: "+ New message" }));
      const view = within(document.getElementById("compose") as HTMLElement);
      await user.click(view.getByRole("checkbox", { name: /^human/ }));
      await user.type(view.getByRole("textbox", { name: "Subject" }), "Check in");
      await user.type(view.getByRole("textbox", { name: "Message" }), "Are you there?");
      await reviewAndSend(user, view, "Send");
      return view;
    };

    server.use(http.post("*/v1/ui/messages", () => new HttpResponse(null, { status: 500 })));
    const failed = render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });
    const failedView = await compose();
    // A 5xx may have come after the hub stored the message, so a retry is offered as safe.
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not tell whether this message was sent.");
    expect(failedView.getByRole("textbox", { name: "Message" })).toHaveValue("Are you there?");
    failed.unmount();

    server.use(http.post("*/v1/ui/messages", () => new HttpResponse(null, { status: 401 })));
    render(<App connect={() => () => {}} />);
    await screen.findByRole("heading", { name: "My messages" });
    await compose();
    expect(await screen.findByText(/run iris ui and open the link/)).toBeVisible();
  }, 20000);

  it("leaves a claim in place when revocation fails and signs out when it is refused", async () => {
    const user = userEvent.setup();
    server.use(http.post("*/v1/ui/claims/:fence/revoke", () => new HttpResponse(null, { status: 500 })));
    const failed = render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /^Claims/ }));
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("Iris could not load this view.");
    expect(screen.getByText("docs/api.md")).toBeVisible();
    expect(screen.getByRole("button", { name: "Revoke" })).toBeEnabled();
    failed.unmount();

    server.use(http.post("*/v1/ui/claims/:fence/revoke", () => new HttpResponse(null, { status: 401 })));
    render(<App connect={() => () => {}} />);
    await user.click(await screen.findByRole("button", { name: /^Claims/ }));
    await user.click(screen.getByRole("button", { name: "Revoke" }));
    expect(await screen.findByText(/run iris ui and open the link/)).toBeVisible();
  });
});

import { act, fireEvent, render, screen } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import type { WatchFrame, WatchStatus } from "./api";
import type { AccountSession } from "./accountApi";
import i18n from "./i18n";
import { server, sessionOverview, threadDetail } from "./test/server";

interface EndpointCounts {
  overview: number;
  doctor: number;
  deliveries: number;
  threads: number;
  thread: number;
}

function trackRequests() {
  const counts: EndpointCounts = {
    overview: 0,
    doctor: 0,
    deliveries: 0,
    threads: 0,
    thread: 0,
  };
  const listener = ({ request }: { request: Request }) => {
    const path = new URL(request.url).pathname;
    if (path === "/v1/ui/overview") counts.overview++;
    else if (path === "/v1/ui/doctor") counts.doctor++;
    else if (path === "/v1/ui/deliveries") counts.deliveries++;
    else if (path === "/v1/ui/threads") counts.threads++;
    else if (path.startsWith("/v1/ui/threads/") && !path.endsWith("/read")) counts.thread++;
  };
  server.events.on("request:start", listener);
  return {
    counts,
    snapshot: (): EndpointCounts => ({ ...counts }),
    reset: () => {
      counts.overview = 0;
      counts.doctor = 0;
      counts.deliveries = 0;
      counts.threads = 0;
      counts.thread = 0;
    },
    stop: () => server.events.removeListener("request:start", listener),
  };
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

async function flushFakeNetwork(steps = 10) {
  for (let i = 0; i < steps; i++) {
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
    });
  }
}

function mockVisibility(initial: DocumentVisibilityState) {
  const original = Object.getOwnPropertyDescriptor(document, "visibilityState");
  let state = initial;
  Object.defineProperty(document, "visibilityState", {
    configurable: true,
    get: () => state,
  });
  return {
    set(next: DocumentVisibilityState) {
      state = next;
      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
    },
    restore() {
      if (original === undefined) {
        Reflect.deleteProperty(document, "visibilityState");
      } else {
        Object.defineProperty(document, "visibilityState", original);
      }
    },
  };
}

beforeEach(async () => {
  await i18n.changeLanguage("en");
});

afterEach(() => {
  vi.useRealTimers();
  vi.restoreAllMocks();
});

describe("console refresh scheduling", () => {
  it("coalesces a burst of frames inside the default 10 s window into one follow-up reload", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      render(
        <App
          connect={(frame, status?: (s: WatchStatus) => void) => {
            onFrame = frame;
            status?.("live");
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();
      expect(screen.getByRole("heading", { name: "My messages" })).toBeVisible();
      tracker.reset();

      // First frame when idle starts one immediate reload and opens the 10 s coalescing window.
      act(() => onFrame?.({ cursor: 1, kind: "changed" }));
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });

      // Burst of 12 frames 50 ms apart inside the 10 s window causes 0 immediate requests, then 1 coalesced reload.
      tracker.reset();
      for (let cursor = 2; cursor <= 13; cursor++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(50);
        });
        act(() => onFrame?.({ cursor, kind: "changed" }));
      }
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(9_500);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });
    } finally {
      tracker.stop();
    }
  });

  it("bounds 4.5 s watch frames to at most one reload per 10 s over 20 s and 60 s", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      server.use(http.post("*/v1/ui/threads/:id/read", () => HttpResponse.json({ read: 0 })));
      render(
        <App
          connect={(frame, status?: (s: WatchStatus) => void) => {
            onFrame = frame;
            status?.("live");
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      expect(screen.getByRole("article", { name: "Rollout check" })).toBeVisible();
      tracker.reset();

      // Deliver "changed" frames every 4.5 s across 60 s, sampling at 20 s and 60 s.
      let elapsed = 0;
      const advanceTo = async (targetMs: number) => {
        const delta = targetMs - elapsed;
        if (delta > 0) {
          await act(async () => {
            await vi.advanceTimersByTimeAsync(delta);
          });
          elapsed = targetMs;
        }
      };
      let at20s: EndpointCounts | undefined;
      for (let t = 4_500; t <= 60_000; t += 4_500) {
        if (at20s === undefined && t > 20_000) {
          await advanceTo(20_000);
          at20s = tracker.snapshot();
        }
        await advanceTo(t);
        act(() => onFrame?.({ cursor: t, kind: "changed" }));
        await flushFakeNetwork(5);
        elapsed += 5;
      }
      await advanceTo(60_000);
      await flushFakeNetwork();
      const at60s = tracker.snapshot();

      expect(at20s).toEqual({ overview: 2, doctor: 2, deliveries: 2, threads: 2, thread: 2 });
      expect(at60s).toEqual({ overview: 6, doctor: 6, deliveries: 6, threads: 6, thread: 6 });
    } finally {
      tracker.stop();
    }
  });

  it("keeps explicit send, read, and claim actions immediate even inside the 10 s window", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      server.use(http.post("*/v1/ui/threads/:id/read", () => HttpResponse.json({ read: 1 })));
      render(
        <App
          connect={(frame, status?: (s: WatchStatus) => void) => {
            onFrame = frame;
            status?.("live");
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();

      // Open the 10 s frame cooldown window.
      act(() => onFrame?.({ cursor: 1, kind: "changed" }));
      await flushFakeNetwork();
      tracker.reset();

      // 1. Explicit thread open -> readThread returns { read: 1 } -> immediate reload inside window.
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 0, threads: 0, thread: 1 });

      // 2. Explicit reply send inside window -> immediate reload + thread/deliveries refresh.
      tracker.reset();
      fireEvent.change(screen.getByRole("textbox", { name: "Message" }), { target: { value: "Immediate reply" } });
      fireEvent.click(screen.getByRole("button", { name: "Review" }));
      fireEvent.click(screen.getByRole("button", { name: "Reply" }));
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });

      // 3. Explicit claim revoke inside window -> immediate reload.
      fireEvent.click(screen.getByRole("button", { name: /^Claims/ }));
      tracker.reset();
      fireEvent.click(screen.getByRole("button", { name: "Revoke" }));
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 0, threads: 0, thread: 0 });
    } finally {
      tracker.stop();
    }
  });

  it("does not re-fetch the open thread twice for a member when overview updates on send", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    const memberSession: AccountSession = {
      csrf: "member-csrf",
      created_ms: 1,
      expires_ms: 9999999999999,
      account: { id: "ca_member", username: "reader", display_name: "Reader", role: "member", disabled: false, ui_locale: "en", correspondence_locale: "en", edit_revision: 1 },
    };
    const overviewReady = deferred<Response>();
    let overviewCalls = 0;
    try {
      server.use(
        http.get("*/v1/ui/session", () => HttpResponse.json(memberSession)),
        http.get("*/v1/ui/overview", () => {
          overviewCalls++;
          const body = {
            ...sessionOverview,
            repositories: sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "rw" as const })),
          };
          return overviewCalls === 2 ? overviewReady.promise : HttpResponse.json(body);
        }),
        http.get("*/v1/ui/threads/:id", () => HttpResponse.json(threadDetail)),
      );
      render(<App connect={() => () => {}} />);
      await flushFakeNetwork();
      fireEvent.change(screen.getByRole("combobox", { name: "Repository" }), { target: { value: "rp_iris" } });
      await flushFakeNetwork();
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      expect(screen.getByRole("article", { name: "Rollout check" })).toBeVisible();

      tracker.reset();
      fireEvent.change(screen.getByRole("textbox", { name: "Message" }), { target: { value: "Member reply" } });
      fireEvent.click(screen.getByRole("button", { name: "Review" }));
      fireEvent.click(screen.getByRole("button", { name: "Reply" }));
      await flushFakeNetwork();
      expect(tracker.snapshot().thread).toBe(1);

      // Resolving the delayed reload() updates overview (and replyAgents) without triggering a second thread GET.
      await act(async () => {
        overviewReady.resolve(HttpResponse.json({
          ...sessionOverview,
          repositories: sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "rw" as const })),
        }));
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 0, deliveries: 1, threads: 1, thread: 1 });
    } finally {
      tracker.stop();
    }
  });

  it("pauses polling while hidden and makes exactly one refresh on becoming visible", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      render(
        <App
          connect={(frame) => {
            onFrame = frame;
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();
      expect(screen.getByRole("heading", { name: "My messages" })).toBeVisible();

      // Hide the tab, deliver frames, and advance past fallback (60 s): zero requests while hidden.
      visibility.set("hidden");
      tracker.reset();
      for (let i = 1; i <= 5; i++) {
        act(() => onFrame?.({ cursor: i, kind: "changed" }));
        await act(async () => {
          await vi.advanceTimersByTimeAsync(12_000);
        });
      }
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      // Becoming visible triggers exactly one refresh; a second visible event makes none.
      visibility.set("visible");
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });

      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  it("coalesces becoming visible while an earlier refresh is still in flight", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let calls = 0;
    const second = deferred<Response>();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      server.use(
        http.get("*/v1/ui/overview", () => {
          calls++;
          return calls === 2 ? second.promise : HttpResponse.json(sessionOverview);
        }),
      );
      render(
        <App
          refreshGap={0}
          connect={(frame) => {
            onFrame = frame;
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();
      tracker.reset();

      act(() => onFrame?.({ cursor: 2, kind: "changed" }));
      await flushFakeNetwork(2);
      expect(calls).toBe(2);

      visibility.set("hidden");
      visibility.set("visible");
      expect(calls).toBe(2);

      await act(async () => {
        second.resolve(HttpResponse.json(sessionOverview));
      });
      await flushFakeNetwork();
      expect(calls).toBe(3);
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  it("fires the bounded 30 s fallback when idle and resets it on watch frames", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      server.use(http.post("*/v1/ui/threads/:id/read", () => HttpResponse.json({ read: 0 })));
      render(
        <App
          connect={(frame) => {
            onFrame = frame;
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      tracker.reset();

      // 20 s idle makes zero requests.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(20_000);
      });
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      // At 30 s total idle, the fallback fires once across active endpoints.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });

      // A watch frame at 15 s resets the 30 s fallback timer so 30 s from the first fallback does not fire early.
      tracker.reset();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(15_000);
      });
      act(() => onFrame?.({ cursor: 10, kind: "changed" }));
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(15_000);
      });
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(15_000);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 2, doctor: 2, deliveries: 2, threads: 2, thread: 2 });
    } finally {
      tracker.stop();
    }
  });

  it("keeps the post-load phase bounded when initial connect and burst frames arrive", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      render(
        <App
          connect={(frame, status?: (s: WatchStatus) => void) => {
            onFrame = frame;
            status?.("live");
            // Simulate hub's initial connect frame emitted immediately on open.
            queueMicrotask(() => frame({ cursor: 1, kind: "changed" }));
            return () => {};
          }}
        />,
      );
      await flushFakeNetwork();

      // Deliver 11 additional burst frames at 100 ms intervals right after load, then advance past 10 s.
      for (let cursor = 2; cursor <= 12; cursor++) {
        await act(async () => {
          await vi.advanceTimersByTimeAsync(100);
        });
        act(() => onFrame?.({ cursor, kind: "changed" }));
      }
      await act(async () => {
        await vi.advanceTimersByTimeAsync(8_900);
      });
      await flushFakeNetwork();

      // Initial load (1) + 1 coalesced reload when the 10 s window closes = 2 total rounds.
      expect(tracker.snapshot()).toEqual({ overview: 2, doctor: 2, deliveries: 2, threads: 2, thread: 0 });
    } finally {
      tracker.stop();
    }
  });
});

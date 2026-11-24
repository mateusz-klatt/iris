import { act, fireEvent, render, screen, within } from "@testing-library/react";
import { http, HttpResponse } from "msw";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "./App";
import type { WatchFrame, WatchStatus } from "./api";
import type { AccountSession } from "./accountApi";
import i18n from "./i18n";
import { server, sessionOverview, threadDetail, threadsPage } from "./test/server";

class FakeSocket {
  static latest: FakeSocket | null = null;
  static opened = 0;

  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(readonly url: string) {
    FakeSocket.latest = this;
    FakeSocket.opened++;
  }

  close() {
    this.onclose?.();
  }
}

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
    else if (request.method === "GET" && path.startsWith("/v1/ui/threads/") && !path.includes("/read")) counts.thread++;
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
  FakeSocket.latest = null;
  FakeSocket.opened = 0;
  vi.useRealTimers();
  vi.unstubAllGlobals();
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
      server.use(http.post("*/v1/ui/threads/:id/read/:message", () => HttpResponse.json({ read: 0 })));
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
      server.use(http.post("*/v1/ui/threads/:id/read/:message", () => HttpResponse.json({ read: 1 })));
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
      server.use(http.post("*/v1/ui/threads/:id/read/:message", () => HttpResponse.json({ read: 0 })));
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

  it("defers watch reconnect until visible on a hidden startup", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const visibility = mockVisibility("hidden");
    const tracker = trackRequests();
    let sessionChecks = 0;
    const onRequest = ({ request }: { request: Request }) => {
      if (new URL(request.url).pathname === "/v1/ui/session") {
        sessionChecks++;
      }
    };
    server.events.on("request:start", onRequest);
    try {
      render(<App />);
      await flushFakeNetwork();
      expect(FakeSocket.opened).toBe(1);
      expect(sessionChecks).toBe(1);

      // Closing the watch socket while still hidden must defer /session checks and reconnects.
      sessionChecks = 0;
      tracker.reset();
      act(() => {
        FakeSocket.latest?.close();
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      await flushFakeNetwork();
      expect(sessionChecks).toBe(0);
      expect(FakeSocket.opened).toBe(1);
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      // Becoming visible revalidates the session, opens the reconnect socket, and runs one refresh.
      visibility.set("visible");
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      await flushFakeNetwork();
      expect(sessionChecks).toBe(1);
      expect(FakeSocket.opened).toBe(2);
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });
    } finally {
      server.events.removeListener("request:start", onRequest);
      visibility.restore();
      tracker.stop();
    }
  });

  it("coalesces a resume with a positive ACK into one round", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let unread = 1;
    try {
      server.use(
        http.get("*/v1/ui/overview", () => HttpResponse.json({ ...sessionOverview, human_unread: unread })),
        http.post("*/v1/ui/threads/:id/read/:message", () => {
          unread = 0;
          return HttpResponse.json({ read: 1 });
        }),
      );
      render(<App connect={() => () => {}} />);
      await flushFakeNetwork();

      // Open an unread thread while hidden so the acknowledgement is deferred.
      visibility.set("hidden");
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      expect(screen.getByText("All checks passed.")).toBeVisible();
      expect(unread).toBe(1);

      // Returning to visible flushes the ACK and runs only one combined refresh round.
      tracker.reset();
      visibility.set("visible");
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(12_000);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });
      expect(screen.getByRole("button", { name: /^My messages\s*0$/ })).toBeVisible();
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  it("runs one resume when visibility bounces twice during the read and the refresh", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    const readGate = deferred<Response>();
    const overviewGate = deferred<Response>();
    let reads = 0;
    let overviews = 0;
    try {
      server.use(
        http.get("*/v1/ui/overview", () => {
          overviews++;
          return overviews === 1 ? HttpResponse.json(sessionOverview) : overviewGate.promise;
        }),
        http.post("*/v1/ui/threads/:id/read/:message", () => {
          reads++;
          return readGate.promise;
        }),
      );
      render(<App connect={() => () => {}} />);
      await flushFakeNetwork();
      expect(overviews).toBe(1);
      expect(screen.getByRole("heading", { name: "My messages" })).toBeVisible();

      // Open while hidden so the acknowledgement waits for the resume.
      visibility.set("hidden");
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      expect(screen.getByText("All checks passed.")).toBeVisible();
      expect(reads).toBe(0);

      // Show, hide, and show again before that read settles: still one POST and no refresh yet.
      tracker.reset();
      visibility.set("visible");
      visibility.set("hidden");
      visibility.set("visible");
      await flushFakeNetwork();
      expect(reads).toBe(1);
      expect(overviews).toBe(1);
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      await act(async () => {
        readGate.resolve(HttpResponse.json({ read: 1 }));
      });
      await flushFakeNetwork(2);
      expect(overviews).toBe(2);
      expect(reads).toBe(1);

      // The resume refresh is still in flight. Another bounce must not start a second round or a second read.
      visibility.set("hidden");
      visibility.set("visible");
      await flushFakeNetwork();
      expect(overviews).toBe(2);
      expect(reads).toBe(1);

      await act(async () => {
        overviewGate.resolve(HttpResponse.json({ ...sessionOverview, human_unread: 0 }));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(12_000);
      });
      await flushFakeNetwork();
      expect(reads).toBe(1);
      expect(overviews).toBe(2);
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 1 });
      expect(screen.getByRole("button", { name: /^My messages\s*0$/ })).toBeVisible();
      expect(screen.getByText("All checks passed.")).toBeVisible();
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  async function bouncedResumeWithDeferredRead() {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    const opening = deferred<Response>();
    const resume = deferred<Response>();
    const counts = { overviews: 0, details: 0 };
    const reads: DocumentVisibilityState[] = [];
    server.use(
      http.get("*/v1/ui/overview", () => {
        counts.overviews++;
        return counts.overviews === 2 ? resume.promise : HttpResponse.json(sessionOverview);
      }),
      http.get("*/v1/ui/threads/:id", () => {
        counts.details++;
        return counts.details === 1 ? opening.promise : HttpResponse.json(threadDetail);
      }),
      http.post("*/v1/ui/threads/:id/read/:message", () => {
        reads.push(document.visibilityState);
        return HttpResponse.json({ read: 1 });
      }),
    );
    render(<App connect={() => () => {}} />);
    await flushFakeNetwork();

    // The first show starts a resume while the opening has no rendered detail to read.
    visibility.set("hidden");
    fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
    await flushFakeNetwork(2);
    visibility.set("visible");
    visibility.set("hidden");
    await flushFakeNetwork(2);

    // Detail commits during the second hidden interval, so its read remains deferred.
    await act(async () => {
      opening.resolve(HttpResponse.json(threadDetail));
    });
    await flushFakeNetwork();
    visibility.set("visible");
    tracker.reset();

    return {
      counts,
      reads,
      visibility,
      settleResume: async () => {
        await act(async () => {
          resume.resolve(HttpResponse.json(sessionOverview));
        });
        await flushFakeNetwork();
      },
      finish: async () => {
        opening.resolve(HttpResponse.json(threadDetail));
        resume.resolve(HttpResponse.json(sessionOverview));
        await flushFakeNetwork();
        visibility.restore();
        tracker.stop();
      },
    };
  }

  it("flushes a read deferred by a visibility bounce when the resume settles, before the fallback", async () => {
    const view = await bouncedResumeWithDeferredRead();
    try {
      expect(view.counts).toEqual({ overviews: 2, details: 2 });
      expect(view.reads).toEqual([]);

      await view.settleResume();
      expect(view.reads).toEqual(["visible"]);
      expect(view.counts.overviews).toBe(2);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(29_000);
      });
      await flushFakeNetwork();
      expect(view.reads).toEqual(["visible"]);
      expect(view.counts.overviews).toBe(2);
    } finally {
      await view.finish();
    }
  });

  it("flushes a bounced resume's deferred read once when the detail closes first", async () => {
    const view = await bouncedResumeWithDeferredRead();
    try {
      fireEvent.click(screen.getByRole("button", { name: "Back to threads" }));
      await flushFakeNetwork();
      expect(view.reads).toEqual(["visible"]);

      await view.settleResume();
      expect(view.reads).toEqual(["visible"]);
      expect(view.counts.overviews).toBe(2);
    } finally {
      await view.finish();
    }
  });

  it("does not flush a bounced resume's deferred read while hidden at settle", async () => {
    const view = await bouncedResumeWithDeferredRead();
    try {
      view.visibility.set("hidden");
      await view.settleResume();
      expect(view.reads).toEqual([]);

      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      await flushFakeNetwork();
      expect(view.reads).toEqual([]);
      expect(view.counts.overviews).toBe(2);
    } finally {
      await view.finish();
    }
  });

  it("does not start a second resume refresh when a bounce defers a read", async () => {
    const view = await bouncedResumeWithDeferredRead();
    try {
      expect(view.counts.overviews).toBe(2);
      await view.settleResume();
      expect(view.counts.overviews).toBe(2);
      expect(view.reads).toEqual(["visible"]);

      act(() => {
        document.dispatchEvent(new Event("visibilitychange"));
      });
      await flushFakeNetwork();
      expect(view.counts.overviews).toBe(2);
    } finally {
      await view.finish();
    }
  });

  it("drops a post-read follow-up reload if the tab hides before acknowledgement settles", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    const ackReady = deferred<Response>();
    try {
      server.use(http.post("*/v1/ui/threads/:id/read/:message", () => ackReady.promise));
      render(<App connect={() => () => {}} />);
      await flushFakeNetwork();

      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      visibility.set("hidden");
      tracker.reset();

      await act(async () => {
        ackReady.resolve(HttpResponse.json({ read: 1 }));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(15_000);
      });
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  it("makes no reload if unmounted before a post-read acknowledgement settles", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    const ackReady = deferred<Response>();
    try {
      server.use(http.post("*/v1/ui/threads/:id/read/:message", () => ackReady.promise));
      const { unmount } = render(<App connect={() => () => {}} />);
      await flushFakeNetwork();

      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      unmount();
      tracker.reset();

      await act(async () => {
        ackReady.resolve(HttpResponse.json({ read: 1 }));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(15_000);
      });
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });
    } finally {
      tracker.stop();
    }
  });

  it("treats a fallback refresh as starting the cooldown window when a frame arrives at the deadline", async () => {
    vi.useFakeTimers();
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    const fallbackGate = deferred<Response>();
    let overviewCalls = 0;
    try {
      server.use(
        http.get("*/v1/ui/overview", () => {
          overviewCalls++;
          return overviewCalls === 3 ? fallbackGate.promise : HttpResponse.json(sessionOverview);
        }),
      );
      render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
      await flushFakeNetwork();

      // Advance cleanly past an initial frame + 10 s window so lastFrameStarted is non-zero.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(1_000);
      });
      act(() => onFrame?.({ cursor: 1, kind: "changed" }));
      await flushFakeNetwork();
      tracker.reset();

      // At the 30 s fallback deadline, deliver a watch frame while the fallback reload is running.
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      await flushFakeNetwork(2);
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });

      act(() => onFrame?.({ cursor: 2, kind: "changed" }));
      await act(async () => {
        fallbackGate.resolve(HttpResponse.json(sessionOverview));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_000);
      });
      expect(tracker.snapshot()).toEqual({ overview: 1, doctor: 1, deliveries: 1, threads: 1, thread: 0 });

      await act(async () => {
        await vi.advanceTimersByTimeAsync(5_000);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 2, doctor: 2, deliveries: 2, threads: 2, thread: 0 });
    } finally {
      tracker.stop();
    }
  });

  it("makes no requests after teardown across both deadlines and closed sockets", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let sessionChecks = 0;
    const onRequest = ({ request }: { request: Request }) => {
      if (new URL(request.url).pathname === "/v1/ui/session") {
        sessionChecks++;
      }
    };
    server.events.on("request:start", onRequest);
    try {
      const { unmount } = render(<App />);
      await flushFakeNetwork();

      // Cross both the 10 s cooldown deadline and the 30 s fallback deadline.
      act(() => {
        FakeSocket.latest?.onopen?.();
      });
      act(() => {
        FakeSocket.latest?.onmessage?.({ data: JSON.stringify({ cursor: 1, kind: "changed" }) });
        FakeSocket.latest?.onmessage?.({ data: JSON.stringify({ cursor: 2, kind: "changed" }) });
      });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(30_000);
      });
      await flushFakeNetwork();

      // Schedule a pending cooldown and reconnect timer, then unmount before they fire.
      act(() => {
        FakeSocket.latest?.onmessage?.({ data: JSON.stringify({ cursor: 3, kind: "changed" }) });
      });
      await flushFakeNetwork();
      act(() => {
        FakeSocket.latest?.onmessage?.({ data: JSON.stringify({ cursor: 4, kind: "changed" }) });
        FakeSocket.latest?.close();
      });
      await flushFakeNetwork();
      const openedBeforeUnmount = FakeSocket.opened;
      unmount();
      tracker.reset();
      sessionChecks = 0;

      visibility.set("hidden");
      visibility.set("visible");
      await act(async () => {
        await vi.advanceTimersByTimeAsync(60_000);
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });
      expect(sessionChecks).toBe(0);
      expect(FakeSocket.opened).toBe(openedBeforeUnmount);
    } finally {
      server.events.removeListener("request:start", onRequest);
      visibility.restore();
      tracker.stop();
    }
  });

  it("drops a resume reload if the tab hides while acknowledgement is in flight and coalesces visible ACKs during a run", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let ackGate = deferred<Response>();
    const overviewGate = deferred<Response>();
    let holdOverview = false;
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    try {
      server.use(
        http.get("*/v1/ui/overview", () => holdOverview ? overviewGate.promise : HttpResponse.json(sessionOverview)),
        http.post("*/v1/ui/threads/:id/read/:message", () => ackGate.promise),
      );
      render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
      await flushFakeNetwork();

      // Hide, defer read, become visible, then hide again before the resume ACK settles.
      visibility.set("hidden");
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      tracker.reset();

      visibility.set("visible");
      visibility.set("hidden");
      await act(async () => {
        ackGate.resolve(HttpResponse.json({ read: 1 }));
      });
      await flushFakeNetwork();
      expect(tracker.snapshot()).toEqual({ overview: 0, doctor: 0, deliveries: 0, threads: 0, thread: 0 });

      // Recover to visible, then click an unread thread while a frame refresh is in flight.
      ackGate = deferred<Response>();
      visibility.set("visible");
      await flushFakeNetwork();
      tracker.reset();

      holdOverview = true;
      act(() => onFrame?.({ cursor: 2, kind: "changed" }));
      await flushFakeNetwork(2);
      holdOverview = false;
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      await act(async () => {
        ackGate.resolve(HttpResponse.json({ read: 1 }));
      });
      await flushFakeNetwork();
      expect(tracker.counts.overview).toBe(1);

      await act(async () => {
        overviewGate.resolve(HttpResponse.json(sessionOverview));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
      await flushFakeNetwork();
      expect(tracker.counts.overview).toBe(2);
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });

  it("resolves a member's stale reply suggestion when overview updates unless edited", async () => {
    vi.useFakeTimers();
    const memberSession: AccountSession = {
      csrf: "member-csrf",
      created_ms: 1,
      expires_ms: 9999999999999,
      account: { id: "ca_member", username: "reader", display_name: "Reader", role: "member", disabled: false, ui_locale: "en", correspondence_locale: "en", edit_revision: 1 },
    };
    const baseOverview = {
      ...sessionOverview,
      repositories: sessionOverview.repositories.map((repo) => ({ ...repo, console_access: "rw" as const })),
    };
    const newAgent = { ...sessionOverview.agents[0]!, id: "ag_codex", name: "codex-new" };
    const sent: unknown[] = [];
    for (const editedBeforeUpdate of [false, true]) {
      let onFrame: ((frame: WatchFrame) => void) | undefined;
      let overviewCalls = 0;
      let threadCalls = 0;
      const delayedOverview = deferred<Response>();
      server.use(
        http.get("*/v1/ui/session", () => HttpResponse.json(memberSession)),
        http.get("*/v1/ui/overview", () => {
          overviewCalls++;
          return overviewCalls === 2 ? delayedOverview.promise : HttpResponse.json(baseOverview);
        }),
        http.get("*/v1/ui/threads/:id", () => {
          threadCalls++;
          return HttpResponse.json({
            ...threadDetail,
            messages: [{ ...threadDetail.messages[0]!, sender: "codex-new" }],
          });
        }),
        http.post("*/v1/ui/messages", async ({ request }) => {
          sent.push(await request.json());
          return new HttpResponse(null, { status: 204 });
        }),
      );
      const { unmount } = render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
      await flushFakeNetwork();
      fireEvent.change(screen.getByRole("combobox", { name: "Repository" }), { target: { value: "rp_iris" } });
      await flushFakeNetwork();

      // Start an overview refresh before opening so the thread detail resolves against the old directory first.
      act(() => onFrame?.({ cursor: 2, kind: "changed" }));
      fireEvent.click(screen.getByRole("button", { name: /Open thread: Rollout check/ }));
      await flushFakeNetwork();
      const replyForm = screen.getByRole("heading", { name: "Reply" }).closest("form")!;
      expect(within(replyForm).getByRole("alert")).toHaveTextContent("codex-new");
      expect(threadCalls).toBe(1);

      if (editedBeforeUpdate) {
        fireEvent.change(within(replyForm).getByRole("textbox", { name: "Message" }), { target: { value: "Keep my edit" } });
        fireEvent.click(within(replyForm).getByRole("checkbox", { name: /claude-host/ }));
      }

      // Overview adds codex-new without re-fetching thread detail.
      await act(async () => {
        delayedOverview.resolve(HttpResponse.json({
          ...baseOverview,
          agents: [...baseOverview.agents, newAgent],
        }));
      });
      await flushFakeNetwork();
      expect(threadCalls).toBe(1);

      if (!editedBeforeUpdate) {
        expect(within(replyForm).queryByRole("alert")).toBeNull();
        expect(within(replyForm).getByRole("checkbox", { name: /codex-new/ })).toBeChecked();
        fireEvent.change(within(replyForm).getByRole("textbox", { name: "Message" }), { target: { value: "Send to resolved id" } });
        fireEvent.click(within(replyForm).getByRole("button", { name: "Review" }));
        fireEvent.click(within(replyForm).getByRole("button", { name: "Reply" }));
        await flushFakeNetwork();
        expect(sent.at(-1)).toMatchObject({ to: ["ag_codex"], body: "Send to resolved id" });
      } else {
        expect(within(replyForm).getByRole("textbox", { name: "Message" })).toHaveValue("Keep my edit");
        expect(within(replyForm).getByRole("checkbox", { name: /claude-host/ })).toBeChecked();
        expect(within(replyForm).getByRole("checkbox", { name: /codex-new/ })).not.toBeChecked();
      }
      unmount();
      server.resetHandlers();
    }
  });

  it("defers the next page of an in-flight paginated thread load until visible", async () => {
    vi.useFakeTimers();
    const visibility = mockVisibility("visible");
    const tracker = trackRequests();
    let onFrame: ((frame: WatchFrame) => void) | undefined;
    let holdFirstPage = false;
    const pageOneGate = deferred<Response>();
    try {
      server.use(
        http.get("*/v1/ui/threads", ({ request }) => {
          const before = new URL(request.url).searchParams.get("before");
          if (before === "cursor_2") {
            return HttpResponse.json({
              threads: [{
                id: "ms_older",
                subject: "Older rollout",
                participants: ["claude-host", "human"],
                count: 1,
                last_at: "2026-11-05T10:05:00Z",
                unread: 0,
              }],
            });
          }
          if (holdFirstPage) {
            holdFirstPage = false;
            return pageOneGate.promise;
          }
          return HttpResponse.json({ ...threadsPage, next: "cursor_2" });
        }),
      );
      render(<App connect={(frame) => { onFrame = frame; return () => {}; }} />);
      await flushFakeNetwork();
      fireEvent.click(screen.getByRole("button", { name: "Older threads" }));
      await flushFakeNetwork();
      expect(screen.getByRole("button", { name: /Open thread: Older rollout/ })).toBeVisible();

      // Start page 1 of a refresh, hide the tab before page 1 returns, and assert page 2 waits until visible.
      holdFirstPage = true;
      act(() => onFrame?.({ cursor: 2, kind: "changed" }));
      await flushFakeNetwork(2);
      visibility.set("hidden");
      tracker.reset();

      await act(async () => {
        pageOneGate.resolve(HttpResponse.json({ ...threadsPage, next: "cursor_2" }));
      });
      await flushFakeNetwork();
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10_000);
      });
      expect(tracker.snapshot().threads).toBe(0);

      visibility.set("visible");
      await flushFakeNetwork();
      expect(tracker.snapshot().threads).toBe(2);
      expect(screen.getByRole("button", { name: /Open thread: Older rollout/ })).toBeVisible();
    } finally {
      visibility.restore();
      tracker.stop();
    }
  });
});

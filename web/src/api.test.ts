import { afterEach, describe, expect, it, vi } from "vitest";

import { ApiError, getDoctor, listThreads, revokeClaim, watchUi, type WatchFrame } from "./api";
import { server } from "./test/server";
import { http, HttpResponse } from "msw";

class FakeSocket {
  static latest: FakeSocket | null = null;

  onopen: (() => void) | null = null;
  onmessage: ((event: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;

  constructor(readonly url: string) {
    FakeSocket.latest = this;
  }

  close() {
    this.onclose?.();
  }
}

afterEach(() => {
  vi.unstubAllGlobals();
  vi.useRealTimers();
});

describe("operator watch", () => {
  it("refetches from a content-free frame and reconnects until stopped", () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const frames: WatchFrame[] = [];
    const statuses: string[] = [];
    const stop = watchUi(
      (frame) => frames.push(frame),
      (status) => statuses.push(status),
    );

    const socket = FakeSocket.latest;
    expect(socket?.url).toBe("ws://localhost:3000/v1/ui/watch");
    socket?.onopen?.();
    socket?.onmessage?.({ data: JSON.stringify({ cursor: 124, kind: "changed" }) });
    socket?.onmessage?.({ data: "{not json" });
    socket?.onmessage?.({ data: 12 });
    expect(frames).toEqual([{ cursor: 124, kind: "changed" }]);
    expect(statuses).toEqual(["connecting", "live"]);

    socket?.close();
    vi.advanceTimersByTime(1000);
    expect(statuses).toContain("reconnecting");
    expect(FakeSocket.latest).not.toBe(socket);

    stop();
    FakeSocket.latest?.close();
    const count = statuses.length;
    vi.advanceTimersByTime(5000);
    expect(statuses).toHaveLength(count);
  });

  // A socket that keeps failing is retried ever more slowly, up to 30 s, and an open resets it.
  it("backs off from a second up to thirty with jitter and starts over after an open", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    let checks = 0;
    const statuses: string[] = [];
    const stop = watchUi(() => undefined, (status) => statuses.push(status), {
      check: async () => {
        checks++;
      },
      random: () => 1,
    });
    const reconnectsAfter = async (ms: number) => {
      const socket = FakeSocket.latest;
      socket?.close();
      await vi.advanceTimersByTimeAsync(ms - 1);
      expect(FakeSocket.latest).toBe(socket);
      await vi.advanceTimersByTimeAsync(1);
      expect(FakeSocket.latest).not.toBe(socket);
    };
    for (const ms of [1000, 2000, 4000, 8000, 16000, 30000, 30000]) {
      await reconnectsAfter(ms);
    }
    expect(checks).toBe(7);
    FakeSocket.latest?.onopen?.();
    await reconnectsAfter(1000);
    expect(checks).toBe(7);
    expect(statuses.filter((status) => status === "reconnecting")).toHaveLength(8);
    stop();
  });

  it("waits at least half a step when the jitter is smallest", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const stop = watchUi(() => undefined, undefined, { check: async () => undefined, random: () => 0 });
    const socket = FakeSocket.latest;
    socket?.close();
    await vi.advanceTimersByTimeAsync(499);
    expect(FakeSocket.latest).toBe(socket);
    await vi.advanceTimersByTimeAsync(1);
    expect(FakeSocket.latest).not.toBe(socket);
    stop();
  });

  // A browser never sees a handshake's 401, so a socket that never opened asks over HTTP. A
  // refused session ends the watch instead of knocking every second for ever.
  it("stops and says unauthorized when the session is refused, and retries when the hub is only down", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const statuses: string[] = [];
    let answer: unknown = new TypeError("Failed to fetch");
    const stop = watchUi(() => undefined, (status) => statuses.push(status), {
      check: () => Promise.reject(answer),
      random: () => 1,
    });
    let socket = FakeSocket.latest;
    socket?.close();
    await vi.advanceTimersByTimeAsync(1000);
    expect(FakeSocket.latest).not.toBe(socket);

    answer = new ApiError(500);
    socket = FakeSocket.latest;
    socket?.close();
    await vi.advanceTimersByTimeAsync(2000);
    expect(FakeSocket.latest).not.toBe(socket);

    answer = new ApiError(401);
    socket = FakeSocket.latest;
    socket?.close();
    await vi.advanceTimersByTimeAsync(60000);
    expect(FakeSocket.latest).toBe(socket);
    expect(statuses.at(-1)).toBe("unauthorized");
    stop();
  });

  it("does nothing after stop, even when a check answers later", async () => {
    vi.useFakeTimers();
    vi.stubGlobal("WebSocket", FakeSocket);
    const statuses: string[] = [];
    const answers: { resolve: () => void; reject: (error: unknown) => void }[] = [];
    const stop = watchUi(() => undefined, (status) => statuses.push(status), {
      check: () => new Promise<void>((resolve, reject) => answers.push({ resolve, reject })),
    });
    FakeSocket.latest?.close();
    FakeSocket.latest?.close();
    expect(answers).toHaveLength(2);
    const socket = FakeSocket.latest;
    stop();
    answers[0]?.resolve();
    answers[1]?.reject(new ApiError(401));
    await vi.advanceTimersByTimeAsync(60000);
    expect(FakeSocket.latest).toBe(socket);
    expect(statuses).toEqual(["connecting"]);
  });

  it("opens a secure socket from an https page and omits an empty thread query", async () => {
    vi.stubGlobal("WebSocket", FakeSocket);
    for (const [page, socket] of [
      ["https://console.example/iris", "wss://console.example/v1/ui/watch"],
      ["http://localhost:3000/iris", "ws://localhost:3000/v1/ui/watch"],
    ]) {
      vi.stubGlobal("location", new URL(page as string));
      const stop = watchUi(() => undefined);
      expect(FakeSocket.latest?.url).toBe(socket);
      stop();
    }
    vi.unstubAllGlobals();

    let search = "unset";
    server.use(
      http.get("*/v1/ui/threads", ({ request }) => {
        const url = new URL(request.url);
        if (url.pathname !== "/v1/ui/threads" || url.search !== "") {
          return undefined;
        }
        search = url.search;
        return HttpResponse.json({ threads: [] });
      }),
    );
    await expect(listThreads({})).resolves.toEqual({ threads: [] });
    expect(search).toBe("");
  });

  it("revokes by fence and treats a missing doctor report as absent", async () => {
    let fence = "";
    server.use(
      http.post("*/v1/ui/claims/:fence/revoke", ({ params }) => {
        fence = String(params.fence);
        return new HttpResponse(null, { status: 204 });
      }),
      http.get("*/v1/ui/doctor", () => new HttpResponse(null, { status: 404 })),
    );

    await revokeClaim(12);
    await expect(getDoctor()).resolves.toBeNull();
    expect(fence).toBe("12");
  });
});

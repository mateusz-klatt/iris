import { afterEach, describe, expect, it, vi } from "vitest";

import { getDoctor, listThreads, revokeClaim, watchUi, type WatchFrame } from "./api";
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

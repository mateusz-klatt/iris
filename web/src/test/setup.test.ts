import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { drainRequests, teardownTest } from "./setup";
import { server } from "./server";

const realSetTimeout = globalThis.setTimeout.bind(globalThis);
const url = "http://iris.test/v1/ui/drain";

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((done) => {
    resolve = done;
  });
  return { promise, resolve };
}

function waitReal(ms: number): Promise<void> {
  return new Promise((resolve) => realSetTimeout(resolve, ms));
}

describe("MSW request drain", () => {
  it("drains a request whose response was not awaited", async () => {
    const started = deferred<void>();
    let finished = false;
    server.use(http.get(url, async () => {
      started.resolve();
      await waitReal(10);
      finished = true;
      return HttpResponse.json({ drained: true });
    }));

    const request = fetch(url);
    await started.promise;
    expect(finished).toBe(false);
    await drainRequests();

    expect(finished).toBe(true);
    await request;
  });

  it("does not match a drained request against replacement handlers", async () => {
    let originalFinished = false;
    let originalCalls = 0;
    let replacementCalls = 0;
    server.use(http.get(url, async () => {
      originalCalls++;
      await waitReal(10);
      originalFinished = true;
      return HttpResponse.json({ drained: true });
    }));

    const request = fetch(url);
    await drainRequests();
    await request;
    expect(originalFinished).toBe(true);
    expect(originalCalls).toBe(1);
    server.resetHandlers();
    server.use(http.get(url, () => {
      replacementCalls++;
      return HttpResponse.json({ drained: false });
    }));

    await waitReal(20);
    expect(replacementCalls).toBe(0);
  });

  it("fails with the method and URL of a request that never resolves and keeps tracking it", async () => {
    const pendingURL = `${url}/pending`;
    const started = deferred<void>();
    const gate = deferred<Response>();
    server.use(http.get(pendingURL, () => {
      started.resolve();
      return gate.promise;
    }));
    const request = fetch(pendingURL);

    try {
      await started.promise;
      await expect(drainRequests(10)).rejects.toThrow(`GET ${pendingURL}`);
      await expect(drainRequests(10)).rejects.toThrow(`GET ${pendingURL}`);
    } finally {
      gate.resolve(HttpResponse.json({ drained: true }));
      await request;
    }
  });

  it("releases an unhandled request and preserves the unhandled-frame failure", async () => {
    const unhandledURL = `${url}/unhandled`;
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);

    try {
      await expect(fetch(unhandledURL)).rejects.toMatchObject({
        cause: expect.objectContaining({
          message: expect.stringContaining('Cannot bypass a request when using the "error" strategy'),
        }),
      });
      await expect(drainRequests(10)).resolves.toBeUndefined();
    } finally {
      consoleError.mockRestore();
    }
  });

  it("releases a request whose handler throws", async () => {
    const failedURL = `${url}/failed`;
    const consoleError = vi.spyOn(console, "error").mockImplementation(() => undefined);
    server.use(http.get(failedURL, () => {
      throw new Error("drain handler failed");
    }));

    try {
      const response = await fetch(failedURL);
      expect(response.status).toBe(500);
      expect(response.statusText).toBe("Unhandled Exception");
      await expect(drainRequests(10)).resolves.toBeUndefined();
    } finally {
      consoleError.mockRestore();
    }
  });

  it("releases an aborted request whose handler never settles", async () => {
    const abortedURL = `${url}/aborted`;
    const started = deferred<void>();
    const controller = new AbortController();
    server.use(http.get(abortedURL, () => {
      started.resolve();
      return new Promise<Response>(() => undefined);
    }));
    const request = fetch(abortedURL, { signal: controller.signal });

    await started.promise;
    controller.abort();
    await expect(request).rejects.toMatchObject({ name: "AbortError" });
    await expect(drainRequests(10)).resolves.toBeUndefined();
  });

  it("drains a request after an undefined handler falls through", async () => {
    const sessionURL = "http://localhost:3000/v1/ui/session";
    server.use(http.get(sessionURL, () => undefined));

    const response = await fetch(sessionURL);
    expect(response.status).toBe(200);
    expect(await response.json()).toMatchObject({ account: { username: "operator" } });
    await expect(drainRequests(10)).resolves.toBeUndefined();
  });

  it("clears a timed-out request during teardown so the next teardown does not fail", async () => {
    const pendingURL = `${url}/timeout-cleanup`;
    const started = deferred<void>();
    const gate = deferred<Response>();
    server.use(http.get(pendingURL, () => {
      started.resolve();
      return gate.promise;
    }));
    const request = fetch(pendingURL);

    try {
      await started.promise;
      await expect(teardownTest(10)).rejects.toThrow(`GET ${pendingURL}`);
      await expect(teardownTest(10)).resolves.toBeUndefined();
    } finally {
      gate.resolve(HttpResponse.json({ drained: true }));
      await request;
    }
  });

  it("times out with real timers and a monotonic clock under fake timers", async () => {
    const pendingURL = `${url}/fake-timers`;
    const started = deferred<void>();
    const gate = deferred<Response>();
    server.use(http.get(pendingURL, () => {
      started.resolve();
      return gate.promise;
    }));
    const request = fetch(pendingURL);
    await started.promise;
    vi.useFakeTimers();
    vi.spyOn(Date, "now").mockReturnValue(0);
    vi.spyOn(performance, "now").mockReturnValue(0);

    try {
      await expect(Promise.race([
        drainRequests(20),
        new Promise<never>((_, reject) => {
          realSetTimeout(() => reject(new Error("drain did not use its real timer and clock")), 200);
        }),
      ])).rejects.toThrow(`GET ${pendingURL}`);
    } finally {
      vi.useRealTimers();
      vi.restoreAllMocks();
      gate.resolve(HttpResponse.json({ drained: true }));
      await request;
    }
  });
});

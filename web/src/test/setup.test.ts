import { http, HttpResponse } from "msw";
import { describe, expect, it, vi } from "vitest";

import { drainRequests } from "./setup";
import { server } from "./server";

const url = "http://iris.test/v1/ui/drain";
let firstRequestStarted = false;
let firstRequestFinished = false;

describe("MSW request drain", () => {
  it("starts a request without awaiting its response", async () => {
    let notifyStarted!: () => void;
    const started = new Promise<void>((resolve) => {
      notifyStarted = resolve;
    });
    server.use(http.get(url, async () => {
      firstRequestStarted = true;
      notifyStarted();
      await new Promise((resolve) => setTimeout(resolve, 10));
      firstRequestFinished = true;
      return HttpResponse.json({ drained: true });
    }));

    void fetch(url);

    await started;
    expect(firstRequestStarted).toBe(true);
    expect(firstRequestFinished).toBe(false);
  });

  it("drains the previous test's request before new handlers are installed", async () => {
    let calls = 0;
    server.use(http.get(url, () => {
      calls++;
      return HttpResponse.json({ drained: false });
    }));

    expect(firstRequestFinished).toBe(true);
    await new Promise((resolve) => setTimeout(resolve, 20));
    expect(calls).toBe(0);
  });

  it("fails the drain with the method and URL of a request that never resolves", async () => {
    const pendingURL = `${url}/pending`;
    let started = false;
    let release!: (response: Response) => void;
    const gate = new Promise<Response>((resolve) => {
      release = resolve;
    });
    server.use(http.get(pendingURL, () => {
      started = true;
      return gate;
    }));
    const request = fetch(pendingURL);

    try {
      await vi.waitFor(() => expect(started).toBe(true));
      await expect(drainRequests(10)).rejects.toThrow(`GET ${pendingURL}`);
    } finally {
      release(HttpResponse.json({ drained: true }));
      await request;
    }
  });
});

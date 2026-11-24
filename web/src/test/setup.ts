import "@testing-library/jest-dom/vitest";

import { cleanup, configure } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";

configure({ asyncUtilTimeout: 5000 });

import { resetFixtures, server } from "./server";
import { clearAccountSession } from "../accountApi";

const realSetTimeout = globalThis.setTimeout.bind(globalThis);
const monotonicNow = performance.now.bind(performance);
const inFlightRequests = new Map<string, Request>();

server.events.on("request:start", ({ requestId, request }) => {
  if (request.signal.aborted) return;
  inFlightRequests.set(requestId, request);
  request.signal.addEventListener("abort", () => {
    inFlightRequests.delete(requestId);
  }, { once: true });
});
server.events.on("request:end", ({ requestId }) => {
  inFlightRequests.delete(requestId);
});
server.events.on("request:unhandled", ({ requestId }) => {
  inFlightRequests.delete(requestId);
});
server.events.on("unhandledException", ({ requestId }) => {
  inFlightRequests.delete(requestId);
});

export async function drainRequests(timeoutMs = 2000): Promise<void> {
  const deadline = monotonicNow() + timeoutMs;
  await new Promise((resolve) => realSetTimeout(resolve, 0));
  while (inFlightRequests.size > 0) {
    const remaining = deadline - monotonicNow();
    if (remaining <= 0) {
      const pending = [...inFlightRequests.values()]
        .map((request) => `${request.method} ${request.url}`)
        .join("\n");
      throw new Error(`MSW request drain timed out after ${timeoutMs} ms:\n${pending}`);
    }
    await new Promise((resolve) => realSetTimeout(resolve, Math.min(5, remaining)));
  }
}

beforeAll(() => server.listen({ onUnhandledFrame: "error" }));

export async function teardownTest(timeoutMs = 2000): Promise<void> {
  cleanup();
  try {
    await drainRequests(timeoutMs);
  } finally {
    inFlightRequests.clear();
    clearAccountSession();
    resetFixtures();
    server.resetHandlers();
    if (window.location.hash !== "") {
      window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}`);
    }
  }
}

afterEach(() => teardownTest());

afterAll(() => server.close());

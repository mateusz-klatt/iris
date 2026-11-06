import "@testing-library/jest-dom/vitest";

import { cleanup, configure } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";

configure({ asyncUtilTimeout: 5000 });

import { resetFixtures, server } from "./server";

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));

afterEach(() => {
  cleanup();
  resetFixtures();
  server.resetHandlers();
});

afterAll(() => server.close());

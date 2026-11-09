import "@testing-library/jest-dom/vitest";

import { cleanup, configure } from "@testing-library/react";
import { afterAll, afterEach, beforeAll } from "vitest";

configure({ asyncUtilTimeout: 5000 });

import { resetFixtures, server } from "./server";
import { clearAccountSession } from "../accountApi";

beforeAll(() => server.listen({ onUnhandledRequest: "error" }));

afterEach(() => {
  cleanup();
  clearAccountSession();
  resetFixtures();
  server.resetHandlers();
});

afterAll(() => server.close());

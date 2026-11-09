import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "./api";
import { ConsoleClosedError, login, type AccountSession } from "./accountApi";
import Login from "./Login";
import i18n, * as languages from "./i18n";
import { uiLocalePreferenceKey } from "./preferences";

vi.mock("./accountApi", async (original) => ({ ...await original<typeof import("./accountApi")>(), login: vi.fn() }));

beforeEach(async () => {
  vi.resetAllMocks();
  const values = new Map<string, string>();
  vi.stubGlobal("localStorage", {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => values.set(key, value),
  });
  await i18n.changeLanguage("en");
});
afterEach(() => { vi.restoreAllMocks(); vi.unstubAllGlobals(); });

describe("Iris sign in", () => {
  it("submits credentials without changing password text and shows the pending state", async () => {
    let finish: (session: AccountSession) => void = () => undefined;
    vi.mocked(login).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    const onLogin = vi.fn();
    render(<Login onLogin={onLogin} />);
    const user = userEvent.setup();
    expect(screen.getByRole("heading", { name: "Sign in to Iris" })).toBeVisible();
    expect(screen.getByLabelText("Username")).toHaveAttribute("autocomplete", "username");
    expect(screen.getByLabelText("Password")).toHaveAttribute("autocomplete", "current-password");
    await user.type(screen.getByLabelText("Username"), "Iris operator");
    await user.type(screen.getByLabelText("Password"), "pass word  ");
    await user.click(screen.getByRole("button", { name: "Sign in" }));
    expect(login).toHaveBeenCalledWith("Iris operator", "pass word  ");
    fireEvent.submit(screen.getByRole("button", { name: "Signing in…" }).closest("form")!);
    expect(login).toHaveBeenCalledOnce();
    expect(screen.getByRole("button", { name: "Signing in…" })).toBeDisabled();
    const session = { csrf: "iris-csrf" } as AccountSession;
    finish(session);
    await vi.waitFor(() => expect(onLogin).toHaveBeenCalledWith(session));
    expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled();
  });

  it("uses only the signed-out browser language preference and lets the person change it", async () => {
    localStorage.setItem(uiLocalePreferenceKey, "pl");
    render(<Login onLogin={vi.fn()} />);
    await vi.waitFor(() => expect(document.documentElement.lang).toBe("pl"));
    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /Polski/ }));
    await user.click(screen.getByRole("option", { name: /English/ }));
    await vi.waitFor(() => expect(document.documentElement.lang).toBe("en"));
    expect(localStorage.getItem(uiLocalePreferenceKey)).toBe("en");
  });

  it("reports language loading failures instead of leaving an unhandled rejection", async () => {
    vi.spyOn(languages, "loadLocale").mockRejectedValue(new Error("language unavailable"));
    render(<Login onLogin={vi.fn()} />);
    expect(await screen.findByRole("alert")).toHaveTextContent("Could not save your language.");
  });

  it.each([
    [new ConsoleClosedError(), "Console sign-in is unavailable. Contact the hub owner."],
    [new ApiError(401), "Invalid username or password"],
    [new ApiError(429), "Too many attempts. Wait before trying again."],
    [new ApiError(403), "Could not sign in. Try again."],
    [new Error("offline"), "Could not sign in. Try again."],
  ])("shows safe feedback and erases the submitted password after a failure", async (failure, message) => {
    vi.mocked(login).mockRejectedValue(failure);
    render(<Login onLogin={vi.fn()} />);
    fireEvent.change(screen.getByLabelText("Password"), { target: { value: "private" } });
    fireEvent.submit(screen.getByRole("button", { name: "Sign in" }).closest("form")!);
    expect(await screen.findByRole("alert")).toHaveTextContent(message as string);
    expect(screen.getByLabelText("Password")).toHaveValue("");
    expect(screen.getByRole("button", { name: "Sign in" })).toBeEnabled();
  });
});

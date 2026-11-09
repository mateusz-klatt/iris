import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "./api";
import { changePassword, getSession, updateProfile, validNewPassword, type AccountSession } from "./accountApi";
import Account from "./Account";
import i18n from "./i18n";

vi.mock("./accountApi", async (original) => ({ ...await original<typeof import("./accountApi")>(), changePassword: vi.fn(), getSession: vi.fn(), updateProfile: vi.fn() }));

const session: AccountSession = {
  account: { id: "ca_iris", username: "iris-person", display_name: "Iris person", role: "member", disabled: false, ui_locale: "en", correspondence_locale: "pl", edit_revision: 3 },
  csrf: "iris-session-csrf", created_ms: 1, expires_ms: 2,
};

beforeEach(async () => { vi.resetAllMocks(); await i18n.changeLanguage("en"); });

function setup() {
  const onSession = vi.fn();
  const onSignedOut = vi.fn();
  render(<Account session={session} onSession={onSession} onSignedOut={onSignedOut} />);
  return { onSession, onSignedOut };
}

function submitProfile() {
  fireEvent.submit(screen.getByRole("button", { name: "Save display name" }).closest("form")!);
}

function enterPasswords(next = "a different valid password", confirm = next, current = "old password") {
  fireEvent.change(screen.getByLabelText("Current password"), { target: { value: current } });
  fireEvent.change(screen.getByLabelText("New password"), { target: { value: next } });
  fireEvent.change(screen.getByLabelText("Confirm new password"), { target: { value: confirm } });
  fireEvent.submit(screen.getByRole("button", { name: "Change password" }).closest("form")!);
}

describe("account profile", () => {
  it("shows identity and preserves account fields while saving a Unicode display name", async () => {
    const { onSession } = setup();
    expect(screen.getByText("iris-person")).toBeVisible();
    expect(screen.getByText("Member")).toBeVisible();
    expect(screen.queryByLabelText("Correspondence language")).not.toBeInTheDocument();
    expect(screen.getByLabelText("Display name")).not.toHaveAttribute("maxlength");
    let finish: (value: typeof session.account) => void = () => undefined;
    vi.mocked(updateProfile).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    const display = "😀".repeat(128);
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: display } });
    submitProfile();
    expect(updateProfile).toHaveBeenCalledWith(session.account, display, "en");
    expect(screen.getByRole("button", { name: "Saving display name…" })).toBeDisabled();
    const updated = { ...session.account, display_name: display, edit_revision: 4 };
    await act(async () => finish(updated));
    expect(onSession).toHaveBeenCalledWith({ ...session, account: updated });
    expect(screen.getByText("Display name saved.")).toBeVisible();
  });

  it("refuses overlong display names in Unicode characters", () => {
    setup();
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "😀".repeat(129) } });
    submitProfile();
    expect(screen.getByText("Display name must contain at most 128 Unicode characters.")).toBeVisible();
    expect(updateProfile).not.toHaveBeenCalled();
  });

  it("saves the interface language against the account profile", async () => {
    const { onSession } = setup();
    const updated = { ...session.account, ui_locale: "de", edit_revision: 4 };
    vi.mocked(updateProfile).mockResolvedValue(updated);
    fireEvent.change(screen.getByLabelText("Display name"), { target: { value: "Unsubmitted draft" } });
    fireEvent.change(screen.getByLabelText("Interface language"), { target: { value: "de" } });
    expect(updateProfile).toHaveBeenCalledWith(session.account, "Iris person", "de");
    await vi.waitFor(() => expect(onSession).toHaveBeenCalledWith({ ...session, account: updated }));
    expect(screen.getByLabelText("Display name")).toHaveValue("Unsubmitted draft");
    expect(screen.getByText("Language saved for your account")).toBeVisible();
  });

  it("refreshes a revision conflict before allowing another save", async () => {
    const { onSession } = setup();
    const latest = { ...session, account: { ...session.account, display_name: "Changed elsewhere", edit_revision: 4 } };
    vi.mocked(updateProfile).mockRejectedValue(new ApiError(409));
    vi.mocked(getSession).mockResolvedValue(latest);
    submitProfile();
    expect(await screen.findByText("Your profile changed elsewhere. We refreshed it; review and try again.")).toBeVisible();
    expect(getSession).toHaveBeenCalledWith(undefined, session);
    expect(onSession).toHaveBeenCalledWith(latest);
    expect(screen.getByLabelText("Display name")).toHaveValue("Changed elsewhere");
  });

  it.each([new ApiError(400), new Error("offline")])("keeps profile errors recoverable", async (failure) => {
    setup();
    vi.mocked(updateProfile).mockRejectedValue(failure);
    submitProfile();
    expect(await screen.findByText("The display name could not be saved.")).toBeVisible();
  });

  it.each([new ApiError(401), new Error("offline")])("handles an unavailable conflict refresh", async (failure) => {
    const { onSession, onSignedOut } = setup();
    vi.mocked(updateProfile).mockRejectedValue(new ApiError(409));
    vi.mocked(getSession).mockRejectedValue(failure);
    submitProfile();
    if (failure instanceof ApiError) {
      await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
    } else {
      expect(await screen.findByText("The display name could not be saved.")).toBeVisible();
      expect(onSignedOut).not.toHaveBeenCalled();
    }
    expect(getSession).toHaveBeenCalledWith(undefined, session);
    expect(onSession).not.toHaveBeenCalled();
  });

  it("ends the browser account view when its session is refused", async () => {
    const { onSignedOut } = setup();
    vi.mocked(updateProfile).mockRejectedValue(new ApiError(401));
    submitProfile();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
  });

  it("clears the view when a saved profile belongs to another browser account", async () => {
    const { onSession, onSignedOut } = setup();
    const other = { ...session.account, id: "ca_other", display_name: "Another person" };
    vi.mocked(updateProfile).mockResolvedValue(other);
    submitProfile();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
    expect(onSession).not.toHaveBeenCalled();
    expect(screen.getByLabelText("Display name")).toHaveValue("Iris person");
    expect(screen.queryByText("Another person")).not.toBeInTheDocument();
  });
});

describe("account password", () => {
  it("validates ASCII, multibyte and non-BMP code-point and byte boundaries", () => {
    expect(validNewPassword("a".repeat(14))).toBe(false);
    expect(validNewPassword("a".repeat(15))).toBe(true);
    expect(validNewPassword("a".repeat(1024))).toBe(true);
    expect(validNewPassword("a".repeat(1025))).toBe(false);
    expect(validNewPassword("é".repeat(15))).toBe(true);
    expect(validNewPassword("é".repeat(512))).toBe(true);
    expect(validNewPassword("é".repeat(513))).toBe(false);
    expect(validNewPassword("😀".repeat(14))).toBe(false);
    expect(validNewPassword("😀".repeat(15))).toBe(true);
    expect(validNewPassword("😀".repeat(256))).toBe(true);
    expect(validNewPassword("😀".repeat(257))).toBe(false);
  });

  it("requires confirmation without sending a mismatched password", () => {
    setup();
    enterPasswords("a valid new password", "different confirmation");
    expect(screen.getByText("The new password and confirmation do not match.")).toBeVisible();
    expect(changePassword).not.toHaveBeenCalled();
  });

  it.each([["😀".repeat(14), "old"], ["é".repeat(513), "old"]])("enforces password byte bounds before submitting", (next, current) => {
    setup();
    enterPasswords(next, next, current);
    expect(screen.getByText("Use at least 15 Unicode characters and at most 1024 UTF-8 bytes.")).toBeVisible();
    expect(changePassword).not.toHaveBeenCalled();
  });

  it("bounds imported current passwords without imposing the new password minimum", () => {
    setup();
    enterPasswords("a valid new password", "a valid new password", "é".repeat(513));
    expect(screen.getByText("The password could not be changed. Check the current password and requirements.")).toBeVisible();
    expect(changePassword).not.toHaveBeenCalled();
  });

  it("changes an unmodified password then clears inputs and ends the session", async () => {
    const { onSignedOut } = setup();
    let finish: () => void = () => undefined;
    vi.mocked(changePassword).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    const next = "😀".repeat(15) + "  ";
    enterPasswords(next, next, "old password  ");
    expect(screen.getByRole("button", { name: "Changing password…" })).toBeDisabled();
    expect(screen.getByLabelText("New password")).not.toHaveAttribute("minlength");
    expect(screen.getByLabelText("New password")).not.toHaveAttribute("maxlength");
    expect(changePassword).toHaveBeenCalledWith("old password  ", next);
    await act(async () => finish());
    expect(onSignedOut).toHaveBeenCalledOnce();
    expect(screen.getByLabelText("Current password")).toHaveValue("");
    expect(screen.getByLabelText("New password")).toHaveValue("");
    expect(screen.getByLabelText("Confirm new password")).toHaveValue("");
  });

  it.each([new ApiError(429), new ApiError(400), new Error("offline")])("shows password failure feedback without ending a healthy session", async (failure) => {
    const { onSignedOut } = setup();
    vi.mocked(changePassword).mockRejectedValue(failure);
    enterPasswords();
    const message = failure instanceof ApiError && failure.status === 429
      ? "Too many attempts. Wait before trying again."
      : "The password could not be changed. Check the current password and requirements.";
    expect(await screen.findByText(message)).toBeVisible();
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it("distinguishes an incorrect current password from a revoked session", async () => {
    const { onSignedOut } = setup();
    vi.mocked(changePassword).mockRejectedValue(new ApiError(401));
    vi.mocked(getSession).mockResolvedValue(session);
    enterPasswords();
    expect(await screen.findByText("The password could not be changed. Check the current password and requirements.")).toBeVisible();
    expect(getSession).toHaveBeenCalledWith(undefined, session);
    expect(onSignedOut).not.toHaveBeenCalled();
    vi.mocked(getSession).mockRejectedValue(new ApiError(401));
    enterPasswords();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
    expect(getSession).toHaveBeenLastCalledWith(undefined, session);
  });
});

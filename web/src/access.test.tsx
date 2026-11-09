import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, type Repository } from "./api";
import { listAccounts, updateAccess, type AccountSession, type ConsoleAccessAccount } from "./accountApi";
import AccessPanel from "./access";
import i18n from "./i18n";

vi.mock("./accountApi", async (original) => ({ ...await original<typeof import("./accountApi")>(), listAccounts: vi.fn(), updateAccess: vi.fn() }));

const operator: ConsoleAccessAccount = { id: "ca_operator", username: "Operator", display_name: "Ops person", role: "operator", disabled: false, ui_locale: "en", correspondence_locale: "", edit_revision: 1, grants: [] };
const member: ConsoleAccessAccount = { ...operator, id: "ca_member", username: "Member", display_name: "", role: "member", edit_revision: 4 };
const otherOperator = { ...operator, id: "ca_other", username: "Other operator" };
const repositories: Repository[] = [{ id: "rp_a", name: "Repository A", aliases: [] }, { id: "rp_b", name: "Repository B", aliases: [] }];
const session: AccountSession = { account: operator, csrf: "synthetic-csrf", created_ms: 1, expires_ms: 2 };

beforeEach(async () => {
  vi.resetAllMocks();
  await i18n.changeLanguage("en");
  vi.mocked(listAccounts).mockResolvedValue([operator, member]);
});

function setup(accountSession = session, repos = repositories) {
  const onSignedOut = vi.fn();
  const view = render(<AccessPanel session={accountSession} repositories={repos} onSignedOut={onSignedOut} />);
  return { ...view, onSignedOut };
}

async function chooseMember() {
  fireEvent.click(await screen.findByRole("button", { name: "Member" }));
}

function submit() {
  fireEvent.submit(screen.getByRole("button", { name: "Save access" }).closest("form")!);
}

describe("operator access editor", () => {
  it.each(["member", "none"])("refuses the %s role without requesting an account list", (role) => {
    setup({ ...session, account: { ...member, role } });
    expect(screen.getByRole("alert")).toHaveTextContent("Operator access is required.");
    expect(listAccounts).not.toHaveBeenCalled();
    expect(updateAccess).not.toHaveBeenCalled();
  });

  it("refuses a disabled operator without requesting account records", () => {
    setup({ ...session, account: { ...operator, disabled: true } });
    expect(screen.getByRole("alert")).toHaveTextContent("Operator access is required.");
    expect(listAccounts).not.toHaveBeenCalled();
  });

  it("protects the last enabled operator and shows the global scope", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, member, { ...otherOperator, disabled: true }]);
    setup();
    expect(await screen.findByLabelText("Role")).toBeDisabled();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    expect(screen.getByText("The last enabled operator cannot be disabled or demoted.")).toBeVisible();
    expect(screen.getByText("Operators have access to every repository.")).toBeVisible();
    expect(screen.queryByLabelText("Repository A access")).not.toBeInTheDocument();
  });

  it("saves explicit repository grants with the selected account revision, including no grant", async () => {
    setup();
    await chooseMember();
    expect(screen.getByLabelText("Repository A access")).toHaveValue("");
    expect(screen.getByLabelText("Repository B access")).toHaveValue("");
    const updated = { ...member, edit_revision: 5, grants: [{ repository_id: "rp_a", mode: "rw" as const }] };
    vi.mocked(updateAccess).mockResolvedValue(updated);
    fireEvent.change(screen.getByLabelText("Repository A access"), { target: { value: "ro" } });
    fireEvent.change(screen.getByLabelText("Repository B access"), { target: { value: "rw" } });
    fireEvent.change(screen.getByLabelText("Repository A access"), { target: { value: "rw" } });
    fireEvent.change(screen.getByLabelText("Repository B access"), { target: { value: "" } });
    submit();
    await screen.findByText("Access saved. The account must sign in again.");
    expect(updateAccess).toHaveBeenCalledWith(member.id, { edit_revision: 4, role: "member", disabled: false, grants: [{ repository_id: "rp_a", mode: "rw" }] });
    expect(screen.getByLabelText("Repository A access")).toHaveValue("rw");
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, expect.objectContaining({ edit_revision: 5 })));
  });

  it("loads existing grants and resets unsaved changes when selecting another account", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, grants: [{ repository_id: "rp_a", mode: "ro" }] }]);
    setup();
    await chooseMember();
    expect(screen.getByLabelText("Repository A access")).toHaveValue("ro");
    fireEvent.change(screen.getByLabelText("Repository A access"), { target: { value: "rw" } });
    fireEvent.click(screen.getByRole("button", { name: "Operator" }));
    fireEvent.click(screen.getByRole("button", { name: "Member" }));
    expect(screen.getByLabelText("Repository A access")).toHaveValue("ro");
    expect(screen.getByRole("button", { name: "Member" })).toHaveAttribute("aria-pressed", "true");
  });

  it("keeps disabled accounts editable, forces none disabled and removes grants on promotion", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, disabled: true, grants: [{ repository_id: "rp_a", mode: "rw" }] }]);
    setup();
    await chooseMember();
    expect(screen.getByLabelText("Disabled")).toBeChecked();
    fireEvent.click(screen.getByLabelText("Disabled"));
    vi.mocked(updateAccess).mockResolvedValue({ ...member, role: "none", disabled: true, edit_revision: 5 });
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "none" } });
    expect(screen.getByLabelText("Disabled")).toBeChecked();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    submit();
    await screen.findByText("Access saved. The account must sign in again.");
    expect(updateAccess).toHaveBeenLastCalledWith(member.id, { edit_revision: 4, role: "none", disabled: true, grants: [] });
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "operator" } });
    fireEvent.click(screen.getByLabelText("Disabled"));
    vi.mocked(updateAccess).mockResolvedValue({ ...member, role: "operator", edit_revision: 6 });
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, { edit_revision: 5, role: "operator", disabled: false, grants: [] }));
  });

  it("refreshes conflicting permissions and requires a reviewed retry with the new revision", async () => {
    setup();
    await chooseMember();
    const latest = { ...member, edit_revision: 8, grants: [{ repository_id: "rp_a", mode: "ro" as const }] };
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockResolvedValue([operator, latest]);
    fireEvent.change(screen.getByLabelText("Repository B access"), { target: { value: "rw" } });
    submit();
    expect(await screen.findByText("Access changed or the last enabled operator would be removed. Review current accounts before trying again.")).toBeVisible();
    expect(screen.getByLabelText("Repository A access")).toHaveValue("ro");
    expect(screen.getByLabelText("Repository B access")).toHaveValue("");
    vi.mocked(updateAccess).mockResolvedValue({ ...latest, edit_revision: 9 });
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, expect.objectContaining({ edit_revision: 8 })));
  });

  it("accepts a last-operator refusal after another operator disappears", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, otherOperator]);
    setup();
    expect(await screen.findByLabelText("Role")).toBeEnabled();
    fireEvent.change(screen.getByLabelText("Role"), { target: { value: "member" } });
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockResolvedValue([operator]);
    submit();
    await screen.findByText("Access changed or the last enabled operator would be removed. Review current accounts before trying again.");
    expect(screen.getByLabelText("Role")).toBeDisabled();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    expect(screen.getByLabelText("Role")).toHaveValue("operator");
  });

  it("ends the current session after an access edit to self", async () => {
    const { onSignedOut } = setup();
    await screen.findByLabelText("Role");
    vi.mocked(updateAccess).mockResolvedValue({ ...operator, edit_revision: 2 });
    submit();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
  });

  it("supports keyboard selection, native controls, and submit while freezing a pending edit", async () => {
    const user = userEvent.setup();
    setup();
    const memberButton = await screen.findByRole("button", { name: "Member" });
    memberButton.focus();
    await user.keyboard("{Enter}");
    await user.tab();
    expect(screen.getByLabelText("Role")).toHaveFocus();
    await user.tab();
    expect(screen.getByLabelText("Disabled")).toHaveFocus();
    await user.keyboard(" ");
    await user.tab();
    expect(screen.getByLabelText("Repository A access")).toHaveFocus();
    await user.selectOptions(screen.getByLabelText("Repository A access"), "ro");
    await user.tab();
    await user.tab();
    expect(screen.getByRole("button", { name: "Save access" })).toHaveFocus();
    let finish!: (account: ConsoleAccessAccount) => void;
    vi.mocked(updateAccess).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Saving access…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Reload accounts" })).toBeDisabled();
    expect(memberButton).toBeDisabled();
    expect(screen.getByLabelText("Role")).toBeDisabled();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    expect(screen.getByLabelText("Repository A access")).toBeDisabled();
    expect(updateAccess).toHaveBeenCalledWith(member.id, { edit_revision: 4, role: "member", disabled: true, grants: [{ repository_id: "rp_a", mode: "ro" }] });
    await act(async () => finish({ ...member, edit_revision: 5, disabled: true, grants: [{ repository_id: "rp_a", mode: "ro" }] }));
    expect(screen.getByText("Access saved. The account must sign in again.")).toBeVisible();
    expect(screen.getByRole("button", { name: "Save access" })).toBeEnabled();
  });

  it("handles empty directories and a selected member without repositories", async () => {
    vi.mocked(listAccounts).mockResolvedValue([]);
    setup(session, []);
    expect(await screen.findByText("No accounts are available.")).toBeVisible();
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    vi.mocked(listAccounts).mockResolvedValue([member]);
    fireEvent.click(screen.getByRole("button", { name: "Reload accounts" }));
    const group = await screen.findByRole("group", { name: "Repositories" });
    expect(within(group).getByText("None yet.")).toBeVisible();
  });

  it.each([new ApiError(401), new ApiError(403), new ApiError(500), new Error("offline")])("handles account-list failures without leaving records editable", async (error) => {
    vi.mocked(listAccounts).mockRejectedValue(error);
    const { onSignedOut } = setup();
    const message = error instanceof ApiError && error.status === 403 ? "Operator access is required." : "Access assignments could not be loaded.";
    expect(await screen.findByRole("alert")).toHaveTextContent(message);
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    expect(onSignedOut).toHaveBeenCalledTimes(error instanceof ApiError && error.status === 401 ? 1 : 0);
    vi.mocked(listAccounts).mockResolvedValue([operator, member]);
    fireEvent.click(screen.getByRole("button", { name: "Reload accounts" }));
    expect(await screen.findByLabelText("Role")).toBeVisible();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it.each([new ApiError(401), new ApiError(403), new ApiError(500), new Error("offline")])("handles access-write failures with appropriate session and permission feedback", async (error) => {
    const { onSignedOut } = setup();
    await chooseMember();
    vi.mocked(updateAccess).mockRejectedValue(error);
    submit();
    if (error instanceof ApiError && error.status === 401) {
      await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
    } else {
      expect(await screen.findByRole("alert")).toHaveTextContent(error instanceof ApiError && error.status === 403 ? "Operator access is required." : "Access could not be saved.");
      expect(onSignedOut).not.toHaveBeenCalled();
    }
    if (error instanceof ApiError && error.status === 403) {
      expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    } else {
      expect(screen.getByLabelText("Role")).toBeEnabled();
    }
  });

  it("discards editor state when a conflict refresh fails and recovers only from a fresh list", async () => {
    setup();
    await chooseMember();
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockRejectedValue(new Error("offline"));
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Access assignments could not be loaded.");
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    expect(screen.queryByText("Access changed or the last enabled operator would be removed. Review current accounts before trying again.")).not.toBeInTheDocument();
    vi.mocked(listAccounts).mockResolvedValue([operator]);
    fireEvent.click(screen.getByRole("button", { name: "Reload accounts" }));
    expect(await screen.findByLabelText("Role")).toHaveValue("operator");
  });

  it.each(["resolve", "reject"])("ignores an account-list %s after its panel unmounts", async (outcome) => {
    let resolve!: (accounts: ConsoleAccessAccount[]) => void;
    let reject!: (reason: unknown) => void;
    vi.mocked(listAccounts).mockImplementation(() => new Promise((accept, refuse) => { resolve = accept; reject = refuse; }));
    const { unmount, onSignedOut } = setup();
    expect(screen.getByText("Loading access assignments…")).toBeVisible();
    expect(screen.getByRole("button", { name: "Reload accounts" })).toBeDisabled();
    const signal = vi.mocked(listAccounts).mock.calls[0]![0]!;
    unmount();
    expect(signal.aborted).toBe(true);
    await act(async () => {
      if (outcome === "resolve") resolve([operator, member]);
      else reject(new ApiError(401));
    });
    expect(screen.queryByText("Member")).not.toBeInTheDocument();
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it.each(["resolve", "reject"])("ignores an access-write %s after a different account replaces its view", async (outcome) => {
    let resolve!: (account: ConsoleAccessAccount) => void;
    let reject!: (reason: unknown) => void;
    vi.mocked(updateAccess).mockImplementation(() => new Promise((accept, refuse) => { resolve = accept; reject = refuse; }));
    const { unmount, onSignedOut } = setup();
    await screen.findByLabelText("Role");
    submit();
    unmount();
    setup({ ...session, account: member });
    await act(async () => {
      if (outcome === "resolve") resolve({ ...operator, edit_revision: 2 });
      else reject(new ApiError(401));
    });
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Operator access is required.");
    expect(screen.queryByText("Access saved. The account must sign in again.")).not.toBeInTheDocument();
  });
});

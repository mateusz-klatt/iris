import { act, fireEvent, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, type Repository } from "./api";
import { getSession, listAccounts, updateAccess, type AccountSession, type ConsoleAccessAccount } from "./accountApi";
import AccessPanel from "./access";
import i18n from "./i18n";

vi.mock("./accountApi", async (original) => ({ ...await original<typeof import("./accountApi")>(), getSession: vi.fn(), listAccounts: vi.fn(), updateAccess: vi.fn() }));

const operator: ConsoleAccessAccount = { id: "ca_operator", username: "Operator", display_name: "Ops person", role: "operator", disabled: false, ui_locale: "en", correspondence_locale: "", edit_revision: 1, grants: [] };
const member: ConsoleAccessAccount = { ...operator, id: "ca_member", username: "Member", display_name: "", role: "member", edit_revision: 4 };
const otherOperator = { ...operator, id: "ca_other", username: "Other operator" };
const repositories: Repository[] = [{ id: "rp_a", name: "Repository A", aliases: [] }, { id: "rp_b", name: "Repository B", aliases: [] }];
const session: AccountSession = { account: operator, csrf: "synthetic-csrf", created_ms: 1, expires_ms: 2 };
const accessA = "Repository A · rp_a access";
const accessB = "Repository B · rp_b access";
const saved = "Access saved. The account must sign in again.";
const conflict = "Access changed or the last enabled operator would be removed. Review current accounts before trying again.";

beforeEach(async () => {
  vi.resetAllMocks();
  await i18n.changeLanguage("en");
  vi.mocked(listAccounts).mockResolvedValue([operator, member]);
  vi.mocked(getSession).mockResolvedValue(session);
});

function setup(accountSession = session, repos = repositories) {
  const onSignedOut = vi.fn();
  const view = render(<AccessPanel session={accountSession} repositories={repos} onSignedOut={onSignedOut} />);
  return { ...view, onSignedOut };
}

function person(name: RegExp) {
  return screen.getByRole("button", { name });
}

async function chooseMember() {
  fireEvent.click(await screen.findByRole("button", { name: /^Member/ }));
}

function choose(label: string, value: string) {
  fireEvent.change(screen.getByLabelText(label), { target: { value } });
}

// A click, not a submit event: a disabled Save must not reach the handler.
function submit() {
  fireEvent.click(screen.getByRole("button", { name: "Save access" }));
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
    expect(screen.queryByLabelText(accessA)).not.toBeInTheDocument();
  });

  it("leaves a disabled operator editable while one other operator is enabled", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...otherOperator, disabled: true }]);
    setup();
    fireEvent.click(await screen.findByRole("button", { name: /^Other operator/ }));
    expect(screen.getByLabelText("Role")).toBeEnabled();
    expect(screen.getByLabelText("Disabled")).toBeEnabled();
    expect(screen.queryByText("The last enabled operator cannot be disabled or demoted.")).not.toBeInTheDocument();
  });

  it("saves explicit repository grants with the selected account revision, including no grant", async () => {
    setup();
    await chooseMember();
    expect(screen.getByLabelText(accessA)).toHaveValue("");
    expect(screen.getByLabelText(accessB)).toHaveValue("");
    const updated = { ...member, edit_revision: 5, grants: [{ repository_id: "rp_a", mode: "rw" as const }] };
    vi.mocked(updateAccess).mockResolvedValue(updated);
    choose(accessA, "ro");
    choose(accessB, "rw");
    choose(accessA, "rw");
    choose(accessB, "");
    submit();
    await screen.findByText(saved);
    expect(updateAccess).toHaveBeenCalledWith(member.id, { edit_revision: 4, role: "member", disabled: false, grants: [{ repository_id: "rp_a", mode: "rw" }] });
    expect(screen.getByLabelText(accessA)).toHaveValue("rw");
    choose(accessA, "ro");
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, expect.objectContaining({ edit_revision: 5 })));
  });

  it("offers Save only after an edit, whatever order the grants were chosen in", async () => {
    const grants = [{ repository_id: "rp_a", mode: "ro" as const }, { repository_id: "rp_b", mode: "rw" as const }];
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, grants }]);
    setup();
    await chooseMember();
    const save = screen.getByRole("button", { name: "Save access" });
    expect(save).toBeDisabled();
    fireEvent.click(save);
    expect(updateAccess).not.toHaveBeenCalled();
    choose(accessA, "");
    expect(save).toBeEnabled();
    choose(accessA, "ro");
    expect(save).toBeDisabled();
    fireEvent.click(screen.getByLabelText("Disabled"));
    expect(save).toBeEnabled();
  });

  it("keeps grants while the role changes and sends them only for a member", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, grants: [{ repository_id: "rp_a", mode: "rw" }] }]);
    vi.mocked(updateAccess).mockResolvedValue({ ...member, role: "operator", edit_revision: 5 });
    setup();
    await chooseMember();
    choose("Role", "operator");
    expect(screen.queryByLabelText(accessA)).not.toBeInTheDocument();
    choose("Role", "member");
    expect(screen.getByLabelText(accessA)).toHaveValue("rw");
    expect(screen.getByRole("button", { name: "Save access" })).toBeDisabled();
    choose("Role", "operator");
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenCalledWith(member.id, { edit_revision: 4, role: "operator", disabled: false, grants: [] }));
  });

  it("labels every repository with its id, because names are not unique", async () => {
    const sameName: Repository[] = [{ id: "rp_1", name: "app", aliases: [] }, { id: "rp_2", name: "app", aliases: [] }, { id: "rp_3", name: "", aliases: [] }];
    setup(session, sameName);
    await chooseMember();
    expect(screen.getByLabelText("app · rp_1 access")).toBeVisible();
    expect(screen.getByLabelText("app · rp_2 access")).toBeVisible();
    expect(screen.getByLabelText("rp_3 access")).toBeVisible();
    expect(screen.getByText("app · rp_2")).toBeVisible();
    expect(screen.getByText("rp_3")).toBeVisible();
  });

  it("names a person by their role and state as well as their login", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, disabled: true }]);
    setup();
    expect(await screen.findByRole("button", { name: /^Member.*Member · Disabled/ })).toBeVisible();
    expect(person(/^Operator.*Ops person.*Operator · Active/)).toBeVisible();
  });

  it("loads existing grants and resets unsaved changes when selecting another account", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, grants: [{ repository_id: "rp_a", mode: "ro" }] }]);
    setup();
    await chooseMember();
    expect(screen.getByLabelText(accessA)).toHaveValue("ro");
    choose(accessA, "rw");
    fireEvent.click(person(/^Operator/));
    fireEvent.click(person(/^Member/));
    expect(screen.getByLabelText(accessA)).toHaveValue("ro");
    expect(person(/^Member/)).toHaveAttribute("aria-pressed", "true");
  });

  it("keeps disabled accounts editable and forces none disabled", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, disabled: true, grants: [{ repository_id: "rp_a", mode: "rw" }] }]);
    setup();
    await chooseMember();
    expect(screen.getByLabelText("Disabled")).toBeChecked();
    fireEvent.click(screen.getByLabelText("Disabled"));
    vi.mocked(updateAccess).mockResolvedValue({ ...member, role: "none", disabled: true, edit_revision: 5 });
    choose("Role", "none");
    expect(screen.getByLabelText("Disabled")).toBeChecked();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    submit();
    await screen.findByText(saved);
    expect(updateAccess).toHaveBeenLastCalledWith(member.id, { edit_revision: 4, role: "none", disabled: true, grants: [] });
    choose("Role", "operator");
    fireEvent.click(screen.getByLabelText("Disabled"));
    vi.mocked(updateAccess).mockResolvedValue({ ...member, role: "operator", edit_revision: 6 });
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, { edit_revision: 5, role: "operator", disabled: false, grants: [] }));
  });

  it("replaces the form with the hub's copy after a save", async () => {
    setup();
    await chooseMember();
    vi.mocked(updateAccess).mockResolvedValue({ ...member, edit_revision: 5, grants: [{ repository_id: "rp_b", mode: "ro" }] });
    choose(accessA, "rw");
    submit();
    await screen.findByText(saved);
    expect(screen.getByLabelText(accessA)).toHaveValue("");
    expect(screen.getByLabelText(accessB)).toHaveValue("ro");
  });

  it("refreshes conflicting permissions and requires a reviewed retry with the new revision", async () => {
    setup();
    await chooseMember();
    const latest = { ...member, edit_revision: 8, grants: [{ repository_id: "rp_a", mode: "ro" as const }] };
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockResolvedValue([operator, latest]);
    choose(accessB, "rw");
    submit();
    expect(await screen.findByText(conflict)).toBeVisible();
    expect(screen.getByLabelText(accessA)).toHaveValue("ro");
    expect(screen.getByLabelText(accessB)).toHaveValue("");
    vi.mocked(updateAccess).mockResolvedValue({ ...latest, edit_revision: 9 });
    choose(accessB, "rw");
    submit();
    await vi.waitFor(() => expect(updateAccess).toHaveBeenLastCalledWith(member.id, expect.objectContaining({ edit_revision: 8 })));
  });

  it("accepts a last-operator refusal after another operator disappears", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, otherOperator]);
    setup();
    expect(await screen.findByLabelText("Role")).toBeEnabled();
    choose("Role", "member");
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockResolvedValue([operator]);
    submit();
    await screen.findByText(conflict);
    expect(screen.getByLabelText("Role")).toBeDisabled();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    expect(screen.getByLabelText("Role")).toHaveValue("operator");
  });

  it("ends the current session after an access edit to self", async () => {
    vi.mocked(listAccounts).mockResolvedValue([operator, otherOperator]);
    const { onSignedOut } = setup();
    await screen.findByLabelText("Role");
    choose("Role", "member");
    vi.mocked(updateAccess).mockResolvedValue({ ...operator, role: "member", edit_revision: 2 });
    submit();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
  });

  it("supports keyboard selection, native controls, and submit while freezing a pending edit", async () => {
    const user = userEvent.setup();
    setup();
    const memberButton = await screen.findByRole("button", { name: /^Member/ });
    memberButton.focus();
    await user.keyboard("{Enter}");
    await user.tab();
    expect(screen.getByLabelText("Role")).toHaveFocus();
    await user.tab();
    expect(screen.getByLabelText("Disabled")).toHaveFocus();
    await user.keyboard(" ");
    await user.tab();
    expect(screen.getByLabelText(accessA)).toHaveFocus();
    await user.selectOptions(screen.getByLabelText(accessA), "ro");
    await user.tab();
    await user.tab();
    expect(screen.getByRole("button", { name: "Save access" })).toHaveFocus();
    let finish!: (account: ConsoleAccessAccount) => void;
    vi.mocked(updateAccess).mockImplementation(() => new Promise((resolve) => { finish = resolve; }));
    await user.keyboard("{Enter}");
    expect(screen.getByRole("button", { name: "Saving access…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Saving access…" })).toHaveAttribute("aria-busy", "true");
    expect(screen.getByRole("button", { name: "Reload accounts" })).toBeDisabled();
    expect(memberButton).toBeDisabled();
    expect(screen.getByLabelText("Role")).toBeDisabled();
    expect(screen.getByLabelText("Disabled")).toBeDisabled();
    expect(screen.getByLabelText(accessA)).toBeDisabled();
    expect(updateAccess).toHaveBeenCalledWith(member.id, { edit_revision: 4, role: "member", disabled: true, grants: [{ repository_id: "rp_a", mode: "ro" }] });
    await act(async () => finish({ ...member, edit_revision: 5, disabled: true, grants: [{ repository_id: "rp_a", mode: "ro" }] }));
    expect(screen.getByText(saved)).toBeVisible();
    expect(screen.getByRole("heading", { name: "Selected account: Member" })).toHaveFocus();
    expect(screen.getByLabelText("Role")).toBeEnabled();
  });

  it("brings focus to the form after each action that takes it away, and never on its own", async () => {
    const { rerender, onSignedOut } = setup();
    const heading = () => screen.getByRole("heading", { name: /^Selected account/ });
    const focused = () => vi.waitFor(() => expect(heading()).toHaveFocus());
    await screen.findByLabelText("Role");
    expect(document.body).toHaveFocus();

    fireEvent.click(person(/^Member/));
    await focused();
    // Selecting the person already shown changes no state, and must not leave a focus move due later.
    screen.getByLabelText("Role").focus();
    fireEvent.click(person(/^Member/));
    await focused();
    screen.getByLabelText("Role").focus();
    rerender(<AccessPanel session={session} repositories={[...repositories]} onSignedOut={onSignedOut} />);
    expect(screen.getByLabelText("Role")).toHaveFocus();

    vi.mocked(updateAccess).mockResolvedValueOnce({ ...member, edit_revision: 5, grants: [{ repository_id: "rp_a", mode: "ro" }] });
    choose(accessA, "ro");
    submit();
    await screen.findByText(saved);
    await focused();

    vi.mocked(updateAccess).mockRejectedValueOnce(new Error("offline"));
    choose(accessA, "rw");
    submit();
    await screen.findByRole("alert");
    await focused();

    vi.mocked(updateAccess).mockRejectedValueOnce(new ApiError(409));
    vi.mocked(listAccounts).mockResolvedValue([operator, { ...member, edit_revision: 6 }]);
    submit();
    await screen.findByText(conflict);
    await focused();

    screen.getByLabelText("Role").focus();
    fireEvent.click(screen.getByRole("button", { name: "Reload accounts" }));
    await screen.findByLabelText("Role");
    await focused();
  });

  it("clears an old status or failure when selecting a person or reloading", async () => {
    setup();
    await chooseMember();
    vi.mocked(updateAccess).mockResolvedValueOnce({ ...member, edit_revision: 5, grants: [{ repository_id: "rp_a", mode: "ro" }] });
    choose(accessA, "ro");
    submit();
    await screen.findByText(saved);
    fireEvent.click(person(/^Operator/));
    expect(screen.queryByText(saved)).not.toBeInTheDocument();

    fireEvent.click(person(/^Member/));
    vi.mocked(updateAccess).mockRejectedValueOnce(new Error("offline"));
    choose(accessA, "rw");
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Access could not be saved.");
    fireEvent.click(person(/^Operator/));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    fireEvent.click(person(/^Member/));
    vi.mocked(updateAccess).mockResolvedValueOnce({ ...member, edit_revision: 6, grants: [{ repository_id: "rp_a", mode: "rw" }] });
    choose(accessA, "rw");
    submit();
    await screen.findByText(saved);
    fireEvent.click(screen.getByRole("button", { name: "Reload accounts" }));
    await screen.findByLabelText("Role");
    expect(screen.queryByText(saved)).not.toBeInTheDocument();
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
    choose(accessA, "ro");
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

  it("signs out instead of claiming missing access when another tab replaced the session", async () => {
    const { onSignedOut } = setup();
    await chooseMember();
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(403));
    vi.mocked(getSession).mockRejectedValue(new ApiError(401));
    choose(accessA, "ro");
    submit();
    await vi.waitFor(() => expect(onSignedOut).toHaveBeenCalledOnce());
    expect(getSession).toHaveBeenCalledWith(undefined, session);
    expect(screen.queryByText("Operator access is required.")).not.toBeInTheDocument();
  });

  it("keeps the refusal when the session cannot be checked", async () => {
    const { onSignedOut } = setup();
    await chooseMember();
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(403));
    vi.mocked(getSession).mockRejectedValue(new ApiError(500));
    choose(accessA, "ro");
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Operator access is required.");
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it("ignores the session check after its panel unmounts", async () => {
    let reject!: (reason: unknown) => void;
    vi.mocked(getSession).mockImplementation(() => new Promise((_accept, refuse) => { reject = refuse; }));
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(403));
    const { unmount, onSignedOut } = setup();
    await chooseMember();
    choose(accessA, "ro");
    submit();
    await vi.waitFor(() => expect(getSession).toHaveBeenCalledOnce());
    unmount();
    await act(async () => reject(new ApiError(401)));
    expect(onSignedOut).not.toHaveBeenCalled();
  });

  it("discards editor state when a conflict refresh fails and recovers only from a fresh list", async () => {
    setup();
    await chooseMember();
    vi.mocked(updateAccess).mockRejectedValue(new ApiError(409));
    vi.mocked(listAccounts).mockRejectedValue(new Error("offline"));
    choose(accessA, "ro");
    submit();
    expect(await screen.findByRole("alert")).toHaveTextContent("Access assignments could not be loaded.");
    expect(screen.queryByLabelText("Role")).not.toBeInTheDocument();
    expect(screen.queryByText(conflict)).not.toBeInTheDocument();
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
    vi.mocked(listAccounts).mockResolvedValue([operator, otherOperator]);
    const { unmount, onSignedOut } = setup();
    await screen.findByLabelText("Role");
    choose("Role", "member");
    submit();
    unmount();
    setup({ ...session, account: member });
    await act(async () => {
      if (outcome === "resolve") resolve({ ...operator, edit_revision: 2 });
      else reject(new ApiError(401));
    });
    expect(onSignedOut).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("Operator access is required.");
    expect(screen.queryByText(saved)).not.toBeInTheDocument();
  });
});

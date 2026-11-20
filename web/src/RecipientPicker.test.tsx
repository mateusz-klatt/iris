import { useState } from "react";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { RecipientPicker } from "./RecipientPicker";
import { sessionOverview } from "./test/server";
import "./i18n";

describe("recipient selection", () => {
  it("searches names and hosts without losing a selection hidden by search", async () => {
    function Form() {
      const [selected, setSelected] = useState<string[]>([]);
      return <RecipientPicker agents={sessionOverview.agents} selected={selected} onChange={setSelected} />;
    }
    const user = userEvent.setup();
    render(<Form />);
    await user.click(screen.getByRole("checkbox", { name: /claude-host/ }));
    const search = screen.getByRole("searchbox", { name: "Find recipients" });
    await user.type(search, " HUMAN ");
    expect(screen.queryByRole("checkbox", { name: /claude-host/ })).toBeNull();
    await user.click(screen.getByRole("checkbox", { name: /^human/ }));
    await user.clear(search);
    expect(screen.getByRole("checkbox", { name: /claude-host/ })).toBeChecked();
    await user.click(screen.getByRole("checkbox", { name: /claude-host/ }));
    expect(screen.getByRole("checkbox", { name: /claude-host/ })).not.toBeChecked();
    await user.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(screen.getByRole("checkbox", { name: /^human/ })).not.toBeChecked();
    await user.type(search, "missing");
    expect(screen.getByText("No active agents match this search.")).toBeVisible();
    await user.clear(search);
    await user.type(search, "local");
    expect(screen.getByRole("checkbox", { name: /claude-host/ })).toBeVisible();
  });

  it("lists inactive agents only on request, and keeps a chosen one listed", async () => {
    const ended = { ...sessionOverview.agents[0]!, id: "ag_ended", name: "codex-old-worktree", state: "ended" };
    const lost = { ...sessionOverview.agents[0]!, id: "ag_lost", name: "kimi-lost", state: "lost" };
    function Form({ initial }: { initial: string[] }) {
      const [selected, setSelected] = useState<string[]>(initial);
      return <RecipientPicker agents={[...sessionOverview.agents, ended, lost]} selected={selected} onChange={setSelected} />;
    }
    const user = userEvent.setup();
    const { unmount } = render(<Form initial={[]} />);
    expect(screen.getByRole("checkbox", { name: /claude-host/ })).toBeVisible();
    expect(screen.queryByRole("checkbox", { name: /codex-old-worktree/ })).toBeNull();
    expect(screen.queryByRole("checkbox", { name: /kimi-lost/ })).toBeNull();
    await user.click(screen.getByRole("checkbox", { name: "Include inactive agents" }));
    await user.click(screen.getByRole("checkbox", { name: /codex-old-worktree/ }));
    expect(screen.getByRole("checkbox", { name: /kimi-lost/ })).toBeVisible();
    await user.click(screen.getByRole("checkbox", { name: "Include inactive agents" }));
    expect(screen.getByRole("checkbox", { name: /codex-old-worktree/ })).toBeChecked();
    expect(screen.queryByRole("checkbox", { name: /kimi-lost/ })).toBeNull();
    unmount();
    render(<Form initial={["ag_lost"]} />);
    expect(screen.getByRole("checkbox", { name: /kimi-lost/ })).toBeChecked();
    expect(screen.queryByRole("alert")).toBeNull();
  });

  it("does not silently select an agent recreated under the same name", () => {
    const changed = vi.fn();
    const { rerender } = render(<RecipientPicker agents={sessionOverview.agents} selected={["ag_claude"]} onChange={changed} />);
    rerender(<RecipientPicker agents={[{ ...sessionOverview.agents[0]!, id: "ag_recreated" }]} selected={["ag_claude"]} onChange={changed} />);
    expect(screen.getByRole("alert")).toHaveTextContent("Confirm the recipient selection");
    expect(screen.getByRole("checkbox", { name: /claude-host/ })).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(changed).toHaveBeenLastCalledWith([]);
    fireEvent.click(screen.getByRole("checkbox", { name: /claude-host/ }));
    expect(changed).toHaveBeenLastCalledWith(["ag_claude", "ag_recreated"]);
  });
});

import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Claim, Repository } from "./api";
import { ClaimScopeLine, ClaimsPanel } from "./ClaimsPanel";
import "./i18n";
import { timeLeft } from "./timeLeft";

const now = Date.parse("2026-11-08T10:00:00Z");
const repositories: Repository[] = [
  { id: "rp_iris", name: "iris", aliases: [] },
  { id: "rp_sample", name: "sample", aliases: [] },
];

function claim(fence: number, path: string, holder: string, repository_id: string, expires_at: string | null): Claim {
  return { fence, path, holder, repository_id, kind: "exact", origin: "auto", expires_at };
}

const claims = [
  claim(1, "docs/api.md", "claude-laptop-iris", "rp_iris", "2026-11-08T10:00:45Z"),
  claim(2, "internal/store/store.go", "codex-home-iris", "rp_iris", "2026-11-08T10:04:30Z"),
  claim(3, "src/sample/data/repository.py", "codex-home-sample", "rp_sample", "2026-11-08T12:30:00Z"),
  claim(4, "README.md", "claude-laptop-iris", "rp_sample", null),
];

function rows(): string[] {
  return screen.queryAllByRole("listitem").map((item) => item.querySelector("strong")?.textContent ?? "");
}

function show(list: Claim[] = claims, onRevoke = vi.fn(), revoking: number | null = null) {
  return render(<ClaimsPanel claims={list} repositories={repositories} revoking={revoking} onRevoke={onRevoke} />);
}

afterEach(() => {
  vi.useRealTimers();
});

describe("timeLeft", () => {
  it("counts seconds in the last minute, then minutes, then hours, in the given language", () => {
    expect(timeLeft("2026-11-08T10:00:45Z", now, "en")).toBe("in 45 seconds");
    expect(timeLeft("2026-11-08T10:00:00.400Z", now, "en")).toBe("in 1 second");
    expect(timeLeft("2026-11-08T10:04:30Z", now, "en")).toBe("in 4 minutes");
    expect(timeLeft("2026-11-08T12:30:00Z", now, "en")).toBe("in 2 hours");
    expect(timeLeft("2026-11-08T10:04:30Z", now, "de")).toBe("in 4 Minuten");
  });

  it("says a claim at or past its expiry has expired, and cannot read what is not a time", () => {
    expect(timeLeft("2026-11-08T10:00:00Z", now, "en")).toBeNull();
    expect(timeLeft("2026-11-08T09:00:00Z", now, "en")).toBeNull();
    expect(timeLeft("soon", now, "en")).toBeUndefined();
  });
});

describe("ClaimsPanel", () => {
  it("shows each claim with its repository, holder, kind and time left, and the absolute time on hover", () => {
    vi.useFakeTimers({ now, toFake: ["Date", "setInterval", "clearInterval"] });
    show();
    const first = screen.getAllByRole("listitem")[0]!;
    expect(first).toHaveTextContent("iris · claude-laptop-iris · exact");
    const left = within(first).getByText("in 45 seconds");
    expect(left.tagName).toBe("TIME");
    expect(left).toHaveAttribute("dateTime", "2026-11-08T10:00:45Z");
    expect(left.getAttribute("title")).toMatch(/2026/);
    expect(screen.getAllByRole("listitem")[3]).toHaveTextContent("None yet.");
  });

  it("counts down every second and says when a claim has expired", () => {
    vi.useFakeTimers({ now, toFake: ["Date", "setInterval", "clearInterval"] });
    show();
    expect(screen.getByText("in 45 seconds")).toBeVisible();
    act(() => {
      vi.advanceTimersByTime(5000);
    });
    expect(screen.getByText("in 40 seconds")).toBeVisible();
    act(() => {
      vi.advanceTimersByTime(40000);
    });
    const expired = screen.getByText("Expired");
    expect(expired).toHaveAttribute("data-expired", "yes");
  });

  it("runs no timer when no claim expires, and stops its timer when it goes", () => {
    vi.useFakeTimers({ now, toFake: ["Date", "setInterval", "clearInterval"] });
    const quiet = show([claims[3]!]);
    expect(vi.getTimerCount()).toBe(0);
    quiet.unmount();
    const busy = show();
    expect(vi.getTimerCount()).toBe(1);
    busy.unmount();
    expect(vi.getTimerCount()).toBe(0);
  });

  it("filters by repository, holder and path, together", async () => {
    const user = userEvent.setup();
    show();
    expect(rows()).toHaveLength(4);

    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "rp_iris");
    expect(rows()).toEqual(["docs/api.md", "internal/store/store.go"]);

    await user.selectOptions(screen.getByRole("combobox", { name: "Held by" }), "claude-laptop-iris");
    expect(rows()).toEqual(["docs/api.md"]);

    await user.selectOptions(screen.getByRole("combobox", { name: "Repository" }), "");
    expect(rows()).toEqual(["docs/api.md", "README.md"]);

    await user.type(screen.getByRole("searchbox", { name: "Path" }), "  README ");
    expect(rows()).toEqual(["README.md"]);

    await user.clear(screen.getByRole("searchbox", { name: "Path" }));
    await user.type(screen.getByRole("searchbox", { name: "Path" }), "nothing-here");
    expect(rows()).toEqual([]);
    expect(screen.getByRole("status")).toHaveTextContent("No claim matches these filters.");
  });

  it("offers each repository and holder once, by name, and names an unknown repository by its id", () => {
    show([...claims, claim(5, "x/y", "codex-home-iris", "rp_gone", null)]);
    const repository = screen.getByRole("combobox", { name: "Repository" });
    expect(within(repository).getAllByRole("option").map((option) => option.textContent)).toEqual(["All", "iris", "rp_gone", "sample"]);
    const holder = screen.getByRole("combobox", { name: "Held by" });
    expect(within(holder).getAllByRole("option").map((option) => option.textContent)).toEqual(["All", "claude-laptop-iris", "codex-home-iris", "codex-home-sample"]);
    expect(screen.getByText("x/y").closest("li")).toHaveTextContent("rp_gone · codex-home-iris");
  });

  it("keeps a chosen holder after their last claim ends, and shows that nothing matches", async () => {
    const user = userEvent.setup();
    const view = show();
    await user.selectOptions(screen.getByRole("combobox", { name: "Held by" }), "codex-home-sample");
    view.rerender(<ClaimsPanel claims={claims.slice(0, 2)} repositories={repositories} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.getByRole("combobox", { name: "Held by" })).toHaveValue("codex-home-sample");
    expect(screen.getByRole("status")).toHaveTextContent("No claim matches these filters.");
  });

  it("revokes the claim on its row, and shows the one being revoked", async () => {
    const user = userEvent.setup();
    const onRevoke = vi.fn();
    const view = show(claims, onRevoke);
    const row = screen.getByText("internal/store/store.go").closest("li")!;
    await user.click(within(row).getByRole("button", { name: "Revoke" }));
    expect(onRevoke).toHaveBeenCalledWith(claims[1]);
    view.rerender(<ClaimsPanel claims={claims} repositories={repositories} revoking={2} onRevoke={onRevoke} />);
    expect(within(row).getByRole("button", { name: "Revoking…" })).toBeDisabled();
  });

  it("says there are none, without filters, when nothing is claimed", () => {
    show([]);
    expect(screen.getByText("None yet.")).toBeVisible();
    expect(screen.queryByRole("group")).toBeNull();
  });

  it("shows each project's automatic claim scope and leaves a host context out", () => {
    const repos: Repository[] = [
      { id: "rp_iris", name: "iris", aliases: [], claim_auto_scope: "repository" },
      { id: "rp_sample", name: "sample", aliases: [], claim_auto_scope: "worktree" },
      { id: "rp_old", name: "", aliases: [] },
      { id: "rp_outside", name: "outside git on home", aliases: ["outside:home"], kind: "outside", host: "home" },
    ];
    render(<ClaimsPanel claims={[]} repositories={repos} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.getByText("iris: Repository-wide")).toBeVisible();
    expect(screen.getByText(/sample: Worktree-scoped/)).toBeVisible();
    expect(screen.getByText(/Overlapping automatic claims in different worktrees are advisory/)).toBeVisible();
    expect(screen.getByText("rp_old: Unknown")).toBeVisible();
    expect(screen.queryByText(/outside git on home/)).toBeNull();
    expect(screen.getByText("iris: Repository-wide").closest("p")?.querySelector("small")).toBeNull();
  });

  it("shows a claim's own checkout, and unknown for a legacy row with no path", () => {
    const repos: Repository[] = [
      { id: "rp_iris", name: "iris", aliases: [], claim_auto_scope: "worktree" },
    ];
    const rows: Claim[] = [
      { ...claim(1, "src/api.go", "codex-a", "rp_iris", null), worktree_path: "/wt", branch: "delegate/api", host: "host-a", effective_scope: "worktree" },
      { ...claim(2, "src/api.go", "codex-b", "rp_iris", null), worktree_path: "/wt", branch: "delegate/web", host: "host-b", effective_scope: "worktree" },
      { ...claim(3, "README.md", "claude", "rp_iris", null), origin: "explicit", worktree_path: "/wt-explicit", branch: "main", host: "host-a", effective_scope: "repository" },
      { ...claim(4, "old.go", "claude", "rp_iris", null), worktree_path: "", branch: "", effective_scope: "repository" },
    ];
    const view = render(<ClaimsPanel claims={rows} repositories={repos} revoking={null} onRevoke={vi.fn()} />);
    const row = (fence: string) => view.container.querySelector(`[data-fence="${fence}"]`);
    expect(row("1")).toHaveTextContent("Worktree /wt on host-a · Branch delegate/api · Applies in this worktree");
    expect(row("2")).toHaveTextContent("Worktree /wt on host-b · Branch delegate/web · Applies in this worktree");
    expect(row("3")).toHaveTextContent("Worktree /wt-explicit · Branch main · Applies across the repository");
    expect(row("3")).not.toHaveTextContent("Applies in this worktree");
    expect(row("4")).toHaveTextContent("Location unknown · Applies across the repository");
  });

  it("replaces the checkout and the scope when the claims change", () => {
    const repos: Repository[] = [{ id: "rp_iris", name: "iris", aliases: [], claim_auto_scope: "repository" }];
    const first = { ...claim(1, "src/api.go", "codex-a", "rp_iris", null), worktree_path: "/wt1", branch: "main", effective_scope: "repository" };
    const view = render(<ClaimsPanel claims={[first]} repositories={repos} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.getByText("src/api.go").closest("li")).toHaveTextContent("Worktree /wt1 · Branch main · Applies across the repository");
    const next = { ...first, worktree_path: "/wt2", branch: "delegate/api", effective_scope: "worktree" };
    view.rerender(<ClaimsPanel claims={[next]} repositories={[{ ...repos[0]!, claim_auto_scope: "worktree" }]} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.getByText("src/api.go").closest("li")).toHaveTextContent("Worktree /wt2 · Branch delegate/api · Applies in this worktree");
    expect(screen.getByText(/iris: Worktree-scoped/)).toBeVisible();
  });

  it("names a scope with no repository when the line has no name, and hides the list when there is no project", () => {
    render(<ClaimScopeLine name="" scope="repository" />);
    expect(screen.getByText("Repository-wide")).toBeVisible();
    expect(screen.queryByText(/: Repository-wide/)).toBeNull();
    render(<ClaimsPanel claims={[]} repositories={[]} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.queryByLabelText("Automatic claim scope")).toBeNull();
  });
});

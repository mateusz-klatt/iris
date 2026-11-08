import { act, render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { Claim, Repository } from "./api";
import { ClaimsPanel } from "./ClaimsPanel";
import "./i18n";
import { timeLeft } from "./timeLeft";

const now = Date.parse("2026-11-08T10:00:00Z");
const repositories: Repository[] = [
  { id: "rp_iris", name: "iris", aliases: [] },
  { id: "rp_snapper", name: "snapper", aliases: [] },
];

function claim(fence: number, path: string, holder: string, repository_id: string, expires_at: string | null): Claim {
  return { fence, path, holder, repository_id, kind: "exact", origin: "auto", expires_at };
}

const claims = [
  claim(1, "docs/api.md", "claude-laptop-iris", "rp_iris", "2026-11-08T10:00:45Z"),
  claim(2, "internal/store/store.go", "codex-home-iris", "rp_iris", "2026-11-08T10:04:30Z"),
  claim(3, "src/snapper/data/repository.py", "codex-home-snapper", "rp_snapper", "2026-11-08T12:30:00Z"),
  claim(4, "README.md", "claude-laptop-iris", "rp_snapper", null),
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
    expect(within(repository).getAllByRole("option").map((option) => option.textContent)).toEqual(["All", "iris", "rp_gone", "snapper"]);
    const holder = screen.getByRole("combobox", { name: "Held by" });
    expect(within(holder).getAllByRole("option").map((option) => option.textContent)).toEqual(["All", "claude-laptop-iris", "codex-home-iris", "codex-home-snapper"]);
    expect(screen.getByText("x/y").closest("li")).toHaveTextContent("rp_gone · codex-home-iris");
  });

  it("keeps a chosen holder after their last claim ends, and shows that nothing matches", async () => {
    const user = userEvent.setup();
    const view = show();
    await user.selectOptions(screen.getByRole("combobox", { name: "Held by" }), "codex-home-snapper");
    view.rerender(<ClaimsPanel claims={claims.slice(0, 2)} repositories={repositories} revoking={null} onRevoke={vi.fn()} />);
    expect(screen.getByRole("combobox", { name: "Held by" })).toHaveValue("codex-home-snapper");
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
});

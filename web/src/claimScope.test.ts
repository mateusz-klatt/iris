import { describe, expect, it } from "vitest";

import type { Claim } from "./api";
import { claimAutoScopeText, claimEffectiveText, claimLocation } from "./claimScope";
import i18n from "./i18n";

const t = i18n.t.bind(i18n);

function claim(path: string, host = "", branch = ""): Claim {
  return {
    fence: path.length + host.length,
    repository_id: "rp",
    path: "a.go",
    kind: "exact",
    holder: host || "holder",
    origin: "auto",
    expires_at: null,
    worktree_path: path,
    branch,
    host,
  };
}

describe("claim scope text", () => {
  it("names the two settings and calls anything else unknown", () => {
    expect(claimAutoScopeText("repository", t)).toBe("Repository-wide");
    expect(claimAutoScopeText("worktree", t)).toBe("Worktree-scoped");
    expect(claimAutoScopeText(undefined, t)).toBe("Unknown");
    expect(claimAutoScopeText("seat", t)).toBe("Unknown");
    expect(claimEffectiveText("worktree", t)).toBe("Applies in this worktree");
    expect(claimEffectiveText("repository", t)).toBe("Applies across the repository");
    expect(claimEffectiveText(undefined, t)).toBe("Unknown");
  });

  it("names a recorded checkout, and says unknown when the claim has no display path", () => {
    const alone = claim("/wt1", "host-a", "main");
    expect(claimLocation(alone, [alone], t)).toBe("Worktree /wt1 · Branch main");
    expect(claimLocation(claim("/wt1", "host-a", ""), [claim("/wt1", "host-a", "")], t)).toBe("Worktree /wt1 · Branch unknown");
    expect(claimLocation(claim("", "host-a", "main"), [claim("", "host-a", "main")], t)).toBe("Location unknown");
    const legacy: Claim = {
      fence: 9,
      repository_id: "rp",
      path: "old.go",
      kind: "exact",
      holder: "holder",
      origin: "auto",
      expires_at: null,
    };
    const noBranch: Claim = { ...alone, branch: undefined };
    expect(claimLocation(noBranch, [noBranch, legacy], t)).toBe("Worktree /wt1 · Branch unknown");
  });

  it("adds the host only when another claim records the same path", () => {
    const first = claim("/same", "host-a", "one");
    const second = claim("/same", "host-b", "two");
    const other = claim("/other", "host-c", "three");
    expect(claimLocation(first, [first, second, other], t)).toBe("Worktree /same on host-a · Branch one");
    expect(claimLocation(second, [first, second], t)).toBe("Worktree /same on host-b · Branch two");
    expect(claimLocation(other, [first, other], t)).toBe("Worktree /other · Branch three");
    const nameless = claim("/same", "", "one");
    expect(claimLocation(nameless, [nameless, second], t)).toBe("Worktree /same · Branch one");
  });
});

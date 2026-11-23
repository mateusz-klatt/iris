import type { TFunction } from "i18next";

import type { Claim } from "./api";

// claim.auto_scope as the console says it. Anything other than the two settings
// is unknown: a missing field is a hub from before ADR 037, not a guess.
export function claimAutoScopeText(scope: string | undefined, t: TFunction): string {
  switch (scope) {
    case "repository":
      return t("operator.claimAutoScopeRepository");
    case "worktree":
      return t("operator.claimAutoScopeWorktree");
    default:
      return t("operator.claimAutoScopeUnknown");
  }
}

// Effective behaviour of one claim. The hub decides it. A missing value is unknown,
// so a null key is never shown as worktree-scoped from the holder's checkout alone.
export function claimEffectiveText(scope: string | undefined, t: TFunction): string {
  switch (scope) {
    case "worktree":
      return t("operator.claimEffectiveWorktree");
    case "repository":
      return t("operator.claimEffectiveRepository");
    default:
      return t("operator.claimAutoScopeUnknown");
  }
}

// The claim's own display checkout. An empty path is unknown, including a legacy
// row whose holder names some other worktree. The host is added only when another
// claim records the same path.
export function claimLocation(claim: Claim, claims: Claim[], t: TFunction): string {
  const path = claim.worktree_path ?? "";
  if (path === "") {
    return t("operator.claimLocationUnknown");
  }
  const shared = claims.filter((other) => (other.worktree_path ?? "") === path).length > 1;
  const host = claim.host ?? "";
  const where = shared && host !== "" ? ` ${t("operator.claimOnHost", { host })}` : "";
  const branch = (claim.branch ?? "") !== ""
    ? t("operator.claimBranch", { branch: claim.branch })
    : t("operator.claimBranchUnknown");
  return `${t("operator.claimWorktree", { path })}${where} · ${branch}`;
}

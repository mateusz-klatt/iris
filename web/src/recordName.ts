import type { TFunction } from "i18next";

import type { Repository } from "./api";

/**
 * The name a person reads for a record. A host's context of sessions outside git has no name of
 * its own, so it is named for its host (ADR 034).
 */
export function recordName(repository: Repository, t: TFunction): string {
  return repository.kind === "outside" ? t("console.outsideGit", { host: repository.host }) : repository.name;
}

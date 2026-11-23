import type { TFunction } from "i18next";

const labelKeys = {
  claim: {
    exact: "operator.claimKindExact",
    prefix: "operator.claimKindPrefix",
  },
  session: {
    active: "operator.sessionStateActive",
    lost: "operator.sessionStateLost",
    ended: "operator.sessionStateEnded",
  },
  delivery: {
    pending: "operator.deliveryStatePending",
    inflight: "operator.deliveryStateInflight",
    acked: "operator.deliveryStateAcked",
    handled: "operator.deliveryStateHandled",
  },
} as const;

// Translate known protocol values without interpreting arbitrary hub data as keys.
// A newer hub's unknown values remain visible for diagnosis.
export function consoleLabel(kind: keyof typeof labelKeys, value: string, t: TFunction): string {
  const keys: Readonly<Record<string, string>> = labelKeys[kind];
  return Object.hasOwn(keys, value) ? t(keys[value]!) : value;
}

// Subjects cross account languages. Recognize every catalog's reply prefix without
// loading all 44 catalogs into the initial bundle. The catalog test checks every pair.
const replyPrefix = /^(?:(?:Re\s*|Sv|Απ|رد|پاسخ|Odp|Ynt):|(?:回复|回覆)：)/u;

export function replySubject(subject: string, t: TFunction): string {
  return replyPrefix.test(subject)
    ? subject
    : t("operator.replySubject", { subject });
}

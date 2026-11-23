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
// loading all 44 catalogs into the initial bundle. Match each catalog's exact
// delimiter: a colon and space, or the Chinese fullwidth colon without a space.
// French adds a nonbreaking space before its colon. The tests check every pair.
const replyPrefix = /^(?:(?:Re\u00a0?|Sv|Απ|رد|پاسخ|Odp|Ynt): |(?:回复|回覆)：)/iu;
const encoder = new TextEncoder();
const graphemes = new Intl.Segmenter(undefined, { granularity: "grapheme" });

// Only message metadata establishes that the source is a reply. A root subject
// can legitimately start with text such as "Odp: parser test plan".
export function replySubject(subject: string, t: TFunction, originalIsReply: boolean, maximumBytes: number): string {
  const prefix = originalIsReply && replyPrefix.test(subject)
    ? ""
    : t("operator.replySubject", { subject: "" });
  const available = maximumBytes - encoder.encode(prefix).byteLength;
  let truncated = "";
  let bytes = 0;
  // Keep whole user-perceived characters, including combining marks and joined
  // emoji, while reserving enough UTF-8 bytes for the localized prefix.
  for (const { segment } of graphemes.segment(subject)) {
    const size = encoder.encode(segment).byteLength;
    if (bytes + size > available) {
      break;
    }
    truncated += segment;
    bytes += size;
  }
  return prefix + truncated;
}

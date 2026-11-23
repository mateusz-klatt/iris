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
// loading all 44 catalogs into the initial bundle. Older replies may omit the
// space after the colon. French adds a nonbreaking space before its colon, and
// Chinese uses a fullwidth colon. The tests check every catalog pair.
const replyPrefix = /^(?:(?:Re\u00a0?|Sv|Απ|رد|پاسخ|Odp|Ynt):|(?:回复|回覆)：)/iu;
const encoder = new TextEncoder();

// Only message metadata establishes that the source is a reply. A root subject
// can legitimately start with text such as "Odp: parser test plan".
export function replySubject(subject: string, t: TFunction, originalIsReply: boolean, maximumBytes: number): string {
  const prefix = originalIsReply && replyPrefix.test(subject)
    ? ""
    : t("operator.replySubject", { subject: "" });
  const available = maximumBytes - encoder.encode(prefix).byteLength;
  let truncated = "";
  let bytes = 0;
  // Keep whole graphemes when supported. Older browsers fall back to whole code
  // points: this preserves surrogate pairs, but may separate combining marks or
  // joined emoji. Construct Segmenter only after checking support, not at startup.
  const segments = typeof Intl.Segmenter === "function"
    ? Array.from(new Intl.Segmenter(undefined, { granularity: "grapheme" }).segment(subject), ({ segment }) => segment)
    : Array.from(subject);
  for (const segment of segments) {
    const size = encoder.encode(segment).byteLength;
    if (bytes + size > available) {
      break;
    }
    truncated += segment;
    bytes += size;
  }
  return prefix + truncated;
}

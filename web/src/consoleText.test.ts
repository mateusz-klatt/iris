import { afterEach, describe, expect, it } from "vitest";

import { consoleLabel, replySubject } from "./consoleText";
import i18n, { englishTranslation, loadLocale, supportedLocales } from "./i18n";

afterEach(async () => { await loadLocale("en"); });

describe("console protocol labels", () => {
  it.each([
    ["claim", "exact", "claimKindExact"],
    ["claim", "prefix", "claimKindPrefix"],
    ["session", "active", "sessionStateActive"],
    ["session", "lost", "sessionStateLost"],
    ["session", "ended", "sessionStateEnded"],
    ["delivery", "pending", "deliveryStatePending"],
    ["delivery", "inflight", "deliveryStateInflight"],
    ["delivery", "acked", "deliveryStateAcked"],
    ["delivery", "handled", "deliveryStateHandled"],
  ] as const)("translates %s value %s without changing the protocol", async (kind, value, key) => {
    await loadLocale("pl");
    const t = i18n.getFixedT("pl");
    const label = consoleLabel(kind, value, t);
    expect(label).toBe(t(`operator.${key}`));
    expect(label).not.toBe(value);
    expect(label).not.toBe(englishTranslation.operator[key]);
  });

  it.each(["new-state", "operator.claimKindExact", "toString", "__proto__", ""])("preserves unknown hub data %j", (value) => {
    for (const kind of ["claim", "session", "delivery"] as const) {
      expect(consoleLabel(kind, value, i18n.t.bind(i18n))).toBe(value);
    }
  });
});

it("suggests a localized reply subject in every locale and preserves existing prefixes", async () => {
  const subjects: string[] = [];
  for (const locale of supportedLocales) {
    await loadLocale(locale);
    const t = i18n.getFixedT(locale);
    const subject = "Rollout {{plan}} / خطة";
    const suggested = replySubject(subject, t);
    expect(suggested, locale).toBe(t("operator.replySubject", { subject }));
    expect(suggested, locale).toContain(subject);
    subjects.push(suggested);
    expect(replySubject(suggested, t), locale).toBe(suggested);
    const english = englishTranslation.operator.replySubject.replace("{{subject}}", subject);
    expect(replySubject(english, t), locale).toBe(english);
  }
  for (const locale of supportedLocales) {
    const t = i18n.getFixedT(locale);
    for (const subject of subjects) {
      expect(replySubject(subject, t), locale).toBe(subject);
    }
  }
});

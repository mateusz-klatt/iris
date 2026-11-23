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
    const suggested = replySubject(subject, t, false, 1024);
    expect(suggested, locale).toBe(t("operator.replySubject", { subject }));
    expect(suggested, locale).toContain(subject);
    subjects.push(suggested);
    expect(replySubject(suggested, t, true, 1024), locale).toBe(suggested);
    const english = englishTranslation.operator.replySubject.replace("{{subject}}", subject);
    expect(replySubject(english, t, true, 1024), locale).toBe(english);
  }
  for (const locale of supportedLocales) {
    const t = i18n.getFixedT(locale);
    for (const subject of subjects) {
      for (const variant of [subject, subject.toUpperCase(), subject.toLowerCase()]) {
        expect(replySubject(variant, t, true, 1024), locale).toBe(variant);
      }
    }
  }
});

describe("reply subjects", () => {
  it("uses message metadata to distinguish a root subject from an existing reply", () => {
    const t = i18n.getFixedT("en");
    expect(replySubject("Odp: parser test plan", t, false, 1024)).toBe("Re: Odp: parser test plan");
    expect(replySubject("Odp: parser test plan", t, true, 1024)).toBe("Odp: parser test plan");
    expect(replySubject("RE: Rollout", t, true, 1024)).toBe("RE: Rollout");
    expect(replySubject("RE: Rollout", t, false, 1024)).toBe("Re: RE: Rollout");
  });

  it.each([
    "Re:Rollout",
    "Odp:parser test plan",
    "prefix Odp: parser test plan",
    " Odp: parser test plan",
    "Re\n: Rollout",
    "Re:\nRollout",
    "Re\t: Rollout",
    "Re : Rollout",
  ])("requires a known prefix and its exact delimiter at the start: %j", (subject) => {
    expect(replySubject(subject, i18n.getFixedT("en"), true, 1024)).toBe(`Re: ${subject}`);
  });

  it("reserves bytes for a longer localized prefix without exceeding the subject limit", async () => {
    await loadLocale("pl");
    const subject = "x".repeat(1020);
    expect(replySubject(subject, i18n.getFixedT("en"), false, 1024)).toBe(`Re: ${subject}`);
    const reply = replySubject(subject, i18n.getFixedT("pl"), false, 1024);
    expect(reply).toBe(`Odp: ${"x".repeat(1019)}`);
    expect(new TextEncoder().encode(reply).byteLength).toBe(1024);
  });

  it.each([
    ["e\u0301", 3],
    ["👩🏽‍💻", 15],
    ["🇵🇱", 8],
  ] as const)("keeps the grapheme %s whole at the UTF-8 boundary", async (grapheme, size) => {
    await loadLocale("pl");
    const t = i18n.getFixedT("pl");
    const fits = "a".repeat(1019 - size);
    const exact = replySubject(`${fits}${grapheme}x`, t, false, 1024);
    expect(exact).toBe(`Odp: ${fits}${grapheme}`);
    expect(new TextEncoder().encode(exact).byteLength).toBe(1024);

    const overflows = `${fits}a`;
    expect(replySubject(`${overflows}${grapheme}x`, t, false, 1024)).toBe(`Odp: ${overflows}`);
  });
});

import { describe, expect, it } from "vitest";

import { readUiLocale, uiLocalePreferenceKey, writeUiLocale } from "./preferences";

function memoryStorage(initial: Record<string, string> = {}) {
  const values = new Map(Object.entries(initial));
  return {
    getItem: (key: string) => values.get(key) ?? null,
    setItem: (key: string, value: string) => {
      values.set(key, value);
    },
    read: (key: string) => values.get(key) ?? null,
  };
}

describe("interface language preference", () => {
  it("stores only a supported locale on this computer", () => {
    const storage = memoryStorage();
    expect(readUiLocale(storage)).toBeNull();
    writeUiLocale("pl", storage);
    expect(storage.read(uiLocalePreferenceKey)).toBe("pl");
    expect(readUiLocale(storage)).toBe("pl");
    expect(readUiLocale(memoryStorage({ [uiLocalePreferenceKey]: "nope" }))).toBeNull();
  });

  it("ignores storage that refuses access", () => {
    const storage = {
      getItem: () => {
        throw new Error("storage disabled");
      },
      setItem: () => {
        throw new Error("storage disabled");
      },
    };
    expect(readUiLocale(storage)).toBeNull();
    expect(() => writeUiLocale("de", storage)).not.toThrow();
  });
});

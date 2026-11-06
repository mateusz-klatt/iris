const maximumInlineImageBytes = 2 * 1024 * 1024;
const maximumInlineImageBase64Length = Math.ceil(maximumInlineImageBytes / 3) * 4;

export function isCanonicalInlineRasterImageSource(source: string): boolean {
  const formats = [
    ["data:image/png;base64,", (raw: string) => raw.startsWith("\x89PNG\r\n\x1a\n")],
    ["data:image/jpeg;base64,", (raw: string) => raw.startsWith("\xff\xd8\xff")],
    [
      "data:image/gif;base64,",
      (raw: string) => raw.startsWith("GIF87a") || raw.startsWith("GIF89a"),
    ],
    [
      "data:image/webp;base64,",
      (raw: string) =>
        raw.length >= 12 && raw.startsWith("RIFF") && raw.slice(8, 12) === "WEBP",
    ],
  ] as const;

  if (source !== source.trim()) {
    return false;
  }
  for (const [prefix, signatureMatches] of formats) {
    if (!source.startsWith(prefix)) {
      continue;
    }
    const payload = source.slice(prefix.length);
    if (payload.length === 0 || payload.length > maximumInlineImageBase64Length) {
      return false;
    }
    try {
      const raw = window.atob(payload);
      return (
        raw.length > 0 &&
        raw.length <= maximumInlineImageBytes &&
        window.btoa(raw) === payload &&
        signatureMatches(raw)
      );
    } catch {
      return false;
    }
  }
  return false;
}

const markdownLinkSchemes = new Set(["http", "https", "mailto"]);
// The control range is intentional: browser URL parsing strips some of these
// characters before protocol handling, so they must fail closed here.
// eslint-disable-next-line no-control-regex
const markdownControlCharacters = /[\u0000-\u001f\u007f-\u009f]/u;
const markdownEncodedControlCharacters =
  /%(?:0[0-9a-f]|1[0-9a-f]|7f)|%c2%(?:8[0-9a-f]|9[0-9a-f])/iu;
const markdownScheme = /^([a-z][a-z0-9+.-]*):/iu;

function hasUnsafeMarkdownLinkEncoding(target: string): boolean {
  try {
    const decoded = decodeURIComponent(target);
    return (
      markdownControlCharacters.test(decoded) ||
      markdownEncodedControlCharacters.test(decoded)
    );
  } catch {
    return true;
  }
}

export function isSafeMarkdownLinkTarget(target: string): boolean {
  if (
    target.length === 0 ||
    target !== target.trim() ||
    hasUnsafeMarkdownLinkEncoding(target) ||
    target.includes("#") ||
    target.startsWith("//") ||
    target.startsWith("\\")
  ) {
    return false;
  }

  const scheme = markdownScheme.exec(target)?.[1]?.toLowerCase();
  return scheme === undefined || markdownLinkSchemes.has(scheme);
}

export function markdownUrlTransform(
  url: string,
  key: string,
): string | undefined {
  if (key === "src") {
    return isCanonicalInlineRasterImageSource(url) ? url : undefined;
  }
  if (key === "href") {
    return isSafeMarkdownLinkTarget(url) ? url : undefined;
  }
  return undefined;
}

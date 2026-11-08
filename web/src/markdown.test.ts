import { describe, expect, it } from "vitest";

import {
  isCanonicalInlineRasterImageSource,
  isSafeMarkdownLinkTarget,
  markdownUrlTransform,
} from "./markdown";

describe("Markdown resource boundaries", () => {
  const inline = (mime: string, raw: string) =>
    `data:image/${mime};base64,${window.btoa(raw)}`;

  it.each([
    ["png", "\x89PNG\r\n\x1a\nrest"],
    ["jpeg", "\xff\xd8\xffrest"],
    ["gif", "GIF87arest"],
    ["gif", "GIF89arest"],
    ["webp", "RIFFxxxxWEBPrest"],
  ])("accepts canonical bounded %s raster bytes", (mime, raw) => {
    expect(isCanonicalInlineRasterImageSource(inline(mime, raw))).toBe(true);
  });

  it.each([
    ["local route", "/logout"],
    ["relative route", "image.png"],
    ["blob URL", "blob:http://test/id"],
    ["remote URL", "https://tracker.invalid/pixel.png"],
    ["uppercase MIME", inline("PNG", "\x89PNG\r\n\x1a\nrest")],
    ["surrounding whitespace", ` ${inline("png", "\x89PNG\r\n\x1a\nrest")}`],
    ["empty payload", "data:image/png;base64,"],
    ["invalid Base64", "data:image/png;base64,%%%%"],
    ["MIME mismatch", inline("png", "GIF89arest")],
    ["noncanonical Base64", "data:image/gif;base64,R0lGODlheB=="],
    [
      "oversized payload",
      `data:image/png;base64,${"A".repeat(Math.ceil((2 * 1024 * 1024) / 3) * 4 + 1)}`,
    ],
    ["short WebP", inline("webp", "RIFFWEBP")],
    ["wrong WebP marker", inline("webp", "RIFFxxxxNOPErest")],
  ])("rejects %s", (_label, source) => {
    expect(isCanonicalInlineRasterImageSource(source)).toBe(false);
  });

  it.each([
    ["HTTP", "http://example.test/path"],
    ["HTTPS", "HTTPS://example.test/path"],
    ["mailto", "mailto:operator@example.test"],
    ["root-relative", "/inbox"],
    ["path-relative", "../messages/101"],
    ["query-relative", "?project=11"],
    ["percent-encoded Polish", "https://example.test/Wroc%C5%82aw"],
    ["percent-encoded CJK", "https://example.test/%E8%B7%AF%E5%BE%84"],
    ["percent-encoded emoji", "https://example.test/%F0%9F%94%90"],
    ["encoded percent sign", "https://example.test/100%25"],
    ["double-encoded Polish", "https://example.test/Wroc%25C5%2582aw"],
  ])("accepts a safe %s link target", (_label, target) => {
    expect(isSafeMarkdownLinkTarget(target)).toBe(true);
  });

  it.each([
    ["empty target", ""],
    ["surrounding whitespace", " https://example.test"],
    ["literal control character", "https://example.test/\u0001probe"],
    ["encoded control character", "mailto:test@example.test?subject=%0aBcc"],
    ["encoded C1 control character", "https://example.test/%C2%85probe"],
    ["double-encoded control character", "https://example.test/%250aprobe"],
    ["double-encoded C1 control character", "https://example.test/%25C2%2585probe"],
    ["malformed percent escape", "https://example.test/%probe"],
    ["fragment target", "#checkpoint"],
    ["relative target with fragment", "/#account"],
    ["protocol-relative target", "//tracker.invalid/path"],
    ["backslash target", "\\tracker.invalid/path"],
    ["JavaScript", "javascript:alert(1)"],
    ["data", "data:text/html,unsafe"],
    ["blob", "blob:https://example.test/id"],
  ])("rejects a %s link target", (_label, target) => {
    expect(isSafeMarkdownLinkTarget(target)).toBe(false);
  });

  it("fails closed for every transformed URL attribute", () => {
    const png = inline("png", "\x89PNG\r\n\x1a\nrest");

    expect(markdownUrlTransform(png, "src")).toBe(png);
    expect(markdownUrlTransform("https://tracker.invalid/pixel", "src")).toBeUndefined();
    expect(markdownUrlTransform("https://example.test", "href")).toBe("https://example.test");
    expect(markdownUrlTransform("javascript:alert(1)", "href")).toBeUndefined();
    expect(markdownUrlTransform("#checkpoint", "href")).toBeUndefined();
    expect(markdownUrlTransform("https://example.test", "cite")).toBeUndefined();
  });
});

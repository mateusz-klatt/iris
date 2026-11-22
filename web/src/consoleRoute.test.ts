import { describe, expect, it } from "vitest";

import { fallbackRoute, resolveConsoleHash, routeToHash } from "./consoleRoute";

const operator = { member: false, isOperator: true, canSend: true };
const memberReadOnly = { member: true, isOperator: false, canSend: false };
const memberWritable = { member: true, isOperator: false, canSend: true };
const nonOperatorMember = { member: false, isOperator: false, canSend: true };

describe("consoleRoute", () => {
  it("formats canonical hashes for every view and mail scope", () => {
    expect(routeToHash("messages", "mine")).toBe("#/messages");
    expect(routeToHash("messages", "all")).toBe("#/activity");
    expect(routeToHash("fleet", "mine")).toBe("#/fleet");
    expect(routeToHash("claims", "mine")).toBe("#/claims");
    expect(routeToHash("diagnostics", "mine")).toBe("#/diagnostics");
    expect(routeToHash("compose", "mine")).toBe("#/compose");
    expect(routeToHash("account", "mine")).toBe("#/account");
    expect(routeToHash("access", "mine")).toBe("#/access");
  });

  it("treats empty hashes as absent so the initial-view latch decides", () => {
    expect(resolveConsoleHash("", operator)).toEqual({ kind: "none" });
    expect(resolveConsoleHash("#", operator)).toEqual({ kind: "none" });
    expect(resolveConsoleHash("#/", operator)).toEqual({ kind: "none" });
  });

  it("allows every route for an operator and falls back for unknown hashes", () => {
    expect(resolveConsoleHash("#/messages", operator)).toEqual({ kind: "allowed", route: { view: "messages", mailScope: "mine" } });
    expect(resolveConsoleHash("#/activity", operator)).toEqual({ kind: "allowed", route: { view: "messages", mailScope: "all" } });
    expect(resolveConsoleHash("#fleet", operator)).toEqual({ kind: "allowed", route: { view: "fleet", mailScope: "mine" } });
    expect(resolveConsoleHash("#/claims", operator)).toEqual({ kind: "allowed", route: { view: "claims", mailScope: "mine" } });
    expect(resolveConsoleHash("#/diagnostics", operator)).toEqual({ kind: "allowed", route: { view: "diagnostics", mailScope: "mine" } });
    expect(resolveConsoleHash("#/compose", operator)).toEqual({ kind: "allowed", route: { view: "compose", mailScope: "mine" } });
    expect(resolveConsoleHash("#/account", operator)).toEqual({ kind: "allowed", route: { view: "account", mailScope: "mine" } });
    expect(resolveConsoleHash("#/access", operator)).toEqual({ kind: "allowed", route: { view: "access", mailScope: "mine" } });

    expect(resolveConsoleHash("#/unknown", operator)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#content", operator)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/threads/ms_root", operator)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#toString", operator)).toEqual({ kind: "fallback", route: fallbackRoute });
  });

  it("restricts diagnostics, access, all activity, and unavailable compose safely", () => {
    expect(resolveConsoleHash("#/diagnostics", memberReadOnly)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/access", memberReadOnly)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/activity", memberReadOnly)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/compose", memberReadOnly)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/compose", memberWritable)).toEqual({ kind: "allowed", route: { view: "compose", mailScope: "mine" } });
    expect(resolveConsoleHash("#/access", nonOperatorMember)).toEqual({ kind: "fallback", route: fallbackRoute });
    expect(resolveConsoleHash("#/fleet", memberReadOnly)).toEqual({ kind: "allowed", route: { view: "fleet", mailScope: "mine" } });
  });
});

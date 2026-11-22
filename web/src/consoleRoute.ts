export type ConsoleView = "messages" | "fleet" | "claims" | "diagnostics" | "compose" | "account" | "access";
export type MailScope = "mine" | "all";

export interface ConsoleRoute {
  view: ConsoleView;
  mailScope: MailScope;
}

export interface RoutePermissions {
  member: boolean;
  isOperator: boolean;
  canSend: boolean;
}

const routes = new Map<string, ConsoleRoute>([
  ["messages", { view: "messages", mailScope: "mine" }],
  ["activity", { view: "messages", mailScope: "all" }],
  ["fleet", { view: "fleet", mailScope: "mine" }],
  ["claims", { view: "claims", mailScope: "mine" }],
  ["diagnostics", { view: "diagnostics", mailScope: "mine" }],
  ["compose", { view: "compose", mailScope: "mine" }],
  ["account", { view: "account", mailScope: "mine" }],
  ["access", { view: "access", mailScope: "mine" }],
]);

export const fallbackRoute: ConsoleRoute = { view: "messages", mailScope: "mine" };

export function routeToHash(view: ConsoleView, mailScope: MailScope): string {
  return view === "messages" && mailScope === "all" ? "#/activity" : `#/${view}`;
}

function isRouteAuthorized(route: ConsoleRoute, permissions: RoutePermissions): boolean {
  switch (route.view) {
    case "diagnostics":
      return !permissions.member;
    case "access":
      return permissions.isOperator;
    case "compose":
      return permissions.canSend;
    case "messages":
      return route.mailScope === "mine" || !permissions.member;
    default:
      return true;
  }
}

export type HashResolution =
  | { kind: "none" }
  | { kind: "allowed"; route: ConsoleRoute }
  | { kind: "fallback"; route: ConsoleRoute };

export function resolveConsoleHash(hash: string, permissions: RoutePermissions): HashResolution {
  const key = hash.replace(/^#\/?/, "");
  if (key === "") {
    return { kind: "none" };
  }
  const route = routes.get(key);
  if (route === undefined || !isRouteAuthorized(route, permissions)) {
    return { kind: "fallback", route: fallbackRoute };
  }
  return { kind: "allowed", route };
}

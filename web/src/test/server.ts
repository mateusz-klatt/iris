import { http, HttpResponse } from "msw";
import { setupServer } from "msw/node";

import type { Claim, DoctorReport, Overview, ThreadDetail, ThreadsPage } from "../api";

export const sessionOverview: Overview = {
  repositories: [{ id: "rp_iris", name: "iris", aliases: ["iris-wt"] }],
  agents: [
    {
      id: "ag_claude",
      name: "claude-host",
      client: "claude",
      host: "local",
      repository_id: "rp_iris",
      worktree: "/work/iris",
      branch: "main",
      state: "active",
      last_seen: "2026-11-06T10:00:00Z",
      unread: 1,
    },
  ],
  claims: [
    {
      fence: 7,
      repository_id: "rp_iris",
      path: "docs/api.md",
      kind: "exact",
      holder: "claude-host",
      origin: "auto",
      expires_at: "2026-11-06T11:00:00Z",
    },
  ],
  human_unread: 1,
};

export const emptyOverview: Overview = {
  repositories: [],
  agents: [],
  claims: [],
  human_unread: 0,
};

export const doctorReport: DoctorReport = {
  checked_at: "2026-11-06T09:00:00Z",
  clients: [
    {
      client: "claude",
      version: "1.0.0",
      status: "ok",
      detail: "Hook received.",
      last_hook_at: "2026-11-06T08:00:00Z",
    },
    {
      client: "codex",
      version: "0.9.0",
      status: "silent",
      detail: "No hook has arrived since install.",
      last_hook_at: "",
    },
    {
      client: "cursor",
      version: "",
      status: "not_configured",
      detail: "No configuration file.",
      last_hook_at: "",
    },
    {
      client: "kimi",
      version: "1.2.0",
      status: "needs_human",
      detail: "Review hooks once in /hooks.",
      last_hook_at: "",
    },
  ],
};

export const threadsPage: ThreadsPage = {
  threads: [
    {
      id: "ms_root",
      subject: "Rollout check",
      participants: ["claude-host", "human"],
      count: 1,
      last_at: "2026-11-06T10:05:00Z",
      unread: 1,
    },
  ],
};

export const threadDetail: ThreadDetail = {
  id: "ms_root",
  messages: [
    {
      id: "ms_root",
      sender: "claude-host",
      recipients: [{ name: "human", state: "pending" }],
      subject: "Rollout check",
      body: [
        "# Release",
        "",
        "All checks passed.",
        "",
        "`<script>alert(1)</script>`",
        "",
        "<script>alert(1)</script>",
        "",
        "[note](https://example.test/note)",
        "",
        "[blocked](javascript:alert(1))",
      ].join("\n"),
      created_at: "2026-11-06T10:05:00Z",
    },
  ],
};

let claims: Claim[] = sessionOverview.claims.map((claim) => ({ ...claim }));
let humanUnread = sessionOverview.human_unread;

export function resetFixtures(): void {
  claims = sessionOverview.claims.map((claim) => ({ ...claim }));
  humanUnread = sessionOverview.human_unread;
}

export function setHumanUnread(value: number): void {
  humanUnread = value;
}

function overview(): Overview {
  return {
    ...sessionOverview,
    claims: claims.map((claim) => ({ ...claim })),
    human_unread: humanUnread,
  };
}

export const server = setupServer(
  http.get("*/v1/ui/overview", () => HttpResponse.json(overview())),
  http.get("*/v1/ui/doctor", () => HttpResponse.json(doctorReport)),
  http.get("*/v1/ui/threads", ({ request }) => {
    const url = new URL(request.url);
    if (url.pathname !== "/v1/ui/threads") {
      return undefined;
    }
    if (url.searchParams.get("q") === "missing") {
      return HttpResponse.json({ threads: [] } satisfies ThreadsPage);
    }
    return HttpResponse.json(threadsPage);
  }),
  http.get("*/v1/ui/threads/:id", ({ params }) => {
    if (params.id !== threadDetail.id) {
      return HttpResponse.json({ error: "missing" }, { status: 404 });
    }
    return HttpResponse.json(threadDetail);
  }),
  http.post("*/v1/ui/threads/:id/read", () => new HttpResponse(null, { status: 204 })),
  http.post("*/v1/ui/messages", () => new HttpResponse(null, { status: 204 })),
  http.post("*/v1/ui/claims/:fence/revoke", ({ params }) => {
    const fence = Number(params.fence);
    claims = claims.filter((claim) => claim.fence !== fence);
    return new HttpResponse(null, { status: 204 });
  }),
);

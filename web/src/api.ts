export class ApiError extends Error {
  constructor(readonly status: number) {
    super(`Iris request failed with HTTP ${status}.`);
    this.name = "ApiError";
  }
}

export interface Repository {
  id: string;
  name: string;
  aliases: string[];
}

export interface Agent {
  id: string;
  name: string;
  client: string;
  host: string;
  repository_id: string;
  worktree: string;
  branch: string;
  state: string;
  last_seen: string;
  unread: number;
}

export interface Claim {
  fence: number;
  repository_id: string;
  path: string;
  kind: string;
  holder: string;
  origin: string;
  expires_at: string | null;
}

export interface Overview {
  repositories: Repository[];
  agents: Agent[];
  claims: Claim[];
  human_unread: number;
}

export interface ThreadSummary {
  id: string;
  subject: string;
  participants: string[];
  count: number;
  last_at: string;
  unread: number;
}

export interface ThreadsPage {
  threads: ThreadSummary[];
}

export interface ThreadRecipient {
  name: string;
  state: string;
}

export interface ThreadMessage {
  id: string;
  sender: string;
  recipients: ThreadRecipient[];
  subject: string;
  body: string;
  created_at: string;
}

export interface ThreadDetail {
  id: string;
  messages: ThreadMessage[];
}

export type DoctorStatus = "ok" | "not_configured" | "needs_human" | "silent";

export interface DoctorClient {
  client: string;
  version: string;
  status: DoctorStatus;
  detail: string;
  last_hook_at: string;
}

export interface DoctorReport {
  checked_at: string | null;
  clients: DoctorClient[];
}

export interface OutgoingMessage {
  key: string;
  to: string[];
  subject: string;
  body: string;
  reply_to: string;
}

export interface ThreadQuery {
  repository_id?: string;
  agent?: string;
  q?: string;
  before?: string;
  limit?: number;
}

export interface WatchFrame {
  cursor: number;
  kind: string;
}

export type WatchStatus = "connecting" | "live" | "reconnecting";

async function request(
  path: string,
  init: RequestInit = {},
): Promise<Response> {
  const response = await fetch(new URL(path, window.location.origin), {
    ...init,
    cache: "no-store",
    credentials: "same-origin",
    headers: {
      Accept: "application/json",
      ...init.headers,
    },
  });
  if (!response.ok) {
    throw new ApiError(response.status);
  }
  return response;
}

async function getJson<T>(path: string, signal?: AbortSignal): Promise<T> {
  const response = await request(path, { method: "GET", signal });
  return response.json() as Promise<T>;
}

async function postJson(path: string, body: unknown, signal?: AbortSignal): Promise<void> {
  await request(path, {
    method: "POST",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body),
  });
}

export function getOverview(signal?: AbortSignal): Promise<Overview> {
  return getJson("/v1/ui/overview", signal);
}

export async function getDoctor(signal?: AbortSignal): Promise<DoctorReport | null> {
  try {
    return await getJson<DoctorReport>("/v1/ui/doctor", signal);
  } catch (error) {
    if (error instanceof ApiError && error.status === 404) {
      return null;
    }
    throw error;
  }
}

export function listThreads(query: ThreadQuery, signal?: AbortSignal): Promise<ThreadsPage> {
  const params = new URLSearchParams();
  for (const [key, value] of Object.entries(query)) {
    if (value !== undefined && value !== "") {
      params.set(key, String(value));
    }
  }
  const suffix = params.size === 0 ? "" : `?${params.toString()}`;
  return getJson(`/v1/ui/threads${suffix}`, signal);
}

export function getThread(id: string, signal?: AbortSignal): Promise<ThreadDetail> {
  return getJson(`/v1/ui/threads/${encodeURIComponent(id)}`, signal);
}

export function sendMessage(message: OutgoingMessage, signal?: AbortSignal): Promise<void> {
  return postJson("/v1/ui/messages", message, signal);
}

export function revokeClaim(fence: number, signal?: AbortSignal): Promise<void> {
  return postJson(`/v1/ui/claims/${fence}/revoke`, {}, signal);
}

export function watchUi(
  onFrame: (frame: WatchFrame) => void,
  onStatus?: (status: WatchStatus) => void,
): () => void {
  let closed = false;
  let socket: WebSocket | null = null;
  let timer = 0;

  const open = (reconnect: boolean) => {
    onStatus?.(reconnect ? "reconnecting" : "connecting");
    const url = new URL("/v1/ui/watch", window.location.href);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    socket = new WebSocket(url.toString());
    socket.onopen = () => {
      onStatus?.("live");
    };
    socket.onmessage = (event) => {
      if (typeof event.data !== "string") {
        return;
      }
      try {
        const frame = JSON.parse(event.data) as WatchFrame;
        if (typeof frame.kind === "string") {
          onFrame(frame);
        }
      } catch {
        // A frame that is not JSON carries nothing the UI can refresh from.
      }
    };
    socket.onclose = () => {
      if (!closed) {
        timer = window.setTimeout(() => open(true), 1000);
      }
    };
  };

  open(false);
  return () => {
    closed = true;
    window.clearTimeout(timer);
    socket?.close();
  };
}

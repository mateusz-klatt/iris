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
  /** Present when older threads follow: pass it as `before` for the next page. */
  next?: string;
}

export interface ThreadRecipient {
  name: string;
  state: string;
}

export interface ThreadMessage {
  id: string;
  sender: string;
  /** The sender's repository. A thread can cross repositories (ADR 004). */
  repository_id?: string;
  recipients: ThreadRecipient[];
  subject: string;
  body: string;
  created_at: string;
}

export interface ThreadDetail {
  id: string;
  messages: ThreadMessage[];
}

// slow: hooks run, but shell reconciliation often passes its budget, so changes go unclaimed.
export type DoctorStatus = "ok" | "not_configured" | "needs_human" | "silent" | "slow";

// The hub omits version, detail and last_hook_at when they are empty.
export interface DoctorClient {
  client: string;
  version?: string;
  status: DoctorStatus;
  detail?: string;
  last_hook_at?: string;
}

/** One host's last doctor report (ADR 007). A doctor too old to name its host reports as "unknown". */
export interface DoctorReport {
  host: string;
  checked_at: string | null;
  clients: DoctorClient[];
}

/** Every host's last report. A host that never ran doctor is absent. */
export interface DoctorReports {
  hosts: DoctorReport[];
}

export interface OutgoingMessage {
  key: string;
  to: string[];
  subject: string;
  body: string;
  reply_to: string;
  /** The repository the operator writes from; the message belongs to it (ADR 004). */
  repository_id?: string;
}

export interface ThreadQuery {
  repository_id?: string;
  agent?: string;
  q?: string;
  before?: string;
  limit?: number;
}

/** One recipient of the human's message whose delivery is in trouble. */
export interface DeliveryRecipient {
  name: string;
  /** The delivery's state: pending or inflight. An acked one is never listed. */
  state: string;
  /** unreachable: no active session. waiting: unacked message older than wait_s. */
  reason: "unreachable" | "waiting";
  /** The recipient's latest session state: active, lost, ended or none. */
  execution: string;
}

export interface DeliveryProblem {
  id: string;
  thread_id: string;
  subject: string;
  created_at: string;
  recipients: DeliveryRecipient[];
}

/** The human's messages in delivery trouble, and the thresholds the hub judged them by. */
export interface DeliveryProblems {
  messages: DeliveryProblem[];
  /** More qualifying messages exist beyond the newest messages returned. */
  more: boolean;
  window_s: number;
  wait_s: number;
}

export interface WatchFrame {
  cursor: number;
  kind: string;
}

export type WatchStatus = "connecting" | "live" | "reconnecting" | "unauthorized";

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

export async function getDoctor(signal?: AbortSignal): Promise<DoctorReports | null> {
  try {
    return await getJson<DoctorReports>("/v1/ui/doctor", signal);
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

/** The human's mail in delivery trouble, sent from repositoryId's view or anywhere on the hub. */
export function getDeliveryProblems(repositoryId = "", signal?: AbortSignal): Promise<DeliveryProblems> {
  const suffix = repositoryId === "" ? "" : `?repository_id=${encodeURIComponent(repositoryId)}`;
  return getJson(`/v1/ui/deliveries${suffix}`, signal);
}

/** A thread as seen from repositoryId, or from the whole hub when it is empty. */
export function getThread(id: string, repositoryId = "", signal?: AbortSignal): Promise<ThreadDetail> {
  const suffix = repositoryId === "" ? "" : `?repository_id=${encodeURIComponent(repositoryId)}`;
  return getJson(`/v1/ui/threads/${encodeURIComponent(id)}${suffix}`, signal);
}

/** Marks the operator's mail in a thread as read, in repositoryId or the whole hub (A13). */
/** Reads the operator's mail in a thread, and says how many deliveries that read. */
export async function readThread(id: string, repositoryId = "", signal?: AbortSignal): Promise<number> {
  const response = await request(`/v1/ui/threads/${encodeURIComponent(id)}/read`, {
    method: "POST",
    signal,
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ repository_id: repositoryId }),
  });
  const out = (await response.json()) as { read: number };
  return out.read;
}

export function sendMessage(message: OutgoingMessage, signal?: AbortSignal): Promise<void> {
  return postJson("/v1/ui/messages", message, signal);
}

export function revokeClaim(fence: number, signal?: AbortSignal): Promise<void> {
  return postJson(`/v1/ui/claims/${fence}/revoke`, {}, signal);
}

// A socket that closes is opened again after a delay that starts at a second and doubles up to
// thirty, and a socket that opens resets it. Each delay is half its step plus a random share of
// the other half, so consoles that lost the hub together do not return together.
const firstWatchDelay = 1000;
const lastWatchDelay = 30_000;

export interface WatchOptions {
  /**
   * Asks over HTTP whether the session still holds. A browser never sees why a WebSocket
   * handshake failed, so a 401 there looks like any other closed socket.
   */
  check?: () => Promise<unknown>;
  random?: () => number;
}

export function watchUi(
  onFrame: (frame: WatchFrame) => void,
  onStatus?: (status: WatchStatus) => void,
  { check = getOverview, random = Math.random }: WatchOptions = {},
): () => void {
  let closed = false;
  let socket: WebSocket | null = null;
  let timer = 0;
  let failures = 0;

  const retry = () => {
    const step = Math.min(lastWatchDelay, firstWatchDelay * 2 ** failures);
    failures++;
    timer = window.setTimeout(() => open(true), step / 2 + random() * (step / 2));
  };

  const open = (reconnect: boolean) => {
    onStatus?.(reconnect ? "reconnecting" : "connecting");
    const url = new URL("/v1/ui/watch", window.location.href);
    url.protocol = url.protocol === "https:" ? "wss:" : "ws:";
    let opened = false;
    socket = new WebSocket(url.toString());
    socket.onopen = () => {
      opened = true;
      failures = 0;
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
      if (closed) {
        return;
      }
      if (opened) {
        retry();
        return;
      }
      // Never opened: the hub may be down, or it may have refused the session. Only an HTTP
      // request can tell, and a refused session ends the watch rather than knocking forever.
      check().then(
        () => {
          if (!closed) {
            retry();
          }
        },
        (error: unknown) => {
          if (closed) {
            return;
          }
          if (error instanceof ApiError && error.status === 401) {
            closed = true;
            onStatus?.("unauthorized");
            return;
          }
          retry();
        },
      );
    };
  };

  open(false);
  return () => {
    closed = true;
    window.clearTimeout(timer);
    socket?.close();
  };
}

import type {
  AcceptedMessage,
  Conversation,
  ConversationView,
  RuntimeOptions,
  RuntimeEvent,
} from "./types";

const apiRoot = "/v1";

async function request<T>(input: string, init: RequestInit = {}): Promise<T> {
  const response = await fetch(input, {
    ...init,
    headers: {
      Accept: "application/json",
      ...init.headers,
    },
  });

  if (!response.ok) {
    let message = `${response.status} ${response.statusText}`;
    try {
      const body = (await response.json()) as { error?: string };
      if (body.error) message = body.error;
    } catch {
      // Keep the HTTP status when the server did not return JSON.
    }
    throw new Error(message);
  }
  return (await response.json()) as T;
}

export function listConversations(signal?: AbortSignal): Promise<Conversation[]> {
  return request<Conversation[]>(`${apiRoot}/conversations`, { signal });
}

export interface CreateConversationOptions {
  environment: string;
  workspace?: string;
  git_repository?: string;
  git_revision?: string;
}

export function createConversation(options: CreateConversationOptions, signal?: AbortSignal): Promise<Conversation> {
  return request<Conversation>(`${apiRoot}/conversations`, {
    method: "POST",
    body: JSON.stringify(options),
    headers: { "Content-Type": "application/json" },
    signal,
  });
}

export function getRuntimeOptions(signal?: AbortSignal): Promise<RuntimeOptions> {
  return request<RuntimeOptions>(`${apiRoot}/options`, { signal });
}

export function getConversation(id: string, signal?: AbortSignal): Promise<ConversationView> {
  return request<ConversationView>(`${apiRoot}/conversations/${encodeURIComponent(id)}`, { signal });
}

export function submitMessage(
  id: string,
  text: string,
  idempotencyKey: string,
  signal?: AbortSignal,
): Promise<AcceptedMessage> {
  return request<AcceptedMessage>(`${apiRoot}/conversations/${encodeURIComponent(id)}/messages`, {
    method: "POST",
    body: JSON.stringify({ parts: [{ type: "text", text }] }),
    headers: {
      "Content-Type": "application/json",
      "Idempotency-Key": idempotencyKey,
    },
    signal,
  });
}

export async function consumeEvents(
  id: string,
  after: number,
  onEvent: (event: RuntimeEvent) => void,
  signal: AbortSignal,
  onConnected?: () => void,
): Promise<void> {
  const response = await fetch(
    `${apiRoot}/conversations/${encodeURIComponent(id)}/events?after=${after}`,
    {
      headers: { Accept: "text/event-stream" },
      signal,
    },
  );
  if (!response.ok) {
    throw new Error(`event stream failed: ${response.status} ${response.statusText}`);
  }
  if (!response.body) throw new Error("event stream is unavailable in this browser");

  onConnected?.();

  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  let buffer = "";
  let data: string[] = [];

  const dispatch = () => {
    if (data.length === 0) return;
    const event = JSON.parse(data.join("\n")) as RuntimeEvent;
    onEvent(event);
    data = [];
  };

  while (true) {
    const { value, done } = await reader.read();
    buffer += decoder.decode(value, { stream: !done });
    let newline = buffer.indexOf("\n");
    while (newline >= 0) {
      let line = buffer.slice(0, newline);
      buffer = buffer.slice(newline + 1);
      if (line.endsWith("\r")) line = line.slice(0, -1);
      if (line === "") {
        dispatch();
      } else if (line.startsWith("data:")) {
        data.push(line.slice(5).trimStart());
      }
      newline = buffer.indexOf("\n");
    }
    if (done) {
      if (buffer !== "") data.push(buffer);
      dispatch();
      return;
    }
  }
}

export function newIdempotencyKey(): string {
  if (typeof crypto.randomUUID === "function") return crypto.randomUUID();
  return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
}

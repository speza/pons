import {
  Fragment,
  type FormEvent,
  type KeyboardEvent,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
} from "react";

import {
  consumeEvents,
  createConversation,
  getRuntimeOptions,
  getConversation,
  listConversations,
  newIdempotencyKey,
  submitMessage,
} from "./api";
import type {
  Conversation,
  ConversationView,
  Message,
  MessagePart,
  RuntimeOptions,
  RuntimeEvent,
  ToolCall,
  ToolResult,
} from "./types";

const activeConversationKey = "pons.activeConversation";

type ConnectionState = "idle" | "loading" | "connected" | "reconnecting" | "error";

interface PendingSubmission {
  text: string;
  key: string;
}

function readStoredConversation(): string {
  try {
    return localStorage.getItem(activeConversationKey) ?? "";
  } catch {
    return "";
  }
}

function writeStoredConversation(id: string): void {
  try {
    if (id) localStorage.setItem(activeConversationKey, id);
    else localStorage.removeItem(activeConversationKey);
  } catch {
    // Storage is an optional convenience, not part of runtime correctness.
  }
}

function wait(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    const timer = window.setTimeout(resolve, ms);
    signal.addEventListener(
      "abort",
      () => {
        window.clearTimeout(timer);
        resolve();
      },
      { once: true },
    );
  });
}

function upsert<T extends { id: string }>(items: T[], item: T): T[] {
  const index = items.findIndex((existing) => existing.id === item.id);
  if (index < 0) return [...items, item];
  const next = [...items];
  next[index] = item;
  return next;
}

function applyEvent(view: ConversationView, event: RuntimeEvent): ConversationView {
  const next: ConversationView = {
    ...view,
    messages: view.messages,
    submissions: view.submissions,
    tool_calls: view.tool_calls,
    environment_events: view.environment_events,
    active_run: view.active_run,
  };
  if (event.cursor && event.cursor > next.event_cursor) next.event_cursor = event.cursor;

  switch (event.type) {
    case "message.upserted":
      if (event.message) next.messages = upsert(next.messages, event.message);
      break;
    case "submission.updated":
      if (event.submission) next.submissions = upsert(next.submissions ?? [], event.submission);
      break;
    case "tool_call.updated":
      if (event.tool_call) next.tool_calls = upsert(next.tool_calls ?? [], event.tool_call);
      break;
    case "run.updated":
      next.active_run = event.run && ["queued", "running"].includes(event.run.status) ? event.run : undefined;
      break;
    case "environment.progress":
      if (event.cursor && event.environment_progress &&
          !next.environment_events?.some((entry) => entry.cursor === event.cursor)) {
        next.environment_events = [...(next.environment_events ?? []), event];
      }
      break;
  }
  return next;
}

function formatDate(value: string): string {
  return new Intl.DateTimeFormat(undefined, {
    dateStyle: "medium",
    timeStyle: "short",
  }).format(new Date(value));
}

function formatJson(value: unknown): string {
  if (value === undefined || value === null) return "{}";
  if (typeof value === "string") return value;
  return JSON.stringify(value, null, 2) ?? String(value);
}

function shortId(id: string): string {
  return id.slice(0, 8);
}

function environmentLabel(value: string | undefined): string {
  if (!value || value === "none") return "In-process";
  if (value === "seatbelt") return "macOS Seatbelt";
  return value;
}

function resultText(result: ToolResult): string {
  return result.error
    ? `${result.output ?? ""}${result.output ? "\n" : ""}${result.error}`
    : result.output ?? "No output";
}

function App() {
  const preferredConversation = useRef(readStoredConversation());
  const [conversations, setConversations] = useState<Conversation[]>([]);
  const [activeId, setActiveId] = useState("");
  const [view, setView] = useState<ConversationView | null>(null);
  const [connection, setConnection] = useState<ConnectionState>("loading");
  const [listLoading, setListLoading] = useState(true);
  const [creating, setCreating] = useState(false);
  const [composer, setComposer] = useState("");
  const [sending, setSending] = useState(false);
  const [error, setError] = useState("");
  const [liveDraft, setLiveDraft] = useState("");
  const [toolProgress, setToolProgress] = useState<Record<string, string>>({});
  const [followingChat, setFollowingChat] = useState(true);
  const messageScrollRef = useRef<HTMLElement | null>(null);
  const shouldFollowChat = useRef(true);
  const [runtimeOptions, setRuntimeOptions] = useState<RuntimeOptions>({
    environments: ["none"],
    default_environment: "none",
  });
  const [newEnvironment, setNewEnvironment] = useState("none");
  const [newWorkspace, setNewWorkspace] = useState("");
  const [newSource, setNewSource] = useState<"workspace" | "git">("workspace");
  const [newGitRepository, setNewGitRepository] = useState("");
  const [newGitRevision, setNewGitRevision] = useState("");
  const [newGitBranch, setNewGitBranch] = useState("");
  const [newGitBase, setNewGitBase] = useState<"branch" | "commit">("branch");
  const liveDraftMessage = useRef("");
  const pendingSubmission = useRef<PendingSubmission | null>(null);

  const loadConversations = useCallback(async (signal?: AbortSignal) => {
    setListLoading(true);
    try {
      const values = await listConversations(signal);
      setConversations(values);
      setActiveId((current) => {
        if (current && values.some((conversation) => conversation.conversation_id === current)) return current;
        const preferred = values.find(
          (conversation) => conversation.conversation_id === preferredConversation.current,
        );
        return preferred?.conversation_id ?? values[0]?.conversation_id ?? "";
      });
    } catch (cause) {
      if (!signal?.aborted) setError(cause instanceof Error ? cause.message : "Could not load conversations");
    } finally {
      if (!signal?.aborted) setListLoading(false);
    }
  }, []);

  useEffect(() => {
    const controller = new AbortController();
    void getRuntimeOptions(controller.signal)
      .then((options) => {
        const environments = options.environments.length > 0 ? options.environments : ["none"];
        const defaultEnvironment = environments.includes(options.default_environment)
          ? options.default_environment
          : environments[0];
        setRuntimeOptions({ environments, default_environment: defaultEnvironment });
        setNewEnvironment(defaultEnvironment);
        setNewSource(defaultEnvironment === "e2b" ? "git" : "workspace");
      })
      .catch((cause) => {
        if (!controller.signal.aborted) {
          setError(cause instanceof Error ? cause.message : "Could not load runtime options");
        }
      });
    void loadConversations(controller.signal);
    return () => controller.abort();
  }, [loadConversations]);

  useEffect(() => writeStoredConversation(activeId), [activeId]);

  useEffect(() => {
    if (!activeId) {
      setView(null);
      setConnection("idle");
      return;
    }

    const controller = new AbortController();
    let stopped = false;
    let cursor = 0;
    liveDraftMessage.current = "";
    setView(null);
    setLiveDraft("");
    setToolProgress({});
    shouldFollowChat.current = true;
    setFollowingChat(true);
    setConnection("loading");

    const onEvent = (event: RuntimeEvent) => {
      if (event.cursor && event.cursor > cursor) cursor = event.cursor;
      setView((current) => (current ? applyEvent(current, event) : current));
      if (event.type === "assistant.delta" && event.delta) {
        const delta = event.delta;
        if (liveDraftMessage.current !== delta.message_id) {
          liveDraftMessage.current = delta.message_id;
          setLiveDraft(delta.text);
        } else {
          setLiveDraft((current) => current + delta.text);
        }
      }
      if (event.type === "message.upserted" && event.message?.final) {
        liveDraftMessage.current = "";
        setLiveDraft("");
      }
      if (event.type === "tool.progress" && event.progress) {
        setToolProgress((current) => ({
          ...current,
          [`${event.run_id ?? ""}:${event.progress!.tool_call_id}`]: event.progress!.text ?? "",
        }));
      }
      if (event.type === "tool_call.updated" && event.tool_call) {
        const tool = event.tool_call;
        if (tool.status !== "requested") {
          setToolProgress((current) => {
            const next = { ...current };
            delete next[`${tool.run_id}:${tool.id}`];
            delete next[tool.id];
            return next;
          });
        }
      }
    };

    const connect = async () => {
      try {
        const snapshot = await getConversation(activeId, controller.signal);
        if (stopped) return;
        cursor = snapshot.event_cursor;
        setView(snapshot);
        setConnection("connected");

        while (!stopped) {
          try {
            await consumeEvents(activeId, cursor, onEvent, controller.signal);
          } catch (cause) {
            if (stopped || controller.signal.aborted) return;
            setConnection("reconnecting");
            setError(cause instanceof Error ? cause.message : "Event stream disconnected");
          }
          if (!stopped) {
            await wait(750, controller.signal);
            setConnection("reconnecting");
          }
        }
      } catch (cause) {
        if (!stopped && !controller.signal.aborted) {
          setConnection("error");
          setError(cause instanceof Error ? cause.message : "Could not load conversation");
        }
      }
    };

    void connect();
    return () => {
      stopped = true;
      controller.abort();
    };
  }, [activeId]);

  useLayoutEffect(() => {
    const scroll = messageScrollRef.current;
    if (scroll && shouldFollowChat.current) scroll.scrollTop = scroll.scrollHeight;
  }, [activeId, view?.messages, view?.tool_calls, view?.environment_events, liveDraft, toolProgress]);

  const updateChatFollow = () => {
    const scroll = messageScrollRef.current;
    if (!scroll) return;
    const nearBottom = scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight < 96;
    shouldFollowChat.current = nearBottom;
    setFollowingChat(nearBottom);
  };

  const jumpToLatest = () => {
    shouldFollowChat.current = true;
    setFollowingChat(true);
    const scroll = messageScrollRef.current;
    if (scroll) scroll.scrollTop = scroll.scrollHeight;
  };

  const activeConversation = useMemo(
    () => conversations.find((conversation) => conversation.conversation_id === activeId),
    [activeId, conversations],
  );
  const { visibleMessages, toolCallsByKey, toolResultsByKey } = useMemo(() => {
    const messages = view?.messages ?? [];
    const calls = new Map<string, ToolCall>();
    const results = new Map<string, ToolResult>();
    const requestKeys = new Set<string>();
    for (const call of view?.tool_calls ?? []) calls.set(`${call.run_id}:${call.id}`, call);
    for (const message of messages) {
      for (const part of message.parts) {
        if (!part.tool_call_id) continue;
        const key = `${message.run_id ?? ""}:${part.tool_call_id}`;
        if (part.type === "tool_call" && message.role === "assistant") requestKeys.add(key);
        if (part.type === "tool_result" && part.result) results.set(key, part.result);
      }
    }
    return {
      visibleMessages: messages.flatMap((message) => {
        if (message.role !== "tool") return [message];
        const parts = message.parts.filter((part) =>
          part.type !== "tool_result" || !part.tool_call_id ||
          !requestKeys.has(`${message.run_id ?? ""}:${part.tool_call_id}`),
        );
        return parts.length ? [{ ...message, parts }] : [];
      }),
      toolCallsByKey: calls,
      toolResultsByKey: results,
    };
  }, [view?.messages, view?.tool_calls]);
  const environmentLogsByMessage = useMemo(() => {
    const grouped = new Map<string, RuntimeEvent[]>();
    for (const event of view?.environment_events ?? []) {
      if (!event.inbound_message_id || !event.environment_progress) continue;
      const entries = grouped.get(event.inbound_message_id) ?? [];
      entries.push(event);
      grouped.set(event.inbound_message_id, entries);
    }
    return grouped;
  }, [view?.environment_events]);

  const createNewConversation = async () => {
    setCreating(true);
    setError("");
    try {
      const options = newEnvironment === "e2b" && newSource === "git"
        ? {
            environment: newEnvironment,
            git_repository: newGitRepository.trim(),
            git_revision: newGitBase === "branch"
              ? `refs/heads/${newGitBranch.trim()}`
              : newGitRevision.trim(),
          }
        : { environment: newEnvironment, workspace: newWorkspace.trim() };
      const conversation = await createConversation(options);
      setConversations((current) => [conversation, ...current]);
      setActiveId(conversation.conversation_id);
    } catch (cause) {
      setError(cause instanceof Error ? cause.message : "Could not create conversation");
    } finally {
      setCreating(false);
    }
  };

  const sendMessage = async () => {
    const text = composer.trim();
    if (!activeId || !text || sending) return;
    const pending =
      pendingSubmission.current?.text === text
        ? pendingSubmission.current
        : { text, key: newIdempotencyKey() };
    pendingSubmission.current = pending;
    setSending(true);
    setError("");
    try {
      await submitMessage(activeId, text, pending.key);
      pendingSubmission.current = null;
      setComposer("");
      jumpToLatest();
    } catch (cause) {
      setError(`${cause instanceof Error ? cause.message : "Could not submit message"}. Retry is safe.`);
    } finally {
      setSending(false);
    }
  };

  const handleSubmit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    void sendMessage();
  };

  const handleComposerKeyDown = (event: KeyboardEvent<HTMLTextAreaElement>) => {
    if (event.key === "Enter" && !event.shiftKey) {
      event.preventDefault();
      void sendMessage();
    }
  };

  const statusLabel = {
    idle: "Offline",
    loading: "Loading",
    connected: "Live",
    reconnecting: "Reconnecting",
    error: "Unavailable",
  }[connection];
  const runStatus = view?.conversation.conversation_id === activeId ? view.active_run?.status : null;
  const runStage = runStatus === "running"
    ? [...(view?.environment_events ?? [])].reverse().find((event) => event.run_id === view?.active_run?.id)?.environment_progress?.message
    : null;
  const gitSourceSelected = newEnvironment === "e2b" && newSource === "git";
  const canCreateConversation = gitSourceSelected
    ? newGitRepository.trim().length > 0 && (newGitBase === "branch"
      ? newGitBranch.trim().length > 0
      : /^[0-9a-fA-F]{40}$/.test(newGitRevision.trim()))
    : newWorkspace.trim().length > 0;

  return (
    <div className="app-shell">
      <aside className="sidebar" aria-label="Conversations">
        <div className="brand-row">
          <div>
            <p className="eyebrow">local agent runtime</p>
            <h1>pons</h1>
          </div>
          <span className={`status-dot status-dot--${connection}`} title={statusLabel} aria-label={statusLabel} />
        </div>

        <div className="new-session-controls">
          <label className="environment-picker" htmlFor="new-environment">
            <span>Sandbox for new sessions</span>
            <select
              id="new-environment"
              value={newEnvironment}
              onChange={(event) => {
                const environment = event.target.value;
                setNewEnvironment(environment);
                setNewSource(environment === "e2b" ? "git" : "workspace");
              }}
              disabled={creating}
            >
              {runtimeOptions.environments.map((option) => (
                <option key={option} value={option}>
                  {environmentLabel(option)}
                </option>
              ))}
            </select>
          </label>
          {newEnvironment === "e2b" && (
            <label className="environment-picker" htmlFor="new-source">
              <span>Workspace source</span>
              <select
                id="new-source"
                value={newSource}
                onChange={(event) => setNewSource(event.target.value as "workspace" | "git")}
                disabled={creating}
              >
                <option value="git">Git repository</option>
                <option value="workspace">Host directory upload</option>
              </select>
            </label>
          )}
          {gitSourceSelected ? (
            <>
              <label className="environment-picker" htmlFor="new-git-repository">
                <span>Git repository URL</span>
                <input
                  id="new-git-repository"
                  type="url"
                  value={newGitRepository}
                  onChange={(event) => setNewGitRepository(event.target.value)}
                  placeholder="https://github.com/owner/repo.git"
                  disabled={creating}
                />
              </label>
              <label className="environment-picker" htmlFor="new-git-base">
                <span>Start from</span>
                <select
                  id="new-git-base"
                  value={newGitBase}
                  onChange={(event) => setNewGitBase(event.target.value as "branch" | "commit")}
                  disabled={creating}
                >
                  <option value="branch">Branch</option>
                  <option value="commit">Commit SHA</option>
                </select>
              </label>
              {newGitBase === "branch" ? (
                <label className="environment-picker" htmlFor="new-git-branch">
                  <span>Branch name</span>
                  <input
                    id="new-git-branch"
                    type="text"
                    value={newGitBranch}
                    onChange={(event) => setNewGitBranch(event.target.value)}
                    placeholder="main"
                    disabled={creating}
                  />
                </label>
              ) : (
                <label className="environment-picker" htmlFor="new-git-revision">
                  <span>Base commit SHA</span>
                  <input
                    id="new-git-revision"
                    type="text"
                    value={newGitRevision}
                    onChange={(event) => setNewGitRevision(event.target.value)}
                    placeholder="40-character commit ID"
                    disabled={creating}
                  />
                </label>
              )}
              <p className="source-hint">
                {newGitBase === "branch"
                  ? "Pons uses the branch tip when it first sets up the workspace, then works on its own branch."
                  : "Pons creates a separate work branch from this commit."}
              </p>
            </>
          ) : (
            <label className="environment-picker" htmlFor="new-workspace">
              <span>Workspace path</span>
              <input
                id="new-workspace"
                type="text"
                value={newWorkspace}
                onChange={(event) => setNewWorkspace(event.target.value)}
                placeholder="/absolute/path/to/workspace"
                disabled={creating}
              />
            </label>
          )}
          <button className="new-conversation" type="button" onClick={() => void createNewConversation()} disabled={creating || !canCreateConversation}>
            <span aria-hidden="true">+</span>
            {creating ? "Creating…" : "New conversation"}
          </button>
        </div>

        <div className="conversation-list" aria-live="polite">
          {listLoading && <p className="muted sidebar-message">Loading conversations…</p>}
          {!listLoading && conversations.length === 0 && (
            <p className="muted sidebar-message">No conversations yet.</p>
          )}
          {conversations.map((conversation) => (
            <button
              className={`conversation-item ${conversation.conversation_id === activeId ? "is-active" : ""}`}
              key={conversation.conversation_id}
              type="button"
              onClick={() => setActiveId(conversation.conversation_id)}
            >
              <span className="conversation-item-top">
                <span className="conversation-title">Conversation {shortId(conversation.conversation_id)}</span>
                {conversation.conversation_id === activeId && runStatus && (
                  <span className={`conversation-activity conversation-activity--${runStatus}`}>
                    <span className="activity-dot" aria-hidden="true" />
                    {runStatus === "queued" ? "Queued" : "Running"}
                  </span>
                )}
              </span>
              <span className="conversation-meta">
                {formatDate(conversation.created_at)} · {environmentLabel(conversation.environment || runtimeOptions.default_environment)}
              </span>
            </button>
          ))}
        </div>

        <div className="sidebar-footer">
          <span className="connection-pill">{statusLabel}</span>
          <span className="muted">SSE runtime</span>
        </div>
      </aside>

      <main className="main-panel">
        <header className="conversation-header">
          <div>
            <p className="eyebrow">conversation</p>
            <h2>{activeConversation ? `Conversation ${shortId(activeConversation.conversation_id)}` : "Welcome to pons"}</h2>
          </div>
          <div className="conversation-header-meta">
            {activeConversation && (
              <span className="environment-pill">
                {environmentLabel(activeConversation.environment || runtimeOptions.default_environment)}
              </span>
            )}
            {runStatus && (
              <span className={`run-pill run-pill--${runStatus}`} role="status" aria-live="polite">
                <span className="activity-dot" aria-hidden="true" />
                {runStatus === "queued" ? "Queued" : `Running${runStage ? ` · ${runStage}` : ""}`}
              </span>
            )}
          </div>
        </header>

        {error && (
          <div className="error-banner" role="alert">
            <span>{error}</span>
            <button type="button" onClick={() => setError("")} aria-label="Dismiss error">
              ×
            </button>
          </div>
        )}

        {!activeId ? (
          <section className="empty-state">
            <div className="empty-mark" aria-hidden="true">↗</div>
            <p className="eyebrow">ready when you are</p>
            <h3>Start a conversation</h3>
            <p className="muted">Create a local session and give the agent something to work on.</p>
            <button className="primary-button" type="button" onClick={() => void createNewConversation()} disabled={creating || !canCreateConversation}>
              {creating ? "Creating…" : "Create conversation"}
            </button>
          </section>
        ) : (
          <>
            <section
              className="message-scroll"
              aria-live="polite"
              aria-label="Conversation messages"
              ref={messageScrollRef}
              onScroll={updateChatFollow}
            >
              {connection === "loading" && <p className="muted loading-message">Loading conversation…</p>}
              {view && view.messages.length === 0 && connection !== "loading" && (
                <div className="empty-state empty-state--compact">
                  <div className="empty-mark" aria-hidden="true">✦</div>
                  <h3>What should we work on?</h3>
                  <p className="muted">Ask pons to inspect, change, or explain something in the workspace.</p>
                </div>
              )}
              {visibleMessages.map((message) => (
                <Fragment key={message.id}>
                  <MessageBubble
                    message={message}
                    toolProgress={toolProgress}
                    toolCallsByKey={toolCallsByKey}
                    toolResultsByKey={toolResultsByKey}
                  />
                  {message.role === "user" && environmentLogsByMessage.has(message.id) && (
                    <EnvironmentSetupLog
                      entries={environmentLogsByMessage.get(message.id)!}
                      active={view?.active_run?.inbound_message_id === message.id}
                    />
                  )}
                </Fragment>
              ))}
              {liveDraft && (
                <article className="message message--assistant message--draft">
                  <div className="message-avatar">P</div>
                  <div className="message-body">
                    <div className="message-label">pons <span className="muted">typing</span></div>
                    <div className="message-content">{liveDraft}</div>
                  </div>
                </article>
              )}
            </section>

            {!followingChat && (
              <button className="jump-to-latest" type="button" onClick={jumpToLatest}>
                Jump to latest ↓
              </button>
            )}

            <form className="composer" onSubmit={handleSubmit}>
              <textarea
                aria-label="Message pons"
                placeholder="Ask pons to do something…"
                value={composer}
                onChange={(event) => setComposer(event.target.value)}
                onKeyDown={handleComposerKeyDown}
                rows={1}
                disabled={sending}
              />
              <div className="composer-footer">
                <span className="muted">Enter to send · Shift+Enter for a new line</span>
                <button className="send-button" type="submit" disabled={sending || !composer.trim()}>
                  {sending ? "Sending…" : "Send"}
                  <span aria-hidden="true">↗</span>
                </button>
              </div>
            </form>
          </>
        )}
      </main>
    </div>
  );
}

function EnvironmentSetupLog({ entries, active }: { entries: RuntimeEvent[]; active: boolean }) {
  const [expanded, setExpanded] = useState(active);
  return (
    <details className="setup-log" open={expanded} onToggle={(event) => setExpanded(event.currentTarget.open)}>
      <summary>
        <span className="setup-log-title">Environment setup</span>
        <span className="setup-log-count">{entries.length} {entries.length === 1 ? "step" : "steps"}</span>
        {active && <span className="tool-spinner" aria-hidden="true" />}
      </summary>
      <ol className="setup-log-entries">
        {entries.map((event) => (
          <li key={event.cursor}>
            <time dateTime={event.created_at}>
              {new Intl.DateTimeFormat(undefined, { timeStyle: "medium" }).format(new Date(event.created_at))}
            </time>
            <span>{event.environment_progress?.message}</span>
          </li>
        ))}
      </ol>
    </details>
  );
}

function MessageBubble({
  message, toolProgress, toolCallsByKey, toolResultsByKey,
}: {
  message: Message;
  toolProgress: Record<string, string>;
  toolCallsByKey: Map<string, ToolCall>;
  toolResultsByKey: Map<string, ToolResult>;
}) {
  const role = message.role === "user" ? "user" : message.role === "tool" ? "tool" : "assistant";
  return (
    <article className={`message message--${role}`}>
      <div className="message-avatar">{role === "user" ? "Y" : role === "tool" ? "↳" : "P"}</div>
      <div className="message-body">
        <div className="message-label">
          {role === "user" ? "You" : role === "tool" ? "Tool result" : "pons"}
          {!message.complete && <span className="muted"> · incomplete</span>}
        </div>
        <div className="message-content">
          {message.parts.map((part, index) => (
            <MessagePartView
              key={`${message.id}-${index}`}
              part={part}
              runId={message.run_id ?? ""}
              toolProgress={toolProgress}
              toolCallsByKey={toolCallsByKey}
              toolResultsByKey={toolResultsByKey}
            />
          ))}
        </div>
      </div>
    </article>
  );
}

function MessagePartView({
  part, runId, toolProgress, toolCallsByKey, toolResultsByKey,
}: {
  part: MessagePart;
  runId: string;
  toolProgress: Record<string, string>;
  toolCallsByKey: Map<string, ToolCall>;
  toolResultsByKey: Map<string, ToolResult>;
}) {
  if (part.type === "text") return <p className="text-part">{part.text}</p>;
  if (part.type === "tool_call") {
    const key = `${runId}:${part.tool_call_id ?? ""}`;
    const call = toolCallsByKey.get(key);
    const result = call?.result ?? toolResultsByKey.get(key);
    const progress = toolProgress[key];
    const status = result ? (result.ok ? "completed" : "failed") : call?.status ?? "requested";
    const pending = status === "requested";
    return (
      <details className={`tool-card ${status === "failed" || status === "interrupted" ? "is-error" : ""} ${status === "completed" ? "is-ok" : ""}`}>
        <summary>
          <span className="tool-kind">{part.tool_kind || "tool"}</span>
          <span className="tool-status">
            {pending && <span className="tool-spinner" aria-hidden="true" />}
            <span className="tool-state">{pending && progress ? "running" : status}</span>
          </span>
        </summary>
        <div className="tool-card-section">
          <span className="tool-section-label">Request</span>
          <pre>{formatJson(part.arguments)}</pre>
        </div>
        {pending && progress && <p className="tool-progress">{progress}</p>}
        {result && (
          <div className="tool-card-section">
            <span className="tool-section-label">Result</span>
            <pre>{resultText(result)}</pre>
          </div>
        )}
      </details>
    );
  }
  if (part.result) {
    return (
      <details className={`tool-card ${part.result.ok ? "is-ok" : "is-error"}`}>
        <summary>
          <span className="tool-kind">{part.result.kind || "tool result"}</span>
          <span className="tool-state">{part.result.ok ? "completed" : "failed"}</span>
        </summary>
        <pre>{resultText(part.result)}</pre>
      </details>
    );
  }
  return <p className="muted">{part.text || "Unsupported message part"}</p>;
}

export default App;

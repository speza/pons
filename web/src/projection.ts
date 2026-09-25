import type { ConversationView, RuntimeEvent, ToolCall } from "./types";

// These match the runtime's NoticeFailedText and NoticeStoppedText so live
// replay renders the same message as a snapshot.
const noticeFailedText = "An error occurred while generating a response. Try again.";
const noticeStoppedText = "Stopped. The response was not finished.";

function upsert<T extends { id: string }>(items: T[], item: T): T[] {
  const index = items.findIndex((existing) => existing.id === item.id);
  if (index < 0) return [...items, item];
  const next = [...items];
  next[index] = item;
  return next;
}

function upsertToolCall(items: ToolCall[], item: ToolCall): ToolCall[] {
  const index = items.findIndex((existing) => existing.run_id === item.run_id && existing.id === item.id);
  if (index < 0) return [...items, item];
  const next = [...items];
  next[index] = item;
  return next;
}

export function applyEvent(view: ConversationView, event: RuntimeEvent): ConversationView {
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
    case "input.accepted":
      if (event.input && event.inbound_message_id) {
        next.messages = upsert(next.messages, {
          id: event.inbound_message_id, conversation_id: event.conversation_id,
          inbound_message_id: event.inbound_message_id, role: "user", complete: true,
          parts: event.input.parts, created_at: event.created_at,
        });
        next.submissions = upsert(next.submissions ?? [], {
          id: event.inbound_message_id, conversation_id: event.conversation_id,
          message_id: event.inbound_message_id, agent_revision: event.input.agent_revision,
          status: "queued", accepted_at: event.created_at,
        });
      }
      break;
    case "assistant.output.committed":
      if (event.assistant_output) {
        next.messages = upsert(next.messages, {
          id: event.assistant_output.id, conversation_id: event.conversation_id,
          inbound_message_id: event.inbound_message_id, run_id: event.run_id,
          role: "assistant", complete: true, final: event.assistant_output.final,
          parts: event.assistant_output.parts, created_at: event.created_at,
        });
        for (const part of event.assistant_output.parts) {
          if (part.type !== "tool_call" || !part.tool_call_id) continue;
          next.tool_calls = upsertToolCall(next.tool_calls ?? [], {
            id: part.tool_call_id, conversation_id: event.conversation_id,
            run_id: event.run_id ?? "", inbound_message_id: event.inbound_message_id ?? "",
            kind: part.tool_kind ?? "", arguments: part.arguments ?? {},
            status: "requested", updated_at: event.created_at,
          });
        }
      }
      break;
    case "tool.outcome.recorded":
      if (event.tool_outcome) {
        const outcome = event.tool_outcome;
        next.messages = upsert(next.messages, {
          id: outcome.id, conversation_id: event.conversation_id,
          inbound_message_id: event.inbound_message_id, run_id: event.run_id,
          role: "tool", complete: true, created_at: event.created_at,
          parts: [{ type: "tool_result", tool_call_id: outcome.tool_call_id,
            tool_kind: outcome.tool_kind, result: outcome.result }],
        });
        next.tool_calls = (next.tool_calls ?? []).map((tool) =>
          tool.run_id === event.run_id && tool.id === outcome.tool_call_id
            ? { ...tool, status: outcome.status, result: outcome.result, updated_at: event.created_at }
            : tool);
      }
      break;
    case "run.started":
    case "run.completed":
    case "run.failed":
    case "run.stopped":
      if ((event.type === "run.failed" || event.type === "run.stopped") && event.run && event.run_id) {
        const stopped = event.type === "run.stopped";
        next.messages = upsert(next.messages, {
          id: `${event.run_id}:notice`, conversation_id: event.conversation_id,
          inbound_message_id: event.inbound_message_id, run_id: event.run_id,
          role: "notice", complete: true, created_at: event.created_at,
          notice: { status: stopped ? "stopped" : "failed", error: event.run.error },
          parts: [{ type: "text", text: stopped ? noticeStoppedText : noticeFailedText }],
        });
      }
      next.active_run = event.type === "run.started" ? event.run : undefined;
      if (event.run) {
        const submission = next.submissions?.find((item) => item.id === event.run?.inbound_message_id);
        if (submission) next.submissions = upsert(next.submissions ?? [], {
          ...submission, status: event.run.status, error: event.run.error,
        });
      }
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

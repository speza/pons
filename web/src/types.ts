export interface AgentSummary {
  id: string;
  name?: string;
}

export interface Conversation {
  conversation_id: string;
  agent_id: string;
  workspace: string;
  environment?: string;
  git_repository?: string;
  git_revision?: string;
  created_at: string;
}

export interface RuntimeOptions {
  agent: AgentSummary;
  environments: string[];
  default_environment: string;
}

export interface TextPart {
  type: "text";
  text: string;
}

export interface ToolResult {
  action_id: string;
  ok: boolean;
  output?: string;
  error?: string;
  exit_code?: number;
  kind?: string;
  payload?: unknown;
}

export interface MessagePart {
  type: string;
  text?: string;
  tool_call_id?: string;
  tool_kind?: string;
  arguments?: unknown;
  result?: ToolResult;
}

export interface Message {
  id: string;
  conversation_id: string;
  inbound_message_id?: string;
  run_id?: string;
  role: string;
  parts: MessagePart[];
  complete: boolean;
  final?: boolean;
  created_at: string;
}

export interface UserInput {
  idempotency_key: string;
  parts: TextPart[];
  source: {
    kind: "human" | "system" | "agent";
    adapter: string;
    principal_id?: string;
    source_conversation_id?: string;
    source_agent_id?: string;
  };
  causation_id?: string;
  target_agent_id: string;
  agent_revision: string;
}

export interface AssistantOutput {
  id: string;
  parts: MessagePart[];
  final?: boolean;
}

export interface ToolOutcome {
  id: string;
  tool_call_id: string;
  tool_kind: string;
  status: string;
  result: ToolResult;
}

export interface Run {
  id: string;
  conversation_id: string;
  inbound_message_id: string;
  agent_revision: string;
  status: string;
  error?: string;
  started_at: string;
  completed_at?: string;
}

export interface ToolCall {
  id: string;
  conversation_id: string;
  run_id: string;
  inbound_message_id: string;
  kind: string;
  arguments: unknown;
  status: string;
  result?: ToolResult;
  updated_at: string;
}

export interface Submission {
  id: string;
  conversation_id: string;
  message_id: string;
  agent_revision: string;
  status: string;
  error?: string;
  accepted_at: string;
}

export interface ConversationView {
  conversation: Conversation;
  agent: AgentSummary;
  messages: Message[];
  submissions?: Submission[];
  active_run?: Run;
  tool_calls?: ToolCall[];
  environment_events?: RuntimeEvent[];
  event_cursor: number;
}

export interface TextDelta {
  message_id: string;
  part_id: string;
  text: string;
}

export interface ToolProgress {
  tool_call_id: string;
  text?: string;
}

export interface EnvironmentProgress {
  step: string;
  message: string;
}

export interface RuntimeEvent {
  cursor?: number;
  type: string;
  conversation_id: string;
  run_id?: string;
  inbound_message_id?: string;
  input?: UserInput;
  assistant_output?: AssistantOutput;
  tool_outcome?: ToolOutcome;
  submission?: Submission;
  tool_call?: ToolCall;
  run?: Run;
  delta?: TextDelta;
  progress?: ToolProgress;
  environment_progress?: EnvironmentProgress;
  created_at: string;
}

export interface AcceptedMessage {
  conversation_id: string;
  inbound_message_id: string;
  duplicate?: boolean;
}

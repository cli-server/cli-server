/**
 * Shared types for the contract between agent-server and sidecar.
 */

export interface ToolPayload {
  id: string;
  name: string;
  title: string;
  status: "started" | "completed" | "failed";
  parent_id: string | null;
  input: Record<string, unknown> | null;
  result?: unknown;
  error?: string;
}

export type AgentEvent =
  | { type: "assistant_text"; text: string }
  | { type: "assistant_thinking"; thinking: string }
  | { type: "tool_started"; tool: ToolPayload }
  | { type: "tool_completed"; tool: ToolPayload }
  | { type: "tool_failed"; tool: ToolPayload }
  | { type: "user_text"; text: string }
  | { type: "system"; data: Record<string, unknown> }
  | { type: "prompt_suggestions"; suggestions: string[] }
  | { type: "complete"; usage?: Record<string, unknown>; total_cost_usd?: number }
  | { type: "error"; message: string };

export interface QueryRequest {
  prompt: string;
  sessionId: string;
  resume?: boolean;
}

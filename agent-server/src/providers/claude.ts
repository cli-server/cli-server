import { query, type Message } from "@anthropic-ai/claude-agent-sdk";
import type { AgentEvent, ToolPayload, QueryRequest } from "./types.js";

const MAX_DESC_LEN = 60;

function truncate(s: string, maxLen = MAX_DESC_LEN): string {
  s = s.trim().split("\n", 1)[0].trim();
  if (s.length > maxLen) return s.slice(0, maxLen - 1) + "\u2026";
  return s;
}

function extractToolDescription(
  toolName: string,
  toolInput: Record<string, unknown> | undefined,
): string {
  if (!toolInput) return "";
  const get = (key: string) => {
    const v = toolInput[key];
    return typeof v === "string" && v.trim() ? v : "";
  };

  if (toolName === "Bash" || toolName === "bash")
    return truncate(get("description") || get("command"));
  if (toolName === "Task" || toolName === "task") return truncate(get("description"));
  if (toolName === "Read" || toolName === "read") return truncate(get("file_path"));
  if (toolName === "Write" || toolName === "write") return truncate(get("file_path"));
  if (toolName === "Edit" || toolName === "edit") return truncate(get("file_path"));
  if (toolName === "Glob" || toolName === "glob") return truncate(get("pattern"));
  if (toolName === "Grep" || toolName === "grep") return truncate(get("pattern"));
  if (toolName === "WebFetch" || toolName === "web_fetch") return truncate(get("url"));
  if (toolName === "WebSearch" || toolName === "web_search") return truncate(get("query"));
  if (toolName === "TodoWrite" || toolName === "TaskCreate") return truncate(get("subject"));

  for (const key of ["description", "prompt", "query", "file_path", "pattern", "command"]) {
    const val = get(key);
    if (val) return truncate(val);
  }
  return "";
}

function formatToolTitle(
  toolName: string,
  toolInput: Record<string, unknown> | undefined,
): string {
  let base = toolName;
  if (toolName.startsWith("mcp__")) {
    const parts = toolName.split("__", 3);
    if (parts.length === 3) base = parts[2].replace(/_/g, " ");
  }
  if (!toolInput) return base;
  const desc = extractToolDescription(toolName, toolInput);
  return desc ? `${base}(${desc})` : base;
}

const PROMPT_SUGGESTIONS_RE =
  /<prompt_suggestions>\s*(.*?)\s*<\/prompt_suggestions>/s;
const LOCAL_COMMAND_STDOUT_RE =
  /<local-command-stdout>(.*?)<\/local-command-stdout>/s;

interface ActiveTool {
  id: string;
  name: string;
  title: string;
  parentId: string | null;
  input: Record<string, unknown> | null;
}

/**
 * Active query handle, allowing interruption.
 */
let activeAbortController: AbortController | null = null;

/**
 * Run a Claude Code SDK query and yield AgentEvents as an async generator.
 */
export async function* runQuery(
  req: QueryRequest,
): AsyncGenerator<AgentEvent> {
  const env: Record<string, string> = {};
  if (process.env.ANTHROPIC_API_KEY) env.ANTHROPIC_API_KEY = process.env.ANTHROPIC_API_KEY;
  if (process.env.ANTHROPIC_BASE_URL) env.ANTHROPIC_BASE_URL = process.env.ANTHROPIC_BASE_URL;

  const abortController = new AbortController();
  activeAbortController = abortController;

  const activeTools = new Map<string, ActiveTool>();
  let totalCostUsd = 0;
  let usage: Record<string, unknown> = {};

  try {
    const conversation = query({
      prompt: req.prompt,
      options: {
        allowedTools: ["*"],
        permissionMode: "bypassPermissions",
        cwd: "/home/agent",
        systemPrompt: "claude_code",
        model: process.env.MODEL || undefined,
      },
      ...(req.resume ? { resume: req.sessionId } : {}),
      abortController,
    });

    for await (const message of conversation) {
      if (abortController.signal.aborted) break;

      const events = processMessage(message, activeTools);

      // Accumulate cost/usage from result messages
      if (message.type === "result") {
        const resultMsg = message as Message & {
          cost_usd?: number;
          usage?: Record<string, unknown>;
          costUSD?: number;
        };
        if (resultMsg.cost_usd != null) totalCostUsd += resultMsg.cost_usd;
        if (resultMsg.costUSD != null) totalCostUsd += resultMsg.costUSD;
        if (resultMsg.usage) usage = resultMsg.usage;
      }

      for (const event of events) {
        yield event;
      }
    }

    // Emit completion event
    const completePayload: AgentEvent = {
      type: "complete",
      total_cost_usd: totalCostUsd,
    };
    if (Object.keys(usage).length > 0) {
      (completePayload as { usage: Record<string, unknown> }).usage = usage;
    }
    yield completePayload;
  } catch (err: unknown) {
    if (abortController.signal.aborted) {
      // Interrupted — don't yield error, the interrupt handler sends cancelled
      return;
    }
    const message = err instanceof Error ? err.message : String(err);
    yield { type: "error", message };
  } finally {
    if (activeAbortController === abortController) {
      activeAbortController = null;
    }
  }
}

/**
 * Interrupt the currently running query.
 */
export function interruptQuery(): boolean {
  if (activeAbortController) {
    activeAbortController.abort();
    activeAbortController = null;
    return true;
  }
  return false;
}

/**
 * Process a single SDK message into zero or more AgentEvents.
 */
function processMessage(
  message: Message,
  activeTools: Map<string, ActiveTool>,
): AgentEvent[] {
  const events: AgentEvent[] = [];

  switch (message.type) {
    case "system":
      events.push({ type: "system", data: { subtype: "session_init" } });
      break;

    case "assistant": {
      const content = (message as Message & { content?: unknown[] }).content;
      const parentToolId =
        ((message as Record<string, unknown>).parent_tool_use_id as string) ?? null;
      if (Array.isArray(content)) {
        for (const block of content) {
          events.push(...processBlock(block, parentToolId, activeTools));
        }
      }
      break;
    }

    case "user": {
      const content = (message as Message & { content?: unknown[] }).content;
      if (Array.isArray(content)) {
        for (const block of content) {
          if (isTextBlock(block)) {
            let text = block.text ?? String(block);
            const match = LOCAL_COMMAND_STDOUT_RE.exec(text);
            if (match) text = match[1].trim();
            if (text) events.push({ type: "user_text", text });
          } else if (isToolResultBlock(block)) {
            events.push(...processToolResult(block, activeTools));
          }
        }
      }
      break;
    }

    case "result":
      // Cost/usage accumulated in the caller
      break;
  }

  return events;
}

function processBlock(
  block: unknown,
  parentToolId: string | null,
  activeTools: Map<string, ActiveTool>,
): AgentEvent[] {
  const events: AgentEvent[] = [];

  if (isTextBlock(block)) {
    let text = block.text ?? String(block);

    // Extract prompt suggestions
    const suggestionsMatch = PROMPT_SUGGESTIONS_RE.exec(text);
    if (suggestionsMatch) {
      const raw = suggestionsMatch[1].trim();
      try {
        const suggestions = JSON.parse(raw);
        if (Array.isArray(suggestions)) {
          events.push({ type: "prompt_suggestions", suggestions });
        }
      } catch {
        // ignore
      }
      text = text.replace(PROMPT_SUGGESTIONS_RE, "").trim();
    }

    if (text) events.push({ type: "assistant_text", text });
  } else if (isThinkingBlock(block)) {
    const thinking = block.thinking ?? String(block);
    if (thinking) events.push({ type: "assistant_thinking", thinking });
  } else if (isToolUseBlock(block)) {
    if (!block.id) return events;
    const input =
      typeof block.input === "object" && block.input !== null
        ? { ...(block.input as Record<string, unknown>) }
        : null;
    const tool: ActiveTool = {
      id: block.id,
      name: block.name,
      title: formatToolTitle(block.name, input ?? undefined),
      parentId: parentToolId,
      input,
    };
    activeTools.set(block.id, tool);
    events.push({
      type: "tool_started",
      tool: {
        id: tool.id,
        name: tool.name,
        title: tool.title,
        status: "started",
        parent_id: tool.parentId,
        input: tool.input,
      },
    });
  } else if (isToolResultBlock(block)) {
    events.push(...processToolResult(block, activeTools));
  }

  return events;
}

function processToolResult(
  block: { tool_use_id?: string; is_error?: boolean; content?: unknown },
  activeTools: Map<string, ActiveTool>,
): AgentEvent[] {
  const toolUseId = block.tool_use_id;
  if (!toolUseId) return [];

  const state = activeTools.get(toolUseId) ?? {
    id: toolUseId,
    name: "unknown",
    title: "Unknown tool",
    parentId: null,
    input: null,
  };
  activeTools.delete(toolUseId);

  const isError = block.is_error ?? false;
  const payload: ToolPayload = {
    id: state.id,
    name: state.name,
    title: state.title,
    status: isError ? "failed" : "completed",
    parent_id: state.parentId,
    input: state.input,
  };

  if (isError) {
    payload.error = normalizeResultToString(block.content);
  } else {
    payload.result = normalizeResult(block.content);
  }

  return [{ type: isError ? "tool_failed" : "tool_completed", tool: payload }];
}

function normalizeResult(result: unknown): unknown {
  if (result == null) return null;
  if (Array.isArray(result)) return result.map(normalizeResult);
  if (typeof result === "object") {
    const out: Record<string, unknown> = {};
    for (const [k, v] of Object.entries(result as Record<string, unknown>)) {
      out[k] = normalizeResult(v);
    }
    return out;
  }
  if (typeof result === "string") {
    const text = result.trim();
    if (!text) return "";
    try {
      return JSON.parse(text);
    } catch {
      return text;
    }
  }
  return result;
}

function normalizeResultToString(result: unknown): string {
  if (typeof result === "string") return result;
  try {
    return JSON.stringify(result);
  } catch {
    return String(result);
  }
}

// Type guards for SDK message content blocks
interface TextBlock {
  type: "text";
  text: string;
}
interface ThinkingBlock {
  type: "thinking";
  thinking: string;
}
interface ToolUseBlock {
  type: "tool_use";
  id: string;
  name: string;
  input?: unknown;
}
interface ToolResultBlock {
  type: "tool_result";
  tool_use_id?: string;
  is_error?: boolean;
  content?: unknown;
}

function isTextBlock(block: unknown): block is TextBlock {
  return typeof block === "object" && block !== null && (block as { type: string }).type === "text";
}
function isThinkingBlock(block: unknown): block is ThinkingBlock {
  return typeof block === "object" && block !== null && (block as { type: string }).type === "thinking";
}
function isToolUseBlock(block: unknown): block is ToolUseBlock {
  return typeof block === "object" && block !== null && (block as { type: string }).type === "tool_use";
}
function isToolResultBlock(block: unknown): block is ToolResultBlock {
  return typeof block === "object" && block !== null && (block as { type: string }).type === "tool_result";
}

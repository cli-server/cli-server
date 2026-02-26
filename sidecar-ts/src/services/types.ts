export type StreamEventType =
  | "assistant_text"
  | "assistant_thinking"
  | "tool_started"
  | "tool_completed"
  | "tool_failed"
  | "user_text"
  | "system"
  | "prompt_suggestions";

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

export interface StreamEvent {
  type: StreamEventType;
  text?: string;
  thinking?: string;
  tool?: ToolPayload;
  data?: Record<string, unknown>;
  suggestions?: string[];
}

export interface ChatStreamRequest {
  prompt: string;
  sessionId: string;
  sandboxName: string;
  podIp: string;
  assistantMessageId: string;
}

export interface StreamEnvelope {
  sessionId: string;
  messageId: string;
  streamId: string;
  seq: number;
  kind: string;
  payload: Record<string, unknown>;
  ts: string;
}

export function buildEnvelope(opts: {
  sessionId: string;
  messageId: string;
  streamId: string;
  seq: number;
  kind: string;
  payload?: Record<string, unknown>;
}): StreamEnvelope {
  return {
    sessionId: opts.sessionId,
    messageId: opts.messageId,
    streamId: opts.streamId,
    seq: opts.seq,
    kind: opts.kind,
    payload: opts.payload ?? {},
    ts: new Date().toISOString(),
  };
}

export class StreamSnapshotAccumulator {
  events: Record<string, unknown>[] = [];
  textParts: string[] = [];

  addEvent(kind: string, payload: Record<string, unknown>): void {
    if (kind === "assistant_text") {
      const text = payload.text;
      if (typeof text === "string" && text) {
        this.textParts.push(text);
      }
    }
    this.events.push({ type: kind, ...payload });
  }

  toRender(): Record<string, unknown> {
    return { events: this.events };
  }

  get contentText(): string {
    return this.textParts.join("");
  }
}

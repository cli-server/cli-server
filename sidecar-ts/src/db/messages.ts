import { randomUUID } from "node:crypto";
import { pool } from "./pool.js";

function stripNullBytes(s: string): string {
  return s.replaceAll("\x00", "");
}

export async function createMessage(
  sessionId: string,
  content: string,
  role: string,
): Promise<string> {
  const messageId = randomUUID();
  const streamStatus = role === "assistant" ? "in_progress" : "completed";
  await pool.query(
    `INSERT INTO messages (id, session_id, role, content_text, stream_status)
     VALUES ($1, $2, $3, $4, $5)`,
    [messageId, sessionId, role, content, streamStatus],
  );
  return messageId;
}

export async function appendEvent(
  sessionId: string,
  messageId: string,
  streamId: string,
  seq: number,
  eventType: string,
  renderPayload: Record<string, unknown>,
): Promise<void> {
  await pool.query(
    `INSERT INTO message_events (id, session_id, message_id, stream_id, seq, event_type, render_payload)
     VALUES ($1, $2, $3, $4, $5, $6, $7)`,
    [
      randomUUID(),
      sessionId,
      messageId,
      streamId,
      seq,
      eventType,
      stripNullBytes(JSON.stringify(renderPayload)),
    ],
  );
}

export interface BatchEvent {
  session_id: string;
  message_id: string;
  stream_id: string;
  seq: number;
  event_type: string;
  render_payload: Record<string, unknown>;
}

export async function appendEventsBatch(
  events: BatchEvent[],
): Promise<void> {
  if (events.length === 0) return;

  // Build a single multi-row INSERT for efficiency
  const values: unknown[] = [];
  const placeholders: string[] = [];
  let idx = 1;

  for (const evt of events) {
    placeholders.push(
      `($${idx++}, $${idx++}, $${idx++}, $${idx++}, $${idx++}, $${idx++}, $${idx++})`,
    );
    values.push(
      randomUUID(),
      evt.session_id,
      evt.message_id,
      evt.stream_id,
      evt.seq,
      evt.event_type,
      stripNullBytes(JSON.stringify(evt.render_payload)),
    );
  }

  await pool.query(
    `INSERT INTO message_events (id, session_id, message_id, stream_id, seq, event_type, render_payload)
     VALUES ${placeholders.join(", ")}`,
    values,
  );
}

export async function updateMessageSnapshot(
  messageId: string,
  contentText: string,
  contentRender: Record<string, unknown>,
  lastSeq: number,
  streamStatus: string,
  totalCostUsd: number,
): Promise<void> {
  await pool.query(
    `UPDATE messages
     SET content_text = $1,
         content_render = $2,
         last_seq = $3,
         stream_status = $4,
         total_cost_usd = $5
     WHERE id = $6`,
    [
      stripNullBytes(contentText),
      stripNullBytes(JSON.stringify(contentRender)),
      lastSeq,
      streamStatus,
      totalCostUsd,
      messageId,
    ],
  );
}

export async function getNextSeq(sessionId: string): Promise<number> {
  const result = await pool.query(
    `SELECT COALESCE(MAX(seq), 0) + 1 AS next_seq
     FROM message_events
     WHERE session_id = $1`,
    [sessionId],
  );
  return result.rows[0]?.next_seq ?? 1;
}

export async function hasMessages(sessionId: string): Promise<boolean> {
  const result = await pool.query(
    `SELECT 1 FROM messages
     WHERE session_id = $1 AND role = 'assistant' AND stream_status = 'completed'
     LIMIT 1`,
    [sessionId],
  );
  return result.rows.length > 0;
}

export interface StoredEvent {
  id: string;
  session_id: string;
  message_id: string;
  stream_id: string;
  seq: number;
  event_type: string;
  render_payload: Record<string, unknown>;
  created_at: string | null;
}

export async function getEventsAfter(
  sessionId: string,
  afterSeq: number,
): Promise<StoredEvent[]> {
  const result = await pool.query(
    `SELECT id, session_id, message_id, stream_id, seq, event_type, render_payload, created_at
     FROM message_events
     WHERE session_id = $1 AND seq > $2
     ORDER BY seq ASC`,
    [sessionId, afterSeq],
  );

  return result.rows.map((row) => ({
    id: String(row.id),
    session_id: row.session_id,
    message_id: String(row.message_id),
    stream_id: String(row.stream_id),
    seq: row.seq,
    event_type: row.event_type,
    render_payload:
      typeof row.render_payload === "string"
        ? JSON.parse(row.render_payload)
        : row.render_payload,
    created_at: row.created_at ? new Date(row.created_at).toISOString() : null,
  }));
}

export async function getPodIp(sessionId: string): Promise<string | null> {
  const result = await pool.query(
    `SELECT pod_ip FROM sessions WHERE id = $1`,
    [sessionId],
  );
  return result.rows[0]?.pod_ip ?? null;
}

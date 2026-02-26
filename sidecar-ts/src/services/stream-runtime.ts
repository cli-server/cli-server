import { randomUUID } from "node:crypto";
import type Redis from "ioredis";

import {
  appendEvent,
  appendEventsBatch,
  getNextSeq,
  hasMessages,
  updateMessageSnapshot,
  type BatchEvent,
} from "../db/messages.js";
import { REDIS_CHANNEL_PREFIX } from "../redis.js";
import { sessionRegistry } from "./session-registry.js";
import {
  type ChatStreamRequest,
  type StreamEnvelope,
  StreamSnapshotAccumulator,
  buildEnvelope,
} from "./types.js";
import { config } from "../config.js";

const SNAPSHOT_FLUSH_INTERVAL_MS = 200;
const SNAPSHOT_FLUSH_EVENT_COUNT = 24;

interface AgentEvent {
  type: string;
  [key: string]: unknown;
}

interface StreamContext {
  sessionId: string;
  messageId: string;
  streamId: string;
  seq: number;
  snapshot: StreamSnapshotAccumulator;
  cancelled: boolean;
  lastFlushAt: number;
  eventsSinceFlush: number;
  pendingEvents: BatchEvent[];
  totalCostUsd: number;
}

export class ChatStreamRuntime {
  private redis: Redis;
  private activeTasks = new Set<string>();

  constructor(redis: Redis) {
    this.redis = redis;
  }

  startBackgroundChat(request: ChatStreamRequest): void {
    this.activeTasks.add(request.sessionId);
    // Fire and forget — errors are logged, not propagated
    this.executeChat(request)
      .catch((err) => {
        console.error(`Background chat task failed for session ${request.sessionId}:`, err);
      })
      .finally(() => {
        this.activeTasks.delete(request.sessionId);
      });
  }

  private async executeChat(request: ChatStreamRequest): Promise<void> {
    const resume = await hasMessages(request.sessionId);

    const session = sessionRegistry.getOrCreate(
      request.sessionId,
      request.podIp,
      config.agentServerPort,
    );

    const ctx: StreamContext = {
      sessionId: request.sessionId,
      messageId: request.assistantMessageId,
      streamId: randomUUID(),
      seq: await getNextSeq(request.sessionId),
      snapshot: new StreamSnapshotAccumulator(),
      cancelled: false,
      lastFlushAt: Date.now(),
      eventsSinceFlush: 0,
      pendingEvents: [],
      totalCostUsd: 0,
    };

    const abortController = new AbortController();
    sessionRegistry.setAbortController(request.sessionId, abortController);

    try {
      const agentUrl = `http://${request.podIp}:${config.agentServerPort}/query`;
      const response = await fetch(agentUrl, {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({
          prompt: request.prompt,
          sessionId: request.sessionId,
          resume,
        }),
        signal: abortController.signal,
      });

      if (!response.ok) {
        const text = await response.text();
        throw new Error(`Agent server responded ${response.status}: ${text}`);
      }

      if (!response.body) {
        throw new Error("Agent server response has no body");
      }

      // Parse NDJSON stream
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";

      while (true) {
        const { done, value } = await reader.read();
        if (done) break;

        buffer += decoder.decode(value, { stream: true });
        const lines = buffer.split("\n");
        buffer = lines.pop() ?? "";

        for (const line of lines) {
          const trimmed = line.trim();
          if (!trimmed) continue;

          let event: AgentEvent;
          try {
            event = JSON.parse(trimmed);
          } catch {
            console.warn("Invalid JSON from agent server:", trimmed);
            continue;
          }

          // Handle terminal events from the agent server
          if (event.type === "complete") {
            ctx.totalCostUsd = (event.total_cost_usd as number) ?? 0;
            const payload: Record<string, unknown> = {
              total_cost_usd: ctx.totalCostUsd,
            };
            if (event.usage) payload.usage = event.usage;
            await this.emitEvent("complete", payload, ctx);
            break;
          }

          if (event.type === "error") {
            await this.emitEvent("error", {
              message: (event.message as string) ?? "Unknown error",
            }, ctx);
            break;
          }

          // Normal streaming events — extract kind and payload
          const { type: kind, ...payload } = event;
          await this.emitEvent(kind, payload as Record<string, unknown>, ctx);
        }
      }

      // Process any remaining buffer
      if (buffer.trim()) {
        try {
          const event: AgentEvent = JSON.parse(buffer.trim());
          if (event.type === "complete") {
            ctx.totalCostUsd = (event.total_cost_usd as number) ?? 0;
            const payload: Record<string, unknown> = {
              total_cost_usd: ctx.totalCostUsd,
            };
            if (event.usage) payload.usage = event.usage;
            await this.emitEvent("complete", payload, ctx);
          } else if (event.type !== "error") {
            const { type: kind, ...payload } = event;
            await this.emitEvent(kind, payload as Record<string, unknown>, ctx);
          }
        } catch {
          // ignore trailing incomplete data
        }
      }
    } catch (err: unknown) {
      if (abortController.signal.aborted) {
        console.log(`Stream cancelled for session ${request.sessionId}`);
        ctx.cancelled = true;
        await this.emitEvent("cancelled", {}, ctx);
      } else {
        console.error(`Error during streaming for session ${request.sessionId}:`, err);
        const message = err instanceof Error ? err.message : String(err);
        await this.emitEvent("error", { message }, ctx);
      }
    } finally {
      sessionRegistry.clearAbortController(request.sessionId);
      await this.flushSnapshot(ctx, true);
    }
  }

  private async emitEvent(
    kind: string,
    payload: Record<string, unknown>,
    ctx: StreamContext,
  ): Promise<void> {
    const seq = ctx.seq;
    ctx.seq += 1;

    ctx.snapshot.addEvent(kind, payload);

    const envelope = buildEnvelope({
      sessionId: ctx.sessionId,
      messageId: ctx.messageId,
      streamId: ctx.streamId,
      seq,
      kind,
      payload,
    });

    ctx.pendingEvents.push({
      session_id: ctx.sessionId,
      message_id: ctx.messageId,
      stream_id: ctx.streamId,
      seq,
      event_type: kind,
      render_payload: payload,
    });

    // Publish to Redis for live subscribers
    const channel = `${REDIS_CHANNEL_PREFIX}${ctx.sessionId}`;
    try {
      await this.redis.publish(channel, JSON.stringify(envelope));
    } catch (err) {
      console.warn("Failed to publish event to Redis:", err);
    }

    ctx.eventsSinceFlush += 1;

    // Throttled flush to DB
    const now = Date.now();
    if (
      ctx.eventsSinceFlush >= SNAPSHOT_FLUSH_EVENT_COUNT ||
      now - ctx.lastFlushAt >= SNAPSHOT_FLUSH_INTERVAL_MS
    ) {
      await this.flushSnapshot(ctx, false);
    }
  }

  private async flushSnapshot(
    ctx: StreamContext,
    force: boolean,
  ): Promise<void> {
    if (ctx.pendingEvents.length === 0 && !force) return;

    // Batch insert pending events
    if (ctx.pendingEvents.length > 0) {
      try {
        await appendEventsBatch(ctx.pendingEvents);
      } catch (err) {
        console.error("Failed to batch insert events:", err);
        // Fall back to individual inserts
        for (const evt of ctx.pendingEvents) {
          try {
            await appendEvent(
              evt.session_id,
              evt.message_id,
              evt.stream_id,
              evt.seq,
              evt.event_type,
              evt.render_payload,
            );
          } catch (innerErr) {
            console.error("Failed to insert event:", innerErr);
          }
        }
      }
      ctx.pendingEvents = [];
    }

    // Determine stream status
    let streamStatus = "in_progress";
    if (force) {
      streamStatus = ctx.cancelled ? "interrupted" : "completed";
    }

    try {
      await updateMessageSnapshot(
        ctx.messageId,
        ctx.snapshot.contentText,
        ctx.snapshot.toRender(),
        ctx.seq > 0 ? ctx.seq - 1 : 0,
        streamStatus,
        ctx.totalCostUsd,
      );
    } catch (err) {
      console.error("Failed to update message snapshot:", err);
    }

    ctx.lastFlushAt = Date.now();
    ctx.eventsSinceFlush = 0;
  }
}

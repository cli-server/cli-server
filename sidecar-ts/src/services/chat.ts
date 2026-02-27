import type { RedisClient } from "../redis.js";

import { createMessage, getEventsAfter, getPodIp } from "../db/messages.js";
import { REDIS_CHANNEL_PREFIX, createRedis } from "../redis.js";
import { sessionRegistry } from "./session-registry.js";
import { ChatStreamRuntime } from "./stream-runtime.js";
import type { ChatStreamRequest } from "./types.js";

export class ChatService {
  private redis: RedisClient;
  private runtime: ChatStreamRuntime;

  constructor(redis: RedisClient, runtime: ChatStreamRuntime) {
    this.redis = redis;
    this.runtime = runtime;
  }

  async initiateChatCompletion(
    sessionId: string,
    sandboxName: string,
    podIp: string,
    prompt: string,
  ): Promise<{ message_id: string; session_id: string }> {
    // 1. Persist the user message
    await createMessage(sessionId, prompt, "user");

    // 2. Create a placeholder assistant message
    const assistantMessageId = await createMessage(sessionId, "", "assistant");

    // 3. Build stream request
    const request: ChatStreamRequest = {
      prompt,
      sessionId,
      sandboxName,
      podIp,
      assistantMessageId,
    };

    // 4. Start background streaming task
    this.runtime.startBackgroundChat(request);

    return {
      message_id: assistantMessageId,
      session_id: sessionId,
    };
  }

  async *createEventStream(
    sessionId: string,
    afterSeq = 0,
  ): AsyncGenerator<{ event: string; data: string }> {
    // 1. Replay persisted events from the database
    const backlog = await getEventsAfter(sessionId, afterSeq);
    let maxSeq = afterSeq;

    for (const event of backlog) {
      const seq = event.seq ?? 0;
      if (seq > maxSeq) maxSeq = seq;
      const envelope = {
        sessionId: event.session_id,
        messageId: event.message_id,
        streamId: event.stream_id,
        seq,
        kind: event.event_type,
        payload: event.render_payload,
      };
      yield { event: "stream", data: JSON.stringify(envelope) };
    }

    // 2. Subscribe to live Redis channel for new events
    const channelName = `${REDIS_CHANNEL_PREFIX}${sessionId}`;
    const subscriber = createRedis();
    await subscriber.connect();
    await subscriber.subscribe(channelName);

    try {
      // Create a message queue to bridge the Redis callback to async generator
      const messageQueue: string[] = [];
      let resolve: (() => void) | null = null;
      let done = false;

      subscriber.on("message", (_channel: string, message: string) => {
        messageQueue.push(message);
        if (resolve) {
          resolve();
          resolve = null;
        }
      });

      while (!done) {
        // Wait for a message or timeout (keepalive)
        if (messageQueue.length === 0) {
          const timeoutPromise = new Promise<void>((r) => setTimeout(r, 30000));
          const messagePromise = new Promise<void>((r) => {
            resolve = r;
          });
          await Promise.race([timeoutPromise, messagePromise]);
        }

        if (messageQueue.length === 0) {
          // Timeout — send keepalive
          yield { event: "ping", data: "" };
          continue;
        }

        while (messageQueue.length > 0) {
          const rawData = messageQueue.shift()!;

          let envelope: Record<string, unknown>;
          try {
            envelope = JSON.parse(rawData);
          } catch {
            console.warn("Invalid JSON from Redis pubsub:", rawData);
            continue;
          }

          const eventSeq = (envelope.seq as number) ?? 0;
          if (eventSeq <= maxSeq) continue; // Skip already replayed events
          maxSeq = eventSeq;

          yield { event: "stream", data: JSON.stringify(envelope) };

          // If this is a terminal event, close the stream
          const kind = envelope.kind as string;
          if (kind === "complete" || kind === "cancelled" || kind === "error") {
            done = true;
            break;
          }
        }
      }
    } finally {
      await subscriber.unsubscribe(channelName);
      subscriber.disconnect();
    }
  }

  async stopStream(sessionId: string): Promise<void> {
    await sessionRegistry.cancelGeneration(sessionId);
  }
}

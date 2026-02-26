import { Router, type Request, type Response } from "express";
import type { ChatService } from "./services/chat.js";
import { getPodIp } from "./db/messages.js";

export function createRoutes(chatService: ChatService): Router {
  const router = Router();

  router.get("/health", (_req: Request, res: Response) => {
    res.json({ status: "ok" });
  });

  router.post("/chat", async (req: Request, res: Response) => {
    const sessionId = req.headers["x-session-id"] as string;
    const sandboxName = (req.headers["x-sandbox-name"] as string) ?? "";
    const podIp = (req.headers["x-pod-ip"] as string) ?? "";

    const { prompt } = req.body as { prompt?: string };
    if (!prompt) {
      res.status(400).json({ error: "prompt is required" });
      return;
    }

    // Resolve pod IP: header > DB lookup
    let resolvedPodIp = podIp;
    if (!resolvedPodIp && sessionId) {
      resolvedPodIp = (await getPodIp(sessionId)) ?? "";
    }
    if (!resolvedPodIp) {
      res.status(400).json({ error: "pod IP not available" });
      return;
    }

    try {
      const result = await chatService.initiateChatCompletion(
        sessionId,
        sandboxName,
        resolvedPodIp,
        prompt,
      );
      res.json(result);
    } catch (err) {
      console.error("Chat initiation error:", err);
      res.status(500).json({ error: "failed to initiate chat" });
    }
  });

  router.get("/stream/:sessionId", async (req: Request, res: Response) => {
    const { sessionId } = req.params;
    const afterSeq = parseInt(req.query.after_seq as string, 10) || 0;

    res.setHeader("Content-Type", "text/event-stream");
    res.setHeader("Cache-Control", "no-cache");
    res.setHeader("Connection", "keep-alive");
    res.setHeader("X-Accel-Buffering", "no");
    res.flushHeaders();

    try {
      for await (const msg of chatService.createEventStream(sessionId, afterSeq)) {
        if (res.destroyed) break;
        res.write(`event: ${msg.event}\ndata: ${msg.data}\n\n`);
      }
    } catch (err) {
      if (!res.destroyed) {
        console.error("Stream error:", err);
      }
    } finally {
      if (!res.destroyed) {
        res.end();
      }
    }
  });

  router.delete("/stream/:sessionId", async (req: Request, res: Response) => {
    const { sessionId } = req.params;
    try {
      await chatService.stopStream(sessionId);
      res.status(204).end();
    } catch (err) {
      console.error("Stop stream error:", err);
      res.status(500).json({ error: "failed to stop stream" });
    }
  });

  return router;
}

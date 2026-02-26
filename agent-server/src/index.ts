import express from "express";
import { runQuery, interruptQuery } from "./providers/claude.js";
import type { QueryRequest } from "./providers/types.js";

const app = express();
app.use(express.json());

const PORT = parseInt(process.env.AGENT_SERVER_PORT ?? "3000", 10);

/**
 * POST /query
 * Accept { prompt, sessionId, resume? }, return NDJSON stream of AgentEvent.
 */
app.post("/query", async (req, res) => {
  const { prompt, sessionId, resume } = req.body as Partial<QueryRequest>;
  if (!prompt) {
    res.status(400).json({ error: "prompt is required" });
    return;
  }

  res.setHeader("Content-Type", "application/x-ndjson");
  res.setHeader("Transfer-Encoding", "chunked");
  res.setHeader("Cache-Control", "no-cache");
  res.setHeader("Connection", "keep-alive");

  const request: QueryRequest = {
    prompt,
    sessionId: sessionId ?? "default",
    resume,
  };

  try {
    for await (const event of runQuery(request)) {
      if (res.destroyed) break;
      res.write(JSON.stringify(event) + "\n");
    }
  } catch (err: unknown) {
    if (!res.destroyed) {
      const message = err instanceof Error ? err.message : String(err);
      res.write(JSON.stringify({ type: "error", message }) + "\n");
    }
  } finally {
    if (!res.destroyed) {
      res.end();
    }
  }
});

/**
 * POST /interrupt
 * Cancel current generation.
 */
app.post("/interrupt", (_req, res) => {
  const interrupted = interruptQuery();
  res.json({ interrupted });
});

/**
 * GET /health
 * Health check for K8s probes.
 */
app.get("/health", (_req, res) => {
  res.json({ status: "ok" });
});

app.listen(PORT, "0.0.0.0", () => {
  console.log(`Agent server listening on port ${PORT}`);
});

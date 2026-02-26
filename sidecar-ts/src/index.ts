import express from "express";
import { config } from "./config.js";
import { redis } from "./redis.js";
import { sessionRegistry } from "./services/session-registry.js";
import { ChatStreamRuntime } from "./services/stream-runtime.js";
import { ChatService } from "./services/chat.js";
import { createRoutes } from "./routes.js";
import { pool } from "./db/pool.js";

const app = express();
app.use(express.json());

// Initialize services
await redis.connect();
const runtime = new ChatStreamRuntime(redis);
const chatService = new ChatService(redis, runtime);

// Mount routes
app.use(createRoutes(chatService));

// Idle session reaper (every 60s, 5min TTL)
const REAP_INTERVAL_MS = 60_000;
const REAP_TTL_MS = 300_000;
const reapTimer = setInterval(() => {
  sessionRegistry.reapIdle(REAP_TTL_MS);
}, REAP_INTERVAL_MS);

// Graceful shutdown
async function shutdown(): Promise<void> {
  console.log("Shutting down sidecar...");
  clearInterval(reapTimer);
  sessionRegistry.terminateAll();
  redis.disconnect();
  await pool.end();
  process.exit(0);
}

process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);

app.listen(config.port, "0.0.0.0", () => {
  console.log(`Sidecar-TS listening on port ${config.port}`);
});

import Redis from "ioredis";
import { config } from "./config.js";

export const REDIS_CHANNEL_PREFIX = "chat:stream:live:";

export function createRedis(): Redis {
  return new Redis(config.redisUrl, {
    maxRetriesPerRequest: 3,
    lazyConnect: true,
  });
}

export const redis = createRedis();

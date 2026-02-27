import Redis from "ioredis";
import { config } from "./config.js";

export type RedisClient = InstanceType<typeof Redis>;

export const REDIS_CHANNEL_PREFIX = "chat:stream:live:";

export function createRedis(): RedisClient {
  return new Redis(config.redisUrl, {
    maxRetriesPerRequest: 3,
    lazyConnect: true,
  });
}

export const redis = createRedis();

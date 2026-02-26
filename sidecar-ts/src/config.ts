export interface Config {
  port: number;
  databaseUrl: string;
  redisUrl: string;
  anthropicApiKey: string;
  anthropicBaseUrl: string;
  agentImage: string;
  model: string;
  sandboxBackend: string;
  agentServerPort: number;
}

function env(key: string, fallback = ""): string {
  return process.env[key] ?? fallback;
}

function normalizeDatabaseUrl(url: string): string {
  // pg library expects postgresql:// scheme
  if (url.startsWith("postgres://")) {
    url = url.replace("postgres://", "postgresql://");
  }
  // Strip sslmode param (pg uses ssl option instead)
  if (url.includes("?")) {
    const [base, qs] = url.split("?", 2);
    const params = qs
      .split("&")
      .filter((p) => !p.startsWith("sslmode="));
    url = params.length ? `${base}?${params.join("&")}` : base;
  }
  return url;
}

export const config: Config = {
  port: parseInt(env("PORT", "8081"), 10),
  databaseUrl: normalizeDatabaseUrl(env("DATABASE_URL", "postgresql://postgres:postgres@localhost:5432/cli_server")),
  redisUrl: env("REDIS_URL", "redis://localhost:6379"),
  anthropicApiKey: env("ANTHROPIC_API_KEY"),
  anthropicBaseUrl: env("ANTHROPIC_BASE_URL"),
  agentImage: env("AGENT_IMAGE", "cli-server-agent:latest"),
  model: env("MODEL"),
  sandboxBackend: env("SANDBOX_BACKEND", "docker"),
  agentServerPort: parseInt(env("AGENT_SERVER_PORT", "3000"), 10),
};

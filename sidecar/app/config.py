from pydantic import field_validator
from pydantic_settings import BaseSettings


class Settings(BaseSettings):
    DATABASE_URL: str = "postgresql+asyncpg://postgres:postgres@localhost:5432/cli_server"
    REDIS_URL: str = "redis://localhost:6379"
    ANTHROPIC_API_KEY: str = ""
    ANTHROPIC_BASE_URL: str = ""
    AGENT_IMAGE: str = "cli-server-agent:latest"
    MODEL: str = ""
    SANDBOX_BACKEND: str = "docker"

    model_config = {"env_prefix": "", "case_sensitive": True}

    @field_validator("DATABASE_URL", mode="before")
    @classmethod
    def normalize_database_url(cls, v: str) -> str:
        if v.startswith("postgres://"):
            v = v.replace("postgres://", "postgresql+asyncpg://", 1)
        elif v.startswith("postgresql://"):
            v = v.replace("postgresql://", "postgresql+asyncpg://", 1)
        # asyncpg doesn't understand sslmode; strip it
        if "?" in v:
            base, qs = v.split("?", 1)
            params = [p for p in qs.split("&") if not p.startswith("sslmode=")]
            v = f"{base}?{'&'.join(params)}" if params else base
        return v


settings = Settings()

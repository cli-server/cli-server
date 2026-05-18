# Notebook image (Plan 1)

`Dockerfile.notebook` at repo root builds a self-contained jupyter image
with `agentserver_sdk` pre-installed. `ctx` is auto-injected into every
kernel via `ipython_startup/00-ctx.py`.

## Build

```bash
docker build -f Dockerfile.notebook -t agentserver-notebook:dev .
```

## Run (against a stub gateway)

See `docker-compose.smoke.yml`:

```bash
docker compose -f notebook/docker-compose.smoke.yml up --build
```

Then open <http://localhost:8888/lab>. In a new notebook:

```python
envs = await ctx.envs()
envs   # rendered as table thanks to _repr_html_
```

## Env vars consumed by `ctx = Ctx.from_env()`

| var | default | purpose |
|---|---|---|
| `AGENTSERVER_GATEWAY_URL` | `ws://localhost:8086/notebook/ws` | WS endpoint |
| `AGENTSERVER_WORKSPACE_TOKEN` | (empty) | Bearer for gateway |
| `AGENTSERVER_WORKSPACE_ID` | (empty) | workspace key |
| `AGENTSERVER_USER_ID` | (empty) | attribution only |

# agentserver-sdk

Async Python SDK for agentserver envs. Lets developers operate on workspace envs (executors) from Python — same env-mcp tools the LLM uses, just without going through prompts.

See `docs/superpowers/specs/2026-05-18-python-env-sdk-design.md` for design.

## Install (development)

```
cd sdk/python
pip install -e ".[dev]"
pytest
```

## Usage (in a kernel where ctx is pre-loaded)

```python
envs = await ctx.envs()
alpha = await ctx.env("alpha")
r = await alpha.shell("uname -a")
print(r.stdout)
```

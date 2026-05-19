"""HTTP client for agentserver SDK.

Talks REST to codex-exec-gateway's /api/sdk/* endpoints. One HTTPClient
per Ctx. Bearer-authenticated; the gateway resolves user_id from the
token server-side.
"""

from __future__ import annotations

from typing import Any

import httpx

from .errors import SdkConnectionError, SdkUnauthorized


class HTTPClient:
    def __init__(self, base_url: str, token: str) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self._http = httpx.AsyncClient(
            base_url=self.base_url,
            headers={"Authorization": f"Bearer {token}"},
            timeout=30.0,
        )

    async def post(self, path: str, json: dict[str, Any]) -> dict[str, Any]:
        try:
            r = await self._http.post(path, json=json)
        except httpx.RequestError as e:
            raise SdkConnectionError(f"POST {path}: {e}") from e
        return self._decode(path, r)

    async def get(self, path: str, params: dict[str, Any] | None = None) -> dict[str, Any]:
        try:
            r = await self._http.get(path, params=params)
        except httpx.RequestError as e:
            raise SdkConnectionError(f"GET {path}: {e}") from e
        return self._decode(path, r)

    @staticmethod
    def _decode(path: str, r: httpx.Response) -> dict[str, Any]:
        if r.status_code == 401:
            raise SdkUnauthorized(f"{path}: 401 — {r.text}")
        if r.status_code >= 400:
            raise SdkConnectionError(f"{path}: {r.status_code} — {r.text}")
        try:
            return r.json()
        except Exception as e:
            raise SdkConnectionError(f"{path}: invalid JSON: {e}") from e

    async def close(self) -> None:
        await self._http.aclose()

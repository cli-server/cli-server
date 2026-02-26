from __future__ import annotations

import asyncio
import logging
from contextlib import suppress
from pathlib import Path
from typing import Any

from aiohttp.http import WSMsgType
from claude_agent_sdk._errors import CLIConnectionError, ProcessError
from claude_agent_sdk.types import ClaudeAgentOptions
from kubernetes_asyncio import client, config
from kubernetes_asyncio.stream import WsApiClient
from kubernetes_asyncio.stream.ws_client import (
    ERROR_CHANNEL,
    STDERR_CHANNEL,
    STDOUT_CHANNEL,
)

from app.services.transports.base import BaseSandboxTransport

logger = logging.getLogger(__name__)

NAMESPACE_FILE = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"
DEFAULT_CONTAINER = "agent"


class K8sSandboxTransport(BaseSandboxTransport):
    def __init__(
        self,
        *,
        sandbox_id: str,
        options: ClaudeAgentOptions,
        container: str = DEFAULT_CONTAINER,
        namespace: str | None = None,
    ) -> None:
        super().__init__(sandbox_id=sandbox_id, options=options)
        self._container = container
        self._namespace = namespace
        self._ws_api: WsApiClient | None = None
        self._ws: Any = None
        self._reader_task: asyncio.Task[None] | None = None
        self._error_data: str = ""

    def _get_namespace(self) -> str:
        if self._namespace:
            return self._namespace
        try:
            return Path(NAMESPACE_FILE).read_text().strip()
        except FileNotFoundError:
            return "default"

    async def connect(self) -> None:
        if self._ready:
            return
        self._stdin_closed = False
        self._error_data = ""

        try:
            config.load_incluster_config()
        except config.ConfigException:
            try:
                await config.load_kube_config()
            except config.ConfigException as exc:
                raise CLIConnectionError(
                    f"Failed to load K8s config: {exc}"
                ) from exc

        namespace = self._get_namespace()
        envs, cwd, user = self._prepare_environment()

        # Build a shell command that exports env vars, cd's to cwd, then exec's the CLI
        env_exports = " ".join(
            f"{k}={_shell_quote(v)}" for k, v in envs.items()
        )
        shell_cmd = f"export {env_exports} && cd {_shell_quote(cwd)} && exec {self._build_command()}"
        # K8s exec runs as the container's default user (no user switching),
        # so we invoke bash directly instead of wrapping with su.
        exec_command = ["bash", "-c", shell_cmd]

        try:
            self._ws_api = WsApiClient()
            v1_ws = client.CoreV1Api(api_client=self._ws_api)
            websocket = await v1_ws.connect_get_namespaced_pod_exec(
                self._sandbox_id,
                namespace,
                command=exec_command,
                container=self._container,
                stderr=True,
                stdin=True,
                stdout=True,
                tty=False,
                _preload_content=False,
            )
            self._ws = await websocket.__aenter__()
        except Exception as exc:
            if self._ws_api:
                with suppress(Exception):
                    await self._ws_api.close()
                self._ws_api = None
            raise CLIConnectionError(
                f"Failed to exec into pod {self._sandbox_id}: {exc}"
            ) from exc

        loop = asyncio.get_running_loop()
        self._reader_task = loop.create_task(self._read_stream_data())
        self._monitor_task = loop.create_task(self._monitor_process())
        self._ready = True

    def _is_connection_ready(self) -> bool:
        return self._ws is not None

    async def _read_stream_data(self) -> None:
        try:
            async for raw_msg in self._ws:
                if raw_msg.type in (
                    WSMsgType.CLOSE,
                    WSMsgType.CLOSING,
                    WSMsgType.CLOSED,
                ):
                    break

                if raw_msg.type == WSMsgType.ERROR:
                    logger.error("WebSocket error: %s", raw_msg.data)
                    break

                if raw_msg.type != WSMsgType.BINARY:
                    continue

                channel = raw_msg.data[0]
                data = raw_msg.data[1:]
                if not data:
                    continue

                if channel == STDOUT_CHANNEL:
                    await self._stdout_queue.put(
                        data.decode("utf-8", errors="replace")
                    )
                elif channel == STDERR_CHANNEL:
                    if self._options.stderr:
                        try:
                            self._options.stderr(
                                data.decode("utf-8", errors="replace")
                            )
                        except Exception:
                            pass
                elif channel == ERROR_CHANNEL:
                    self._error_data += data.decode("utf-8", errors="replace")
        except asyncio.CancelledError:
            pass
        except Exception as e:
            logger.error("K8s stream reader error: %s", e)
        finally:
            await self._put_sentinel()

    async def _monitor_process(self) -> None:
        try:
            # Wait for the reader task to finish — it exits when the websocket
            # closes or the command terminates.
            if self._reader_task:
                await asyncio.shield(self._reader_task)

            # Parse the error channel data (contains exit status JSON)
            if self._error_data:
                try:
                    exit_code = WsApiClient.parse_error_data(self._error_data)
                except Exception:
                    exit_code = -1
                if exit_code != 0:
                    self._exit_error = ProcessError(
                        "Claude CLI exited with an error",
                        exit_code=exit_code,
                        stderr="",
                    )
        except asyncio.CancelledError:
            pass
        except Exception as exc:
            self._exit_error = CLIConnectionError(
                f"Claude CLI stopped unexpectedly: {exc}"
            )
        finally:
            self._ready = False

    async def _send_data(self, data: str) -> None:
        if not self._ws:
            raise CLIConnectionError("WebSocket not available")
        stdin_prefix = chr(0)
        await self._ws.send_bytes(
            (stdin_prefix + data).encode("utf-8")
        )

    async def _send_eof(self) -> None:
        if not self._ws:
            return
        # K8s websocket exec does not support closing only stdin.
        # Sending an empty stdin message is the closest equivalent.
        try:
            await self._ws.send_bytes(b"\x00")
        except OSError:
            pass

    async def _cleanup_resources(self) -> None:
        await self._cancel_task(self._reader_task)
        self._reader_task = None

        if self._ws:
            with suppress(Exception):
                await self._ws.close()
            self._ws = None

        if self._ws_api:
            with suppress(Exception):
                await self._ws_api.close()
            self._ws_api = None


def _shell_quote(s: str) -> str:
    """Single-quote a string for safe shell interpolation."""
    return "'" + s.replace("'", "'\\''") + "'"

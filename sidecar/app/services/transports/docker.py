from __future__ import annotations

import asyncio
import logging
from contextlib import suppress
from dataclasses import dataclass
from typing import Any

import aiodocker
from claude_agent_sdk._errors import CLIConnectionError, ProcessError
from claude_agent_sdk.types import ClaudeAgentOptions

from app.services.transports.base import BaseSandboxTransport

logger = logging.getLogger(__name__)


@dataclass
class DockerConfig:
    host: str | None = None


class DockerSandboxTransport(BaseSandboxTransport):
    def __init__(
        self,
        *,
        sandbox_id: str,
        docker_config: DockerConfig,
        options: ClaudeAgentOptions,
    ) -> None:
        super().__init__(sandbox_id=sandbox_id, options=options)
        self._docker_config = docker_config
        self._docker: aiodocker.Docker | None = None
        self._container: Any = None
        self._exec: Any = None
        self._stream: Any = None
        self._reader_task: asyncio.Task[None] | None = None

    def _get_docker(self) -> aiodocker.Docker:
        if self._docker is None:
            try:
                self._docker = aiodocker.Docker(url=self._docker_config.host or None)
            except Exception as e:
                raise CLIConnectionError(f"Failed to connect to Docker: {e}")
        return self._docker

    async def _get_container(self) -> Any:
        try:
            # Use sandbox_id directly - the Go backend already names containers
            # as cli-session-{short_id}
            container = await self._get_docker().containers.get(self._sandbox_id)
            info = await container.show()
            if info["State"]["Status"] != "running":
                await container.start()
            return container
        except Exception as e:
            raise CLIConnectionError(
                f"Failed to connect to sandbox {self._sandbox_id}: {e}"
            )

    async def connect(self) -> None:
        if self._ready:
            return
        self._stdin_closed = False

        self._container = await self._get_container()

        envs, cwd, user = self._prepare_environment()

        try:
            self._exec = await self._container.exec(
                cmd=["bash", "-c", f"exec {self._build_command()}"],
                stdin=True,
                tty=False,
                environment=envs,
                workdir=cwd,
                user=user,
            )
            self._stream = self._exec.start()
            await self._stream._init()
        except Exception as exc:
            raise CLIConnectionError(f"Failed to start Claude CLI: {exc}") from exc

        loop = asyncio.get_running_loop()
        self._reader_task = loop.create_task(self._read_stream_data())
        self._monitor_task = loop.create_task(self._monitor_process())
        self._ready = True

    def _is_connection_ready(self) -> bool:
        return self._stream is not None

    async def _read_stream_data(self) -> None:
        try:
            while True:
                msg = await self._stream.read_out()
                if msg is None:
                    break

                if msg.stream == 1:
                    await self._stdout_queue.put(
                        msg.data.decode("utf-8", errors="replace")
                    )
                elif msg.stream == 2 and self._options.stderr:
                    try:
                        self._options.stderr(msg.data.decode("utf-8", errors="replace"))
                    except Exception:
                        pass
        except asyncio.CancelledError:
            pass
        except Exception as e:
            logger.error("Stream reader error: %s", e)
        finally:
            await self._put_sentinel()

    async def _get_exec_info(self) -> dict[str, Any] | None:
        if not self._exec:
            return None
        try:
            result: dict[str, Any] = await self._exec.inspect()
            return result
        except Exception as e:
            logger.warning("exec_inspect failed: %s", e)
            return None

    async def _kill_exec_process(self) -> None:
        if not self._exec or not self._container:
            return
        try:
            info = await self._get_exec_info()
            pid = info and info.get("Running") and info.get("Pid")
            if not pid:
                return
            await (
                await self._container.exec(
                    cmd=["/bin/kill", "-KILL", f"-{pid}"],
                    user="root",
                )
            ).start(detach=True)
        except Exception as e:
            logger.debug("Failed to kill exec process: %s", e)

    async def _cleanup_resources(self) -> None:
        await self._cancel_task(self._reader_task)
        self._reader_task = None

        await self._kill_exec_process()

        if self._stream:
            with suppress(Exception):
                await self._stream.close()
            self._stream = None

        self._exec = None

        if self._docker:
            with suppress(Exception):
                await self._docker.close()
            self._docker = None

    async def _send_data(self, data: str) -> None:
        if not self._stream:
            raise CLIConnectionError("Stream not available")
        await self._stream.write_in(data.encode("utf-8"))

    async def _send_eof(self) -> None:
        if not self._stream:
            return
        try:
            await self._stream._init()
            resp = self._stream._resp
            transport = resp and resp.connection and resp.connection.transport
            if transport and transport.can_write_eof():
                transport.write_eof()
        except OSError:
            pass

    async def _monitor_process(self) -> None:
        if not self._exec or not self._container:
            return

        try:
            while self._ready:
                await asyncio.sleep(0.5)

                info = await self._get_exec_info()
                if info is None:
                    self._exit_error = CLIConnectionError(
                        "Claude CLI process disappeared"
                    )
                    break

                if not info.get("Running", True):
                    exit_code = info.get("ExitCode", -1)
                    if exit_code != 0:
                        self._exit_error = ProcessError(
                            "Claude CLI exited with an error",
                            exit_code=exit_code,
                            stderr="",
                        )
                    break
        except asyncio.CancelledError:
            pass
        except Exception as exc:
            self._exit_error = CLIConnectionError(
                f"Claude CLI stopped unexpectedly: {exc}"
            )
        finally:
            self._ready = False

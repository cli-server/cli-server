import asyncio
import json
import logging
import re
import shlex
from abc import ABC, abstractmethod
from collections.abc import AsyncIterator
from contextlib import suppress
from dataclasses import asdict
from types import TracebackType
from typing import Any, Self

from claude_agent_sdk._errors import CLIConnectionError, CLIJSONDecodeError
from claude_agent_sdk._internal.transport import Transport
from claude_agent_sdk._version import __version__ as sdk_version
from claude_agent_sdk.types import ClaudeAgentOptions

logger = logging.getLogger(__name__)

TERMINAL_TYPE = "dumb"
DEFAULT_MAX_BUFFER_SIZE = 1024 * 1024 * 10  # 10MB
STDOUT_QUEUE_MAXSIZE = 32
ANSI_ESCAPE_RE = re.compile(r"\x1B\[[0-?]*[ -/]*[@-~]")
JSON_START_RE = re.compile(r"[{\[]")


class BaseSandboxTransport(Transport, ABC):
    _SENTINEL = object()
    _json_decoder = json.JSONDecoder()

    def __init__(
        self,
        *,
        sandbox_id: str,
        options: ClaudeAgentOptions,
    ) -> None:
        self._sandbox_id = sandbox_id
        self._options = options
        self._max_buffer_size = (
            DEFAULT_MAX_BUFFER_SIZE
            if options.max_buffer_size is None
            else options.max_buffer_size
        )
        self._monitor_task: asyncio.Task[None] | None = None
        self._stdout_queue: asyncio.Queue[str | object] = asyncio.Queue(
            maxsize=STDOUT_QUEUE_MAXSIZE
        )
        self._ready = False
        self._exit_error: Exception | None = None
        self._stdin_closed = False

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(
        self,
        exc_type: type[BaseException] | None,
        _exc_val: BaseException | None,
        _exc_tb: TracebackType | None,
    ) -> bool:
        try:
            await self.close()
        except Exception as cleanup_error:
            logger.error(
                f"Error during {self.__class__.__name__} cleanup: {cleanup_error}",
                exc_info=True,
            )
            if exc_type is None:
                raise
        return False

    def _prepare_environment(self) -> tuple[dict[str, str], str, str]:
        envs = {
            "CLAUDE_CODE_ENTRYPOINT": "sdk-py",
            "CLAUDE_AGENT_SDK_VERSION": sdk_version,
            "CLAUDE_CODE_SANDBOX": "1",
            "PYTHONUNBUFFERED": "1",
            "TERM": TERMINAL_TYPE,
            **(self._options.env or {}),
        }
        return (
            envs,
            str(self._options.cwd or "/home/user"),
            self._options.user or "user",
        )

    async def _cancel_task(self, task: asyncio.Task[Any] | None) -> None:
        if task:
            task.cancel()
            with suppress(asyncio.CancelledError):
                await task

    async def _put_sentinel(self) -> None:
        try:
            self._stdout_queue.put_nowait(self._SENTINEL)
        except asyncio.QueueFull:
            pass

    @abstractmethod
    async def connect(self) -> None:
        pass

    @abstractmethod
    async def _cleanup_resources(self) -> None:
        pass

    @abstractmethod
    def _is_connection_ready(self) -> bool:
        pass

    @abstractmethod
    async def _send_data(self, data: str) -> None:
        pass

    @abstractmethod
    async def _send_eof(self) -> None:
        pass

    async def close(self) -> None:
        if self._ready:
            await self.end_input()
        self._ready = False
        await self._cancel_task(self._monitor_task)
        self._monitor_task = None
        await self._cleanup_resources()
        self._stdin_closed = False
        await self._put_sentinel()

    async def write(self, data: str) -> None:
        if not self._ready or not self._is_connection_ready():
            raise CLIConnectionError("Transport is not ready for writing")
        if self._stdin_closed:
            raise CLIConnectionError("Cannot write after input has been closed")
        try:
            await self._send_data(data)
        except CLIConnectionError:
            raise
        except Exception as exc:
            self._exit_error = CLIConnectionError(
                f"Failed to send data to Claude CLI: {exc}"
            )
            raise self._exit_error

    async def end_input(self) -> None:
        if not self._ready or not self._is_connection_ready() or self._stdin_closed:
            return
        try:
            await self._send_eof()
            self._stdin_closed = True
        except (OSError, CLIConnectionError):
            pass

    def read_messages(self) -> AsyncIterator[dict[str, Any]]:
        return self._parse_cli_output()

    def is_ready(self) -> bool:
        return self._ready

    def _build_command(self) -> str:
        cmd = [
            str(self._options.cli_path or "claude"),
            "--output-format",
            "stream-json",
            "--verbose",
        ]

        if isinstance(self._options.system_prompt, str):
            cmd.extend(["--system-prompt", self._options.system_prompt])
        elif isinstance(self._options.system_prompt, dict):
            if (
                self._options.system_prompt.get("type") == "preset"
                and "append" in self._options.system_prompt
            ):
                cmd.extend(
                    ["--append-system-prompt", self._options.system_prompt["append"]]
                )

        if self._options.allowed_tools:
            cmd.extend(["--allowedTools", ",".join(self._options.allowed_tools)])

        if self._options.max_turns:
            cmd.extend(["--max-turns", str(self._options.max_turns)])

        if self._options.disallowed_tools:
            cmd.extend(["--disallowedTools", ",".join(self._options.disallowed_tools)])

        if self._options.model:
            cmd.extend(["--model", self._options.model])

        if self._options.permission_prompt_tool_name:
            cmd.extend(
                ["--permission-prompt-tool", self._options.permission_prompt_tool_name]
            )

        if self._options.permission_mode:
            cmd.extend(["--permission-mode", self._options.permission_mode])

        if self._options.continue_conversation:
            cmd.append("--continue")

        if self._options.resume:
            cmd.extend(["--resume", self._options.resume])

        if self._options.settings:
            cmd.extend(["--settings", self._options.settings])

        if hasattr(self._options, "add_dirs") and self._options.add_dirs:
            for directory in self._options.add_dirs:
                cmd.extend(["--add-dir", str(directory)])

        if self._options.mcp_servers:
            if isinstance(self._options.mcp_servers, dict):
                servers_for_cli = {
                    name: (
                        {k: v for k, v in config.items() if k != "instance"}
                        if isinstance(config, dict) and config.get("type") == "sdk"
                        else config
                    )
                    for name, config in self._options.mcp_servers.items()
                }
                cmd.extend(
                    ["--mcp-config", json.dumps({"mcpServers": servers_for_cli})]
                )
            else:
                cmd.extend(["--mcp-config", str(self._options.mcp_servers)])

        if self._options.include_partial_messages:
            cmd.append("--include-partial-messages")

        if hasattr(self._options, "fork_session") and self._options.fork_session:
            cmd.append("--fork-session")

        if self._options.max_thinking_tokens:
            cmd.extend(
                ["--max-thinking-tokens", str(self._options.max_thinking_tokens)]
            )

        if hasattr(self._options, "agents") and self._options.agents:
            cmd.extend(
                [
                    "--agents",
                    json.dumps(
                        {
                            name: {
                                k: v
                                for k, v in asdict(agent_def).items()
                                if v is not None
                            }
                            for name, agent_def in self._options.agents.items()
                        }
                    ),
                ]
            )

        if hasattr(self._options, "setting_sources") and self._options.setting_sources:
            cmd.extend(
                ["--setting-sources", ",".join(self._options.setting_sources)]
            )

        if hasattr(self._options, "extra_args") and self._options.extra_args:
            for flag, value in self._options.extra_args.items():
                if value is None:
                    cmd.append(f"--{flag}")
                else:
                    cmd.extend([f"--{flag}", str(value)])

        cmd.extend(["--input-format", "stream-json"])
        return shlex.join(cmd)

    def _parse_json_buffer(self, buffer: str) -> tuple[str, list[Any]]:
        messages: list[Any] = []
        while buffer:
            buffer = buffer.lstrip()
            try:
                data, offset = self._json_decoder.raw_decode(buffer)
            except json.JSONDecodeError:
                break
            messages.append(data)
            buffer = buffer[offset:]
        return buffer, messages

    async def _parse_cli_output(self) -> AsyncIterator[dict[str, Any]]:
        if not self._ready and not self._monitor_task:
            raise CLIConnectionError("Transport is not connected")

        json_buffer = ""
        json_started = False

        while True:
            chunk = await self._stdout_queue.get()

            if chunk is self._SENTINEL:
                break
            if not isinstance(chunk, str):
                continue

            clean_chunk = ANSI_ESCAPE_RE.sub("", chunk).replace("\r", "")

            for json_line in clean_chunk.split("\n"):
                json_line = json_line.strip()
                if not json_line:
                    continue

                if not json_started:
                    match = JSON_START_RE.search(json_line)
                    if not match:
                        continue
                    json_line = json_line[match.start():]
                    json_started = True

                json_buffer += json_line
                if len(json_buffer) > self._max_buffer_size:
                    json_buffer = ""
                    raise CLIJSONDecodeError(
                        json_line,
                        ValueError(
                            f"CLI output exceeded max buffer size of {self._max_buffer_size}"
                        ),
                    )

                json_buffer, parsed_messages = self._parse_json_buffer(json_buffer)
                for data in parsed_messages:
                    yield data
                    if isinstance(data, dict) and data.get("type") == "result":
                        json_buffer = ""
                if not json_buffer:
                    json_started = False

        if json_buffer:
            leftover, parsed_messages = self._parse_json_buffer(json_buffer)
            for data in parsed_messages:
                yield data
            if leftover.strip():
                try:
                    json.loads(leftover)
                except json.JSONDecodeError as exc:
                    raise CLIJSONDecodeError(leftover, exc) from exc

        if self._exit_error:
            raise self._exit_error

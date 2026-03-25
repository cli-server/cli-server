/**
 * NanoClaw Process Runner — No-Container Mode Adapter
 *
 * When NANOCLAW_NO_CONTAINER=true (set in K8s Pod environment),
 * NanoClaw's container-runner should spawn the agent-runner as a
 * direct child process instead of inside a Docker container.
 *
 * In K8s Pod mode:
 * - The Pod itself provides isolation (one NanoClaw instance per sandbox)
 * - Docker is not available inside the Pod
 * - ANTHROPIC_BASE_URL already points to agentserver's llmproxy
 * - Group folders are directly accessible on the filesystem
 *
 * This file serves as documentation and a hook point for the
 * container-runner adaptation. The actual patch to container-runner.ts
 * should check process.env.NANOCLAW_NO_CONTAINER at the top of
 * runContainerAgent() and delegate to direct process spawning:
 *
 *   import { spawn } from 'child_process';
 *
 *   if (process.env.NANOCLAW_NO_CONTAINER === 'true') {
 *     // Spawn agent-runner directly as child process
 *     const proc = spawn('node', ['dist/agent-runner/index.js'], {
 *       stdio: ['pipe', 'pipe', 'pipe'],
 *       env: { ...process.env },
 *     });
 *     // Feed input via stdin, parse output markers from stdout
 *     // Same I/O protocol as container mode
 *   }
 *
 * Trade-offs vs container mode:
 * - No per-agent filesystem isolation (all groups share Pod filesystem)
 * - No per-agent resource limits (Pod-level limits apply to all)
 * - No Docker overhead (faster agent startup)
 * - Acceptable because each NanoClaw sandbox is its own Pod
 */

export const NANOCLAW_NO_CONTAINER = process.env.NANOCLAW_NO_CONTAINER === 'true';

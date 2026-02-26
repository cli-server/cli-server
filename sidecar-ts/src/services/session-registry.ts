/**
 * Session registry tracks active agent server connections per sandbox pod.
 *
 * Simplified vs the Python version: no transport/SDK client to manage.
 * We just track which sessions have active HTTP streams to their agent server
 * and provide cancellation support.
 */

interface ActiveSession {
  sessionId: string;
  podIp: string;
  agentPort: number;
  abortController: AbortController | null;
  lastUsedAt: number;
}

export class SessionRegistry {
  private sessions = new Map<string, ActiveSession>();

  getOrCreate(sessionId: string, podIp: string, agentPort: number): ActiveSession {
    let session = this.sessions.get(sessionId);
    if (session && session.podIp !== podIp) {
      // Pod IP changed (resumed to different pod), recreate
      this.sessions.delete(sessionId);
      session = undefined;
    }
    if (!session) {
      session = {
        sessionId,
        podIp,
        agentPort,
        abortController: null,
        lastUsedAt: Date.now(),
      };
      this.sessions.set(sessionId, session);
    }
    session.lastUsedAt = Date.now();
    return session;
  }

  getSession(sessionId: string): ActiveSession | undefined {
    return this.sessions.get(sessionId);
  }

  setAbortController(sessionId: string, controller: AbortController): void {
    const session = this.sessions.get(sessionId);
    if (session) {
      session.abortController = controller;
    }
  }

  clearAbortController(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (session) {
      session.abortController = null;
    }
  }

  async cancelGeneration(sessionId: string): Promise<void> {
    const session = this.sessions.get(sessionId);
    if (!session) return;

    // Abort the HTTP fetch stream
    if (session.abortController) {
      session.abortController.abort();
      session.abortController = null;
    }

    // Tell the agent server to interrupt
    try {
      await fetch(
        `http://${session.podIp}:${session.agentPort}/interrupt`,
        { method: "POST", signal: AbortSignal.timeout(5000) },
      );
    } catch {
      // Best effort — agent server might already be done
    }
  }

  terminate(sessionId: string): void {
    const session = this.sessions.get(sessionId);
    if (session?.abortController) {
      session.abortController.abort();
    }
    this.sessions.delete(sessionId);
  }

  terminateAll(): void {
    for (const [id] of this.sessions) {
      this.terminate(id);
    }
  }

  reapIdle(ttlMs: number): void {
    const now = Date.now();
    const expired: string[] = [];
    for (const [id, session] of this.sessions) {
      if (session.abortController) continue; // Active stream
      if (now - session.lastUsedAt >= ttlMs) {
        expired.push(id);
      }
    }
    for (const id of expired) {
      this.sessions.delete(id);
    }
    if (expired.length > 0) {
      console.log(`Reaped ${expired.length} idle session(s)`);
    }
  }
}

export const sessionRegistry = new SessionRegistry();

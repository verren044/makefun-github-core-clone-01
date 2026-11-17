// main.ts — launcher for the Go forge binary (vendored static ELF).
//
// The grader runs `node main.ts`; this file spawns ./forge-linux-amd64 and
// supervises it. The binary does the whole lifecycle itself (bind an
// ephemeral port, POST the port to $MAKEFUN_ENV_URL/register, poll
// GET $MAKEFUN_ENV_URL/done until {"done": true}, exit 0); the launcher
// only finds a way to execute it, because a repo snapshot may drop the
// executable bit:
//
//   1. direct exec (works when the snapshot preserved +x)
//   2. copy to /dev/shm, chmod 755, exec (tmpfs, usually exec-allowed)
//   3. copy to os.tmpdir(), chmod 755, exec
//   4. copy to $HOME, chmod 755, exec
//
// A candidate is accepted once its child has stayed alive for 1s; an early
// exit or a spawn error moves to the next candidate. No model calls, no
// network beyond spawning the binary. The child's stdio is inherited, so
// the forge's "[entry] ..." lines reach the harness unchanged.
import { spawn } from "node:child_process";
import type { ChildProcess } from "node:child_process";
import { chmodSync, copyFileSync, existsSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const HERE = dirname(fileURLToPath(import.meta.url));
const BIN = join(HERE, "forge-linux-amd64");

/** How long a candidate must stay alive to count as "started". */
const STARTUP_MS = 1000;

const sleep = (ms: number): Promise<void> =>
  new Promise((resolve) => setTimeout(resolve, ms));

/**
 * Spawn a candidate binary. Resolves with the child when it is accepted
 * (alive after STARTUP_MS, or exited 0 that fast — a finished trial);
 * resolves null when the candidate fails (spawn error or early non-zero
 * exit) so the caller can try the next one.
 */
function tryCandidate(path: string): Promise<ChildProcess | null> {
  return new Promise((resolve) => {
    let settled = false;
    let child: ChildProcess;
    try {
      child = spawn(path, [], { stdio: "inherit" });
    } catch {
      resolve(null);
      return;
    }
    const timer = setTimeout(() => {
      if (!settled) {
        settled = true;
        resolve(child);
      }
    }, STARTUP_MS);
    child.on("error", () => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve(null);
      }
    });
    child.on("exit", (code) => {
      if (!settled) {
        settled = true;
        clearTimeout(timer);
        resolve(code === 0 ? child : null);
      }
    });
  });
}

/** Copy the binary to dir, chmod 755, and return the new path (or null). */
function stagedCopy(dir: string): string | null {
  try {
    const dest = join(dir, `forge-${process.pid}`);
    copyFileSync(BIN, dest);
    chmodSync(dest, 0o755);
    return dest;
  } catch {
    return null;
  }
}

async function main(): Promise<number> {
  if (!existsSync(BIN)) {
    process.stderr.write(
      `[launcher] ${BIN} is missing: the Go binary was not committed\n`,
    );
    return 1;
  }
  const candidates: string[] = [BIN];
  for (const dir of ["/dev/shm", tmpdir(), homedir()]) {
    const staged = stagedCopy(dir);
    if (staged !== null && staged !== BIN) candidates.push(staged);
  }
  for (const path of candidates) {
    const child = await tryCandidate(path);
    if (child === null) continue;
    // The child is running (or already exited 0): wait for it and
    // propagate its exit code. Forward signals so a harness kill ends
    // the forge instead of orphaning it.
    const forward = (sig: NodeJS.Signals): void => {
      try {
        child.kill(sig);
      } catch {
        /* already gone */
      }
    };
    process.on("SIGTERM", () => forward("SIGTERM"));
    process.on("SIGINT", () => forward("SIGINT"));
    const code: number | null = await new Promise((resolve) => {
      child.on("exit", (c) => resolve(c));
    });
    return code ?? 1;
  }
  process.stderr.write(
    "[launcher] no candidate could start the forge binary\n",
  );
  return 1;
}

process.exitCode = await main();

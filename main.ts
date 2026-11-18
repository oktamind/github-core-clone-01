// main.ts — the trial entrypoint (Node 22, type-stripping compatible).
// The forge itself is the statically linked Go binary `forge` committed
// beside this file (vendored source, per the kit's runtime rules). This
// launcher only owns the lifecycle the kit requires: spawn the binary,
// read the ephemeral port it prints, register it with the env, long-poll
// /done until the suite finishes, then stop the binary and exit 0.
import { spawn, type ChildProcess } from 'node:child_process';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (!ENV_URL) {
  console.error('[entry] MAKEFUN_ENV_URL is not set');
  process.exit(1);
}

const FORGE_PATH = join(dirname(fileURLToPath(import.meta.url)), 'forge');

function sleep(ms: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, ms));
}

async function main(): Promise<void> {
  const child: ChildProcess = spawn(FORGE_PATH, [], { stdio: ['ignore', 'pipe', 'inherit'] });
  let buf = '';
  const portPromise = new Promise<number>((resolve, reject) => {
    child.stdout?.on('data', (d: Buffer) => {
      buf += d.toString('utf8');
      const m = buf.match(/PORT (\d+)/);
      if (m) resolve(Number(m[1]));
    });
    child.on('error', reject);
    child.on('exit', (code) => reject(new Error(`forge exited early with code ${code}`)));
  });
  const timeout = sleep(10000).then(() => { throw new Error('forge did not print its port in 10s'); });
  const port = await Promise.race([portPromise, timeout]);
  console.log(`[entry] forge listening on port ${port}`);
  const reg = await fetch(`${ENV_URL}/register`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ port }),
  });
  if (!reg.ok) {
    console.error(`[entry] register failed: HTTP ${reg.status}`);
    child.kill('SIGTERM');
    process.exit(1);
  }
  console.log('[entry] registered; waiting for the env to say done');
  for (;;) {
    await sleep(500);
    let done = false;
    try {
      const res = await fetch(`${ENV_URL}/done`);
      const body = (await res.json()) as { done?: boolean };
      done = body.done === true;
    } catch {
      // A transient poll failure must not kill the trial; keep waiting.
    }
    if (done) break;
  }
  console.log('[entry] done');
  child.kill('SIGTERM');
  await new Promise<void>((resolve) => {
    child.on('exit', () => resolve());
    setTimeout(resolve, 3000);
  });
  process.exit(0);
}

main().catch((e: unknown) => {
  console.error('[entry]', e instanceof Error ? e.message : e);
  process.exit(1);
});

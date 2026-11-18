// Entry lifecycle: bind an ephemeral port, register it with the env, then poll
// GET /done until the trial is over and exit 0. The forge itself lives in
// app.ts; this file only drives the harness protocol (wire.ts):
//
//   1. MAKEFUN_ENV_URL names the env origin (http://store:8080 in a trial).
//      Without it there is nothing to talk to, so exit 1 at once.
//   2. listen(0) — the kernel picks a free port. Never bind 8080: the env owns it.
//   3. POST $MAKEFUN_ENV_URL/register {"port": N}. The env records the address
//      the request came from plus N as the base URL and dials it from then on.
//      The first register wins; a 409 means one already won and this process
//      simply keeps waiting.
//   4. Poll GET $MAKEFUN_ENV_URL/done, which answers at once ({"done": false}
//      while the suite runs). A failed or slow poll is retried with a short
//      exponential backoff; the process never exits before done is true.
//   5. Close the server and exit 0. The harness then stops the env, which
//      writes the export the scorer reads.

import { createApp } from './app.ts';

const ENV_URL = (process.env.MAKEFUN_ENV_URL ?? '').replace(/\/+$/, '');
if (ENV_URL === '') {
  process.stderr.write('[entry] MAKEFUN_ENV_URL is not set: there is no env to register with\n');
  process.exit(1);
}

const REQUEST_TIMEOUT_MS = 30_000;
const RETRY_FIRST_MS = 250;
const RETRY_CAP_MS = 2_000;

const sleep = (ms: number): Promise<void> => new Promise((resolve) => setTimeout(resolve, ms));

function backoff(attempt: number): number {
  return Math.min(RETRY_CAP_MS, RETRY_FIRST_MS * 2 ** Math.min(attempt, 8));
}

async function postJson(path: string, body: unknown, timeoutMs: number): Promise<Response> {
  return fetch(`${ENV_URL}${path}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(timeoutMs),
  });
}

// Register the bound port until the env accepts it. 200 is the normal answer
// and 409 (a first register already won) is fine too. 400 and 405 are
// contract errors and end the program; anything else is retried, since the
// env may still be starting.
async function register(port: number): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await postJson('/register', { port }, REQUEST_TIMEOUT_MS);
      if (res.status === 200 || res.status === 409) return;
      if (res.status === 400 || res.status === 405) {
        throw new Error(`register answered HTTP ${res.status}: ${await res.text()}`);
      }
    } catch (err) {
      if (err instanceof Error && err.message.startsWith('register answered HTTP ')) throw err;
    }
    await sleep(backoff(attempt));
  }
}

// Poll GET /done until it answers {"done": true}; never return before that.
async function waitForDone(): Promise<void> {
  for (let attempt = 0; ; attempt += 1) {
    try {
      const res = await fetch(`${ENV_URL}/done`, { signal: AbortSignal.timeout(REQUEST_TIMEOUT_MS) });
      if (res.ok) {
        const body = (await res.json()) as { done?: unknown };
        if (body.done === true) return;
      }
    } catch {
      // The env is not answering yet, or the poll timed out: poll again.
    }
    await sleep(backoff(attempt));
  }
}

async function main(): Promise<void> {
  const server = createApp();
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(0, () => resolve());
  });
  const address = server.address();
  if (address === null || typeof address === 'string') {
    throw new Error('the server did not bind an ephemeral TCP port');
  }
  process.stdout.write(`[entry] listening on port ${address.port}\n`);

  await register(address.port);
  process.stdout.write('[entry] registered; waiting for the env to say done\n');

  await waitForDone();
  server.close();
  process.stdout.write('[entry] done\n');
}

try {
  await main();
  process.exit(0);
} catch (err) {
  process.stderr.write(`[entry] ${err instanceof Error ? err.message : String(err)}\n`);
  process.exit(1);
}

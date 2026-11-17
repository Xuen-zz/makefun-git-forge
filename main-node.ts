import { setTimeout as sleep } from 'node:timers/promises';
import { createApp } from './app.ts';

async function run() {
  const env = process.env.MAKEFUN_ENV_URL?.replace(/\/+$/, '');
  const server = createApp({ token: process.env.MAKEFUN_SHOP_TOKEN ?? process.env.FORGE_TOKEN,
    login: process.env.FORGE_LOGIN, epoch: process.env.MAKEFUN_TRIAL_NOW });
  server.listen(0, env ? '0.0.0.0' : '127.0.0.1');
  await new Promise<void>((resolve, reject) => { server.once('listening', resolve); server.once('error', reject); });
  const address = server.address();
  if (!address || typeof address === 'string') throw new Error('Could not bind the forge to a TCP port');
  console.log(`Forge listening on port ${address.port}`);
  if (!env) return;

  try {
    for (let delay = 100; ; delay = Math.min(2000, delay * 2)) {
      const response = await fetch(`${env}/register`, { method: 'POST', body: JSON.stringify({ port: address.port }),
        headers: { 'Content-Type': 'application/json' }, signal: AbortSignal.timeout(5000) }).catch(() => null);
      if (response) {
        await response.body?.cancel();
        if (response.ok || response.status === 409) break;
        if (response.status === 400 || response.status === 405) throw new Error(`Registration returned HTTP ${response.status}`);
      }
      await sleep(delay);
    }
    for (;;) {
      const response = await fetch(`${env}/done`, { signal: AbortSignal.timeout(25_000) }).catch(() => null);
      if (response?.ok) {
        const status = await response.json().catch(() => null);
        if (status?.done === true) break;
      } else await response?.body?.cancel();
      await sleep(750);
    }
  } finally {
    await new Promise<void>((resolve, reject) => server.close(error => error ? reject(error) : resolve()));
  }
}

try { await run(); }
catch (error) { console.error(error); process.exitCode = 1; }

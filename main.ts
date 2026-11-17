import { fileURLToPath } from 'node:url';

if (process.platform === 'linux' && process.arch === 'x64') {
  if (typeof process.execve !== 'function') {
    throw new Error('This forge needs Node.js 22.15 or later to start its Linux executable');
  }
  const binary = fileURLToPath(new URL('./native/bin/forge-linux-amd64', import.meta.url));
  // Replace Node with the forge so the entry occupies one process for its entire trial.
  process.execve(binary, [binary]);
} else {
  await import('./main-node.ts');
}

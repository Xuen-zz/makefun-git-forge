import { execFileSync } from 'node:child_process';
import { chmodSync, mkdirSync, readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';

const cwd = fileURLToPath(new URL('.', import.meta.url));
const outputDir = fileURLToPath(new URL('./bin/', import.meta.url));
const output = fileURLToPath(new URL('./bin/forge-linux-amd64', import.meta.url));
mkdirSync(outputDir, { recursive: true });

execFileSync(process.env.FORGE_GO ?? 'go', [
  'build', '-mod=readonly', '-trimpath', '-ldflags=-s -w',
  // Keep the hot paths optimized. Cold GraphQL and crypto code use smaller
  // code generation so the static executable stays within the file limit.
  '-gcflags=github.com/graphql-go/graphql/...=-l',
  '-gcflags=crypto/...=-l',
  '-gcflags=crypto/sha1=',
  '-gcflags=crypto/sha256=',
  '-gcflags=crypto/subtle=',
  '-gcflags=github.com/klauspost/compress/zlib=-l',
  '-gcflags=mime/...=-l',
  '-o', output, '.',
], {
  cwd,
  env: { ...process.env, GOTOOLCHAIN: 'local', CGO_ENABLED: '0', GOOS: 'linux', GOARCH: 'amd64', GOAMD64: 'v1' },
  stdio: 'inherit',
});

const image = readFileSync(output);
if (image.length > 8 * 1024 * 1024) throw new Error('Linux executable exceeds 8 MiB');
if (image.readUInt32BE(0) !== 0x7f454c46 || image[4] !== 2 || image[5] !== 1 || image.readUInt16LE(18) !== 62) {
  throw new Error('Linux executable is not an amd64 ELF image');
}
const table = Number(image.readBigUInt64LE(32));
const size = image.readUInt16LE(54);
const count = image.readUInt16LE(56);
for (let i = 0; i < count; i++) {
  const type = image.readUInt32LE(table + i * size);
  if (type === 2 || type === 3) throw new Error('Linux executable is dynamically linked');
}
chmodSync(output, 0o755);
console.log('Static Linux executable: ' + image.length + ' bytes');

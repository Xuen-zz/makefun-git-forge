type Change = { from: number; to: number; insert: string[] };

const lines = (bytes: Buffer) => bytes.toString().match(/[^\n]*\n|[^\n]+$/g) ?? [];

function changes(base: string[], next: string[]): Change[] | null {
  let from = 0, left = base.length, right = next.length;
  while (from < left && from < right && base[from] === next[from]) from++;
  while (from < left && from < right && base[left - 1] === next[right - 1]) { left--; right--; }
  if (from === left || from === right) return from === left && from === right ? [] : [{ from, to: left, insert: next.slice(from, right) }];
  const width = right - from + 1;
  const cells = width * (left - from + 1);
  if (cells > 2_000_000) return null;
  const table = new Uint32Array(cells);
  const at = (i: number, j: number) => (i - from) * width + j - from;
  for (let i = left - 1; i >= from; i--) {
    for (let j = right - 1; j >= from; j--) {
      table[at(i, j)] = base[i] === next[j] ? 1 + table[at(i + 1, j + 1)] : Math.max(table[at(i + 1, j)], table[at(i, j + 1)]);
    }
  }
  const result: Change[] = [];
  let i = from, j = from;
  while (i < left || j < right) {
    if (i < left && j < right && base[i] === next[j]) { i++; j++; continue; }
    const start = i, inserted = j;
    while (i < left || j < right) {
      if (i < left && j < right && base[i] === next[j]) break;
      const remove = j === right || (i < left && table[at(i + 1, j)] >= table[at(i, j + 1)]);
      if (remove) i++;
      else j++;
    }
    result.push({ from: start, to: i, insert: next.slice(inserted, j) });
  }
  return result;
}

/** Merge non-overlapping UTF-8 text edits. A null result denotes an unresolved conflict. */
export function mergeText(base: Buffer, ours: Buffer, theirs: Buffer): Buffer | null {
  if ([base, ours, theirs].some(bytes => bytes.includes(0) || !Buffer.from(bytes.toString()).equals(bytes))) return null;
  const common = lines(base);
  const left = changes(common, lines(ours));
  const right = changes(common, lines(theirs));
  if (!left || !right) return null;
  const output: string[] = [];
  let position = 0, i = 0, j = 0;
  while (i < left.length || j < right.length) {
    const a = left[i], b = right[j];
    let change: Change;
    if (!b || (a && a.from < b.from && a.to <= b.from)) {
      change = a; i++;
    } else if (!a || (b.from < a.from && b.to <= a.from)) {
      change = b; j++;
    } else if (a.from === b.from && a.to === b.to && a.insert.length === b.insert.length && a.insert.every((line, index) => line === b.insert[index])) {
      change = a; i++; j++;
    } else return null;
    for (; position < change.from; position++) output.push(common[position]);
    for (const line of change.insert) output.push(line);
    position = change.to;
  }
  return Buffer.from(output.concat(common.slice(position)).join(''));
}

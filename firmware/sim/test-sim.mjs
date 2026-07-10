// Node driver for the WASM sim probes (A33 W2 negative probes c/d/e).
// Usage: node test-sim.mjs <mjs> <render16.be> <golden.bin>
import { readFileSync } from 'node:fs';

const [, , mjsPath, scriptPath, goldenPath] = process.argv;
const mod = await (await import('file://' + mjsPath)).default();

const reset = () => mod.ccall('sim_reset', null, ['number'], [0]);
const run = (src) => mod.ccall('sim_run', 'number', ['string'], [src]);
const err = () => mod.ccall('sim_error', 'string', [], []);
const fbPtr = () => mod.ccall('sim_fb', 'number', [], []);
const fb = () => mod.HEAPU8.subarray(fbPtr(), fbPtr() + 30000);

let failures = 0;
const ok = (name, cond, extra = '') => {
  console.log(`${cond ? 'PASS' : 'FAIL'}  ${name}${extra ? '  — ' + extra : ''}`);
  if (!cond) failures++;
};

// (c) cross-golden: render16.be byte-identical to the gcc host build
const src = readFileSync(scriptPath, 'utf8');
reset();
const rc = run(src);
const got = Buffer.from(fb());
const golden = readFileSync(goldenPath);
ok('c/cross-golden rc=0', rc === 0, `rc=${rc} ${err()}`);
ok('c/cross-golden 30000 bytes identical to gcc host', got.length === 30000 && got.equals(golden),
   `len=${got.length} equal=${got.equals(golden)}`);

// (d) deadline: `while true end` -> rc=3 within ~100 ms, no hang
reset();
const t0 = performance.now();
const rcLoop = run('while true end');
const dt = performance.now() - t0;
ok('d/deadline while-true rc=3', rcLoop === 3, `rc=${rcLoop} err="${err()}"`);
ok('d/deadline elapsed ~100ms (<400ms, no hang)', dt < 400, `${dt.toFixed(1)} ms`);

// (e) `import os` fails (OS module really disabled by sim conf)
reset();
const rcOs = run('import os');
ok('e/import-os rejected (module disabled, not a -D no-op)', rcOs !== 0, `rc=${rcOs} err="${err()}"`);

console.log(failures === 0 ? '\nALL PROBES GREEN' : `\n${failures} PROBE(S) RED`);
process.exit(failures === 0 ? 0 : 1);

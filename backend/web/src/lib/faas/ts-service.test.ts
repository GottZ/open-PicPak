// ts-service.test.ts — the Masterplan negative probe (a) for W-A33.8, run in node against the SAME
// wrap + contract + type-check path the worker uses (only the lib.d.ts source differs: here they come
// from node_modules instead of the Vite raw-glob). The in-browser squiggle rendering is DOM-bound and
// documented as manual; this pins the logic that produces it.

import { describe, expect, it } from 'vitest'
import ts from 'typescript'
import {
  createDefaultMapFromNodeModules,
  createSystem,
  createVirtualTypeScriptEnvironment,
} from '@typescript/vfs'
import { createFaasEnv, diagnose, FAAS_COMPILER_OPTIONS } from './ts-service-env'

// Shared, expensive-once lib map.
const LIB_MAP = createDefaultMapFromNodeModules(FAAS_COMPILER_OPTIONS, ts)

const CONTRACT_ERR = (code: string) => diagnose(createFaasEnv(new Map(LIB_MAP)), code)

describe('FaaS contract type-checking (negative probe a)', () => {
  it('GREEN with the service: accessing a non-existent ctx member squiggles (TS2339)', () => {
    const code = 'export default async (ctx, cap) => { const w = ctx.width; return { image: null } }'
    const diags = CONTRACT_ERR(code)
    const widthErr = diags.find((d) => /width/.test(d.message))
    expect(widthErr, `expected a contract diagnostic for ctx.width, got: ${JSON.stringify(diags)}`).toBeTruthy()
    // and it points at `width` in the OPERATOR's own coordinates, not the wrapped scaffold's
    expect(code.slice(widthErr!.from, widthErr!.to)).toBe('width')
  })

  it('RED without the service: the same source as a plain module flags nothing on ctx.width', () => {
    // No contract, no wrap → `ctx` is an untyped parameter, so the property access is not an error.
    // This is exactly the pre-W-A33.8 editor: no language service, no squiggle.
    const fsMap = new Map(LIB_MAP)
    fsMap.set('/plain.ts', 'export default async (ctx, cap) => { const w = ctx.width; return { image: null } }')
    const env = createVirtualTypeScriptEnvironment(createSystem(fsMap), ['/plain.ts'], ts, FAAS_COMPILER_OPTIONS)
    const raw = env.languageService.getSemanticDiagnostics('/plain.ts')
    expect(raw.some((d) => d.code === 2339)).toBe(false)
  })

  it('is clean for a valid contract-conforming function (no false positives)', () => {
    const code =
      'export default async (ctx, cap) => {\n' +
      '  const s = ctx.serial\n' +
      '  cap.log("info", s)\n' +
      '  return { image: cap.sharp(ctx.input).resize(400, 300), dither: "floyd" }\n' +
      '}'
    expect(CONTRACT_ERR(code)).toEqual([])
  })

  it('flags a bad-argument-type call into cap (TS2345)', () => {
    const code = 'export default async (ctx, cap) => { cap.log(42, "m"); return { image: null } }'
    const msgs = CONTRACT_ERR(code)
    expect(msgs.some((d) => /not assignable to parameter of type 'string'/.test(d.message))).toBe(true)
  })

  it('honestly does NOT invent a stricter sharp: cap.sharp(42) is legal (input is `unknown`)', () => {
    // runtime.ts types sharp input as `unknown`; the d.ts mirrors that rather than fabricating a signature
    // just to make the Masterplan `cap.sharp(42)` example red. The honest contract error is `ctx.width`.
    const code = 'export default async (ctx, cap) => { return { image: cap.sharp(42) } }'
    expect(CONTRACT_ERR(code)).toEqual([])
  })
})

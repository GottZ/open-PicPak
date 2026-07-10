// contract-surface-golden.test.ts — the d.ts drift anchor (W-A33.8 probe c). The editor's cap/ctx
// contract (contract-dts.ts) must mirror the REAL worker runtime surface (backend/worker/runtime.ts).
// This pins the top-level members of Cap/Ctx/FnResult bidirectionally: a member added on only one side
// turns this red. Pattern mirrors c2-surface-golden.test.ts (reads the real source, extracts, diffs) —
// including the vacuous-green guard so a moved file or drifted syntax cannot silently empty a set.

import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { describe, expect, it } from 'vitest'
import { FAAS_CONTRACT_DTS } from './contract-dts'

const runtimeSrc = readFileSync(
  fileURLToPath(new URL('../../../../worker/runtime.ts', import.meta.url)),
  'utf8',
)

/** Strip line + block comments so identifiers inside prose never look like members. */
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/[^\n]*/g, '')
}

/** Extract the top-level member names of `interface <name> { … }` from a source string. */
function interfaceMembers(rawSrc: string, name: string): string[] {
  const src = stripComments(rawSrc)
  const at = src.indexOf(`interface ${name}`)
  if (at < 0) throw new Error(`interface ${name} not found`)
  const open = src.indexOf('{', at)
  let depth = 0
  let end = -1
  for (let i = open; i < src.length; i++) {
    const c = src[i]
    if (c === '{') depth++
    else if (c === '}' && --depth === 0) {
      end = i
      break
    }
  }
  const body = src.slice(open + 1, end)
  const names: string[] = []
  let d = 0
  let token = ''
  const flush = () => {
    const m = /(\w+)\s*\??\s*[:(]/.exec(token)
    if (m) names.push(m[1])
    token = ''
  }
  for (const c of body) {
    if (c === '{' || c === '(' || c === '<' || c === '[') d++
    else if (c === '}' || c === ')' || c === '>' || c === ']') d--
    if (d === 0 && (c === ';' || c === '\n')) flush()
    else token += c
  }
  flush()
  return [...new Set(names)]
}

// (real runtime interface, d.ts interface, a member that MUST be present — vacuous-green guard)
const PAIRS: Array<[string, string, string]> = [
  ['Ctx', 'FaasCtx', 'serial'],
  ['Cap', 'FaasCap', 'sharp'],
  ['FnResult', 'FaasResult', 'image'],
]

describe('cap/ctx contract d.ts mirrors backend/worker/runtime.ts', () => {
  for (const [realName, dtsName, known] of PAIRS) {
    it(`${dtsName} pins ${realName} bidirectionally (and is non-empty)`, () => {
      const real = interfaceMembers(runtimeSrc, realName).sort()
      const dts = interfaceMembers(FAAS_CONTRACT_DTS, dtsName).sort()
      // vacuous-green guard: extraction actually found members, including a known one
      expect(real.length).toBeGreaterThan(0)
      expect(real).toContain(known)
      expect(dts.length).toBeGreaterThan(0)
      expect(dts).toContain(known)
      // bidirectional pin
      expect(dts).toEqual(real)
    })
  }
})

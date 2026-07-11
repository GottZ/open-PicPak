import { describe, it, expect } from 'vitest'
import {
  onboardUrlDefaults,
  prefilledUrlFields,
  INGEST_TOKEN_PLACEHOLDER,
  DEFAULT_C2_PERIOD_SECONDS,
} from './defaults'

// Probe (a): origin ⇒ the design/26 §2.2 device-URL formats (frame = <origin>/<token>/frame,
// C2 = <origin>/<token>/c2, period 1800), pinned so a path-scheme drift fails red.
describe('onboardUrlDefaults', () => {
  it('derives frame/c2/period from an origin, token as a clearly-marked placeholder', () => {
    const d = onboardUrlDefaults('https://picpak.janetzky.cloud')
    expect(d.frameUrl).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/frame')
    expect(d.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2')
    expect(d.c2PeriodSeconds).toBe('1800')
  })

  it('strips a trailing slash on the origin so joins stay single-slash', () => {
    const d = onboardUrlDefaults('https://host.example/')
    expect(d.frameUrl).toBe('https://host.example/<INGEST_TOKEN>/frame')
    expect(d.c2Url).toBe('https://host.example/<INGEST_TOKEN>/c2')
  })

  it('exposes the token placeholder + 1800 default as stable constants', () => {
    expect(INGEST_TOKEN_PLACEHOLDER).toBe('<INGEST_TOKEN>')
    expect(DEFAULT_C2_PERIOD_SECONDS).toBe('1800')
  })
})

// Probe (b): a blank field is filled; an operator-typed value is NEVER overwritten.
describe('prefilledUrlFields', () => {
  const origin = 'https://picpak.janetzky.cloud'

  it('fills every blank field from the origin defaults', () => {
    const r = prefilledUrlFields({ frameUrl: '', c2Url: '', c2PeriodSeconds: '' }, origin)
    expect(r.frameUrl).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/frame')
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2')
    expect(r.c2PeriodSeconds).toBe('1800')
  })

  it('treats a whitespace-only value as blank', () => {
    const r = prefilledUrlFields({ frameUrl: '   ', c2Url: '\t', c2PeriodSeconds: ' ' }, origin)
    expect(r.frameUrl).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/frame')
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2')
    expect(r.c2PeriodSeconds).toBe('1800')
  })

  it('NEVER overwrites an operator-typed value', () => {
    const typed = {
      frameUrl: 'https://custom.example/tok/frame',
      c2Url: 'https://custom.example/tok/c2',
      c2PeriodSeconds: '600',
    }
    expect(prefilledUrlFields(typed, origin)).toEqual(typed)
  })

  it('mixes: fills the blank field, keeps the typed one', () => {
    const r = prefilledUrlFields(
      { frameUrl: 'https://typed.example/t/frame', c2Url: '', c2PeriodSeconds: '600' },
      origin,
    )
    expect(r.frameUrl).toBe('https://typed.example/t/frame')
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2')
    expect(r.c2PeriodSeconds).toBe('600')
  })

  it('treats an undefined c2PeriodSeconds as blank and fills it', () => {
    const r = prefilledUrlFields({ frameUrl: '', c2Url: '' }, origin)
    expect(r.c2PeriodSeconds).toBe('1800')
  })
})

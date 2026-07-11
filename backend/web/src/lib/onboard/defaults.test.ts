import { describe, it, expect } from 'vitest'
import {
  onboardUrlDefaults,
  prefilledUrlFields,
  prefilledFromEndpoint,
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

// Probe (b): the A35.3b admin path. The endpoint's tokenized URLs fill ONLY blank fields; a typed value is
// never overwritten; an empty/failed payload degrades to the origin placeholder (Endpoint → Placeholder).
describe('prefilledFromEndpoint', () => {
  const origin = 'https://picpak.janetzky.cloud'
  const resp = {
    frame_url: 'https://picpak.janetzky.cloud/S3CR3T/frame',
    c2_url: 'https://picpak.janetzky.cloud/S3CR3T/c2',
    c2_period_s: 1800,
  }

  it('fills blank fields with the real tokenized URLs (token replaces the placeholder)', () => {
    const r = prefilledFromEndpoint({ frameUrl: '', c2Url: '', c2PeriodSeconds: '' }, resp, origin)
    expect(r.frameUrl).toBe('https://picpak.janetzky.cloud/S3CR3T/frame')
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/S3CR3T/c2')
    expect(r.c2PeriodSeconds).toBe('1800') // number payload stringified for the form field
  })

  it('NEVER overwrites an operator-typed value', () => {
    const typed = {
      frameUrl: 'https://custom.example/tok/frame',
      c2Url: 'https://custom.example/tok/c2',
      c2PeriodSeconds: '600',
    }
    expect(prefilledFromEndpoint(typed, resp, origin)).toEqual(typed)
  })

  it('empty payload URLs (feature dark) fall back to the origin placeholder', () => {
    const dark = { frame_url: '', c2_url: '', c2_period_s: '' }
    const r = prefilledFromEndpoint({ frameUrl: '', c2Url: '', c2PeriodSeconds: '' }, dark, origin)
    expect(r.frameUrl).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/frame')
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2')
    expect(r.c2PeriodSeconds).toBe('1800')
  })

  it('mixes: a partial payload fills its field, the placeholder covers the blank one, typed stays', () => {
    const partial = { frame_url: 'https://picpak.janetzky.cloud/S3CR3T/frame', c2_url: '', c2_period_s: 1800 }
    const r = prefilledFromEndpoint(
      { frameUrl: '', c2Url: '', c2PeriodSeconds: '600' },
      partial,
      origin,
    )
    expect(r.frameUrl).toBe('https://picpak.janetzky.cloud/S3CR3T/frame') // from endpoint
    expect(r.c2Url).toBe('https://picpak.janetzky.cloud/<INGEST_TOKEN>/c2') // blank payload → placeholder
    expect(r.c2PeriodSeconds).toBe('600') // typed, never overwritten
  })
})

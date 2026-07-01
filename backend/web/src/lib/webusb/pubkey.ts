// C2 enrollment pubkey transform (Design 26 §2.4 / D26.7, F1). The firmware's `C2 BOND` emits the long-term
// ECDSA P-256 public point as 130 lowercase hex chars (console.c:447); Doc 17 `POST /api/devices` wants it
// base64. Hex → 65 bytes → base64 is the ONLY transform — the private scalar is never present to transform
// (D26.12). The client-side 65-byte/0x04 pre-check gives a legible bench error instead of a server round-trip
// 422 (D26.7); the server re-validates the full P-256 point (c2rekey gate) and stays the real boundary.

export const PUBKEY_LEN = 65 // uncompressed P-256 point: 0x04 || X(32) || Y(32)
export const PUBKEY_HEX_LEN = 130 // 65 bytes as hex

/**
 * Parse the hex field out of a `C2 BOND` success line, e.g. `C2   ecdsa_pubkey(65)=04ab…`. Returns the
 * lowercased hex, or null if the line carries no pubkey field. (matchBond in console.ts owns the OK/ERR
 * decision; this is the field extractor.)
 */
export function parsePubkeyLine(line: string): string | null {
  const m = /ecdsa_pubkey\((\d+)\)=([0-9a-fA-F]+)/.exec(line)
  return m ? m[2].toLowerCase() : null
}

export function hexToBytes(hex: string): Uint8Array {
  if (hex.length % 2 !== 0) throw new Error('pubkey: odd hex length')
  if (!/^[0-9a-fA-F]*$/.test(hex)) throw new Error('pubkey: non-hex characters')
  const out = new Uint8Array(hex.length / 2)
  for (let i = 0; i < out.length; i++) out[i] = parseInt(hex.slice(i * 2, i * 2 + 2), 16)
  return out
}

export function bytesToBase64(bytes: Uint8Array): string {
  let s = ''
  for (const b of bytes) s += String.fromCharCode(b)
  return btoa(s)
}

/**
 * Full F1 transform: a `C2 BOND` pubkey hex → 65 bytes → base64, with the D26.7 pre-check (65 bytes, 0x04
 * uncompressed lead). Throws a legible error on a truncated / non-0x04 / `ERR bond` blob so the SPA never
 * POSTs a bad key.
 */
export function pubkeyHexToBase64(hex: string): string {
  const bytes = hexToBytes(hex)
  if (bytes.length !== PUBKEY_LEN) {
    throw new Error(`pubkey: expected ${PUBKEY_LEN} bytes (${PUBKEY_HEX_LEN} hex), got ${bytes.length}`)
  }
  if (bytes[0] !== 0x04) {
    throw new Error(`pubkey: expected uncompressed 0x04 lead, got 0x${bytes[0].toString(16).padStart(2, '0')}`)
  }
  return bytesToBase64(bytes)
}

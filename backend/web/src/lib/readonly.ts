// Read-only affordance contract (design 19 D19.17, extends D19.6): an admin-only
// mutation control renders DISABLED WITH A REASON — discoverable (a tooltip),
// never silently hidden. The server stays authoritative via requireAdmin
// (design 17 §4.2); this affordance is cosmetic. A live demotion (a whoami
// re-probe / SSE terminal error flipping session.is_admin, which is reactive)
// flips the control without a reload. Every feature surface (A20-A26) spreads
// this onto its mutation buttons identically — one rule, one shape.

export interface MutationAffordance {
  disabled: boolean
  title: string
  'aria-disabled': boolean
}

const NEEDS_ADMIN = 'read-only — needs admin'

/** Affordance attrs for an admin-only control given the live is_admin flag. */
export function mutationAffordance(isAdmin: boolean): MutationAffordance {
  return isAdmin
    ? { disabled: false, title: '', 'aria-disabled': false }
    : { disabled: true, title: NEEDS_ADMIN, 'aria-disabled': true }
}

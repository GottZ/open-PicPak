// Localize an ApiError's stable machine code (api.ts codeFor + the direct
// throws) into human text via the errors.* catalog. The raw server message
// (ApiError.message) stays available separately as the technical detail —
// design A34 §2: the server stays English, the client localizes the code.
import { m } from '../paraglide/messages.js'
import type { ApiError } from './api'

/** Localized text for an ApiError's code; unknown codes get a generic line. */
export function errorText(e: ApiError): string {
  switch (e.code) {
    case 'network':
      return m['errors.network']()
    case 'internal':
      return m['errors.internal']()
    case 'unauthorized':
      return m['errors.unauthorized']()
    case 'bad_request':
      return m['errors.bad_request']()
    case 'forbidden':
      return m['errors.forbidden']()
    case 'not_found':
      return m['errors.not_found']()
    case 'conflict':
      return m['errors.conflict']()
    case 'validation':
      return m['errors.validation']()
    case 'rate_limited':
      return m['errors.rate_limited']()
    case 'server':
      return m['errors.server']()
    case 'api_error':
      return m['errors.api_error']()
    case 'invalid_response':
      return m['errors.invalid_response']()
    default:
      return m['errors.unknown']()
  }
}

// Typed client for the approvald API.
//
// The types come from the same openapi.yml the Go server is generated
// from, so a spec change breaks this client's consumers at compile time
// rather than in production.
//
// Defaults match the Go client in api/client.go deliberately: a 5s
// timeout, one retry rather than five (five can mean five times the
// traffic to something already struggling), and a circuit breaker. Both
// clients behaving the same way is the point - a Node service and a Go
// service calling the same API should fail the same way.
//
//   import createClient from '@christopherscot/approvald-client'
//   const client = createClient({ baseUrl: 'https://approvald' })
//   const { data, error } = await client.GET('/')
import createFetchClient from 'openapi-fetch'

/** Sent on every request, so a server can see which client versions are
 *  still calling it before changing something they depend on.
 *
 *  Read from package.json rather than written here: package.json is the
 *  version a consumer actually installed, so deriving it means the header
 *  cannot disagree with what they have. package.json in turn tracks the
 *  spec's info.version. */
import pkg from './package.json' with { type: 'json' }

export const ClientVersion = pkg.version
export const ClientVersionHeader = 'Client-Version'

// Client-Name carries WHICH service is calling, where Client-Version
// says which version of the spec it was built against. Set it via
// createClient({ name: 'my-service' }).
//
// No X- prefix on either: RFC 6648 deprecated it in 2012. Renamed while
// nothing read the header, which is the only cheap moment to do it.
export const ClientNameHeader = 'Client-Name'

/** Repeat only what is safe to repeat: a POST may already have applied,
 *  so retrying it can create a second thing. */
const IDEMPOTENT = new Set(['GET', 'HEAD', 'OPTIONS', 'PUT', 'DELETE'])

function retryable(req, res, err) {
  if (!IDEMPOTENT.has(req.method)) return false
  if (err !== undefined) return true
  return res !== undefined && (res.status === 429 || res.status >= 500)
}

/** One retry, a second later. The default: it covers the transient
 *  failure - a pod rolling, a connection reset - without showing a
 *  struggling dependency several times its normal traffic. */
export const singleRetry = {
  backoffs: () => [1000],
  retry: retryable,
}

/** Five jittered attempts, for a dependency that is slow rather than
 *  broken. Jitter so a fleet of callers does not retry in lockstep and
 *  arrive at the recovering service as one wave. */
export const exponentialRetry = {
  backoffs: () => {
    const out = []
    let next = 100
    for (let i = 0; i < 5; i++) {
      out.push(next + (Math.random() * 2 - 1) * 0.25 * next)
      next *= 2
    }
    return out
  },
  retry: retryable,
}

/** For a caller that would rather fail fast. */
export const noRetry = { backoffs: () => [], retry: () => false }

/** Reads the server's own answer to "when should I come back".
 *
 *  A client that retries a 429 on its own schedule is the reason rate
 *  limits have to be strict. RFC 9110 allows either a delay in seconds
 *  or an HTTP-date, and servers send both in the wild.
 *
 *  Returns undefined when the header is absent, unparseable, or further
 *  away than maxMs - a server can say 3600, and sleeping an hour inside
 *  a request is indistinguishable from a hang. */
export function retryAfterMs(res, maxMs) {
  const v = res?.headers?.get('retry-after')
  if (!v) return undefined

  let ms
  const secs = Number(v.trim())
  if (Number.isInteger(secs)) {
    if (secs < 0) return undefined
    ms = secs * 1000
  } else {
    const when = Date.parse(v)
    if (Number.isNaN(when)) return undefined
    ms = Math.max(0, when - Date.now())
  }
  return ms > maxMs ? undefined : ms
}

export class CircuitOpenError extends Error {
  constructor() {
    super('circuit open')
    this.name = 'CircuitOpenError'
  }
}

/** Stops calling a dependency that is failing, so a caller fails
 *  immediately instead of queueing behind a timeout it will hit anyway. */
export class Breaker {
  #failures = 0
  #openedAt = 0

  constructor(threshold = 5, cooldownMs = 30_000) {
    this.threshold = threshold
    this.cooldownMs = cooldownMs
  }

  allow() {
    if (this.#failures < this.threshold) return true
    if (Date.now() - this.#openedAt > this.cooldownMs) {
      this.#failures = 0 // half-open: one probe decides whether to close
      return true
    }
    return false
  }

  record(failed) {
    if (!failed) {
      this.#failures = 0
      return
    }
    this.#failures++
    if (this.#failures === this.threshold) this.#openedAt = Date.now()
  }
}

/**
 * Builds the typed client.
 *
 * @param {object} options
 * @param {string} options.baseUrl
 * @param {number} [options.timeoutMs=5000] bounds a single attempt; a
 *   retry gets its own full timeout.
 * @param {object} [options.policy=singleRetry]
 * @param {Breaker} [options.breaker] omit to disable circuit breaking.
 */
export default function createClient({
  baseUrl,
  // The CALLER's own service name, sent as Client-Name so the server
  // can log who asked. Omitted means the server records "unknown",
  // which is honest rather than guessed.
  name,
  timeoutMs = 5000,
  policy = singleRetry,
  breaker,
  // Bounds how long a server's Retry-After can park this client.
  maxRetryAfterMs = 30_000,
}) {
  const resilientFetch = async (input, init) => {
    const req = new Request(input, init)
    req.headers.set(ClientVersionHeader, ClientVersion)
    if (name) req.headers.set(ClientNameHeader, name)

    if (breaker && !breaker.allow()) throw new CircuitOpenError()

    const backoffs = policy.backoffs()
    let res
    let err

    for (let attempt = 0; ; attempt++) {
      err = undefined
      try {
        res = await fetch(req.clone(), { signal: AbortSignal.timeout(timeoutMs) })
      } catch (e) {
        err = e
        res = undefined
      }
      if (attempt >= backoffs.length || !policy.retry(req, res, err)) break

      // The server's Retry-After wins over the policy's backoff: it
      // knows when capacity returns and the client does not.
      const after = retryAfterMs(res, maxRetryAfterMs)
      const wait = after ?? backoffs[attempt]
      await new Promise((r) => setTimeout(r, wait))
    }

    breaker?.record(err !== undefined || (res !== undefined && res.status >= 500))
    if (err !== undefined) throw err
    return res
  }

  return createFetchClient({ baseUrl, fetch: resilientFetch })
}

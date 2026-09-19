import type { Client } from 'openapi-fetch'
import type { paths } from './schema.js'

// The generated schema, re-exported so consumers can name the types the
// API is made of: `components['schemas']['Thing']` for a response body,
// `paths` for a route.
//
// Without this they are in the package but unreachable - a consumer has
// to restate every shape it handles, which is the thing generating a
// client was supposed to prevent, and the restatement drifts silently
// when the spec moves.
export type { paths, components, operations } from './schema.js'

/** The installed package version, which tracks the spec. */
export declare const ClientVersion: string
export declare const ClientVersionHeader: string
export declare const ClientNameHeader: string

export interface RetryPolicy {
  backoffs(): number[]
  retry(req: Request, res: Response | undefined, err: unknown): boolean
}

export declare const singleRetry: RetryPolicy
export declare const exponentialRetry: RetryPolicy
export declare const noRetry: RetryPolicy

export declare class CircuitOpenError extends Error {}

export declare class Breaker {
  constructor(threshold?: number, cooldownMs?: number)
  readonly threshold: number
  readonly cooldownMs: number
  allow(): boolean
  record(failed: boolean): void
}

/** Reads a Retry-After header, in milliseconds. Undefined when absent,
 *  unparseable, or further away than maxMs. */
export declare function retryAfterMs(res: Response | undefined, maxMs: number): number | undefined

export interface ClientOptions {
  baseUrl: string
  /** Bounds a single attempt. Default 5000; a retry gets its own. */
  timeoutMs?: number
  /** Default singleRetry. */
  policy?: RetryPolicy
  /** Omit to disable circuit breaking. */
  breaker?: Breaker
  /** Bounds how long a server's Retry-After can park this client.
   *  Default 30000. */
  maxRetryAfterMs?: number
}

/** The typed client. Paths and response shapes come from openapi.yml, so
 *  calling an endpoint the spec does not describe is a compile error. */
export default function createClient(options: ClientOptions): Client<paths>

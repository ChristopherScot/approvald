# approvald

One-tap approval endpoint for commands that need a human in the loop.

## Why

A wrapper on the Mac gates a command behind an approval sent to Chris's
phone. The wrapper must be able to ask for approval and learn the answer,
but it must not be able to answer its own question — otherwise anything
running on that Mac could self-approve and the gate is decorative.

So the Mac's ntfy token is write-only on `approvals-req` and read-only on
`approvals-resp`. Only this service can publish to `approvals-resp`, and it
runs in the cluster.

## Flow

1. Mac `POST /register` with a nonce → gets a one-time capability token and
   the tap URLs that embed it.
2. Mac publishes the request to `approvals-req` with those URLs as ntfy
   action buttons.
3. Chris taps. This service verifies the token, records the decision, and
   publishes `approve <nonce>` / `deny <nonce>` to `approvals-resp`.
4. Mac, polling `approvals-resp`, sees the decision.

The tap URL is the credential: unguessable, single-use, and it expires with
the request. A leaked one grants a single approval rather than all of them —
which is why this is not a login session.

## Endpoints

| Route | Auth | Purpose |
|---|---|---|
| `POST /register` | `Authorization: Bearer $REGISTER_TOKEN` | Register a nonce, mint tap URLs |
| `GET /d/{nonce}/{token}/{verb}` | the token in the path | Record + publish a decision |
| `GET /healthz` | none | Liveness |

`verb` is `approve` or `deny`.

## Configuration

All required:

| Env | Meaning |
|---|---|
| `BASE_URL` | Public base URL, used to build tap links |
| `NTFY_URL` | ntfy base URL (in-cluster service) |
| `NTFY_TOKEN` | Token that can write the response topic |
| `RESPONSE_TOPIC` | Topic decisions are published to |
| `REGISTER_TOKEN` | Shared secret the Mac uses to register |

## Behaviour worth knowing

- Unregistered nonces are rejected, so reaching the endpoint is not by
  itself enough to manufacture an approval.
- First decision wins. A second tap reports "already decided" and publishes
  nothing.
- Undecided requests expire after 10 minutes; decided ones are retained 30
  minutes so replays are rejected rather than re-published.
- Decision pages are `Cache-Control: no-store` so a prefetching client
  cannot approve on someone's behalf.
- A decision whose publish failed stays retriable: tapping again retries
  rather than reporting "already decided". The decision itself never
  changes once set.
- Request logging deliberately records the matched route pattern, never the
  raw URI - the URI contains a live capability token.
- Unknown nonces and bad tokens return an identical response, so a public
  caller cannot use it to enumerate which nonces exist.

## Operational constraints

**Single replica, in-memory state.** Pending approvals live in process
memory, so two replicas would each see only their own and roughly half of
all taps would 404. The Deployment pins `replicas: 1` with
`strategy: Recreate`; do not scale it.

**A restart drops pending approvals.** They fail closed - the Mac times
out and denies, and the command is re-run. This is deliberate: persistence
would add a database dependency to a service whose job is gating database
credentials, and the cost of the failure is one re-run.

## Contract with the caller

The Mac side depends on all of these:

| | |
|---|---|
| Wire format | `approve <nonce>` / `deny <nonce>`, published to the response topic |
| Nonce charset | `A-Za-z0-9`, `-`, `_` |
| Nonce max length | 128 bytes |
| `detail` max length | 256 bytes, control characters stripped |
| Request TTL | 10 minutes, also returned as `expires_in_seconds` |

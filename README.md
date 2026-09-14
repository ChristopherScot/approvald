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

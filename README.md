# approvald

Owned by me-myself-and-i.

## The spec is the source of truth

`openapi.yml` describes this API. Everything else is generated from it:

| path | what | regenerate with |
|---|---|---|
| `api/oas_*.go` | server interface, types, validation, Go client | `homelabctl regen` |
| `clients/ts/schema.d.ts` | TypeScript types | `homelabctl regen` |

**Change the API in `openapi.yml`, never in Go.** Add a path, run
`homelabctl regen`, and the build will fail until you write the handler:

```
service does not implement api.Handler (missing method GetThing)
```

That is the point — the code cannot drift from the spec, because it will
not compile if it does. CI runs `homelabctl regen` and fails on a diff, so
a spec change cannot merge without the code that matches it.

## Layout

```
openapi.yml          the API contract
main.go              process lifecycle: logging, timeouts, shutdown
server.go            handlers, metrics middleware, route mounting
api/                 generated, plus two files that are not:
  client.go            timeouts, retries, circuit breaking
  paging.go            cursor iteration as a range loop
clients/ts/          the TypeScript client
deploy/              Kubernetes manifests, rendered from config.yaml
config.yaml          what this service is, for homelabctl
```

`server.go` is the seam: a service whose shape the spec cannot express - a
reverse proxy, say - replaces that one file with a hand-written
`handler()`, and everything else stays as generated. Its header lists what
a replacement has to keep.

`api/client.go` and `api/paging.go` are hand-written and live in `api/` on
purpose: a consumer imports that package, and getting the protocol client
without the defaults would be worse than useless.

## Working on it

```sh
go test ./...                    # server, client and paging
go run .                         # PORT=3000 by default
homelabctl regen                 # after editing openapi.yml
homelabctl check deploy          # deploy manifests and spec problems
homelabctl diff                  # what would change in the GitOps repo
```

Bump `info.version` in `openapi.yml` when the API changes, then
`homelabctl regen` — it syncs the version the clients report. CI tags the
repo on a version change, and that tag is how both clients are released:

```sh
go get github.com/christopherscot/approvald@v0.2.0
npm install git+https://github.com/christopherscot/approvald#v0.2.0
```

## Calling this service

```go
c, err := api.NewClient(url, api.WithClient(api.NewHTTPClient(api.HTTPOptions{})))
```

The zero value is the intended default: a 5s timeout, one retry, no
breaker. One retry rather than five, because five can mean five times the
traffic to a dependency that is already struggling. Pass
`api.ExponentialRetry{}` or `api.NoRetry{}`, and a `Breaker`, when a
specific call wants something else.

## Deploying

Push to main. CI builds the image; argocd-image-updater sees the new digest
and commits it to the homelab repo; ArgoCD syncs it. No homelab credential
lives in this repo.

`deploy/` is rendered from `config.yaml`. Adjust it with `patches` there,
keyed by resource kind, rather than editing the manifests — a patch merges
into what the tool generates, so later convention changes still reach this
service.

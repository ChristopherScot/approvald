# Distroless rather than alpine: the binary is static (CGO_ENABLED=0), so a
# shell and package manager would add attack surface and nothing else. The
# :nonroot variant already runs as uid 65532 - the uid the generated
# securityContext expects - so there is no adduser step to drift out of
# sync, and ca-certificates are included for outbound HTTPS.
#
# bin/app is built by CI before this runs; `docker build` alone will not
# produce a working image. Run `make build` first.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --chown=65532:65532 bin/app /app/app

USER 65532:65532
EXPOSE 3000
ENTRYPOINT ["/app/app"]

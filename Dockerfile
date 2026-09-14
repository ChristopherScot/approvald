# The template's Dockerfile copies into /root/ and runs as root. This
# service runs with runAsNonRoot/runAsUser 65532 and a read-only root
# filesystem, so the binary has to live somewhere that user can execute.
FROM alpine:latest

RUN addgroup -g 65532 -S nonroot && adduser -u 65532 -S nonroot -G nonroot

WORKDIR /app
COPY bin/app /app/app
RUN chmod 0555 /app/app

USER 65532:65532
EXPOSE 3000
ENTRYPOINT ["/app/app"]

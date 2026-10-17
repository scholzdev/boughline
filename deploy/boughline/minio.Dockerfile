FROM golang:1.25 AS build
RUN CGO_ENABLED=0 GOBIN=/out go install github.com/minio/minio@v0.0.0-20260212201848-7aac2a2c5b7c
FROM alpine:3.24
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 bough
COPY --from=build /out/minio /usr/local/bin/minio
USER bough
ENTRYPOINT ["minio"]

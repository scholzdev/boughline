FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/bough ./cmd/bough
FROM alpine:3.24
RUN apk add --no-cache ca-certificates git && adduser -D -u 10001 bough
COPY --from=build /out/bough /usr/local/bin/bough
USER bough
ENTRYPOINT ["bough"]

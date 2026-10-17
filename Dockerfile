FROM --platform=$BUILDPLATFORM data.forgejo.org/oci/xx AS xx

FROM --platform=$BUILDPLATFORM data.forgejo.org/oci/golang:1.27-alpine3.24 AS build-env

ARG GOPROXY
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}

ARG RELEASE_VERSION
ARG TAGS="sqlite sqlite_unlock_notify"
ENV TAGS="bindata timetzdata $TAGS"
ARG CGO_EXTRA_CFLAGS

# it's set to local by default in the base image
# we want to use auto to let go choose the right toolchain
ENV GOTOOLCHAIN=auto

#
# Transparently cross compile for the target platform
#
COPY --from=xx / /
ARG TARGETPLATFORM
RUN apk --no-cache add clang lld
RUN xx-apk --no-cache add gcc musl-dev
ENV CGO_ENABLED=1
RUN xx-go --wrap
#
# for go generate and binfmt to find
# without it the generate phase will fail with
# #19 25.04 modules/public/public_bindata.go:8: running "go": exit status 1
# #19 25.39 aarch64-binfmt-P: Could not open '/lib/ld-musl-aarch64.so.1': No such file or directory
# why exactly is it needed? where is binfmt involved?
#
RUN cp /*-alpine-linux-musl*/lib/ld-musl-*.so.1 /lib || true

RUN apk --no-cache add build-base git nodejs npm

COPY . ${GOPATH}/src/forgejo.org
WORKDIR ${GOPATH}/src/forgejo.org

RUN make clean-no-bindata
RUN make frontend
RUN LDFLAGS="-buildid=" make FORGEJO_GENERATE_SKIP_HASH=true RELEASE_VERSION=$RELEASE_VERSION GOFLAGS="-trimpath" go-check generate-backend static-executable && xx-verify gitea

FROM data.forgejo.org/oci/alpine:3.24
ARG RELEASE_VERSION
LABEL org.opencontainers.image.authors="Boughline and Forgejo contributors" \
      org.opencontainers.image.url="https://github.com/scholzdev/boughline" \
      org.opencontainers.image.documentation="https://github.com/scholzdev/boughline/blob/main/deploy/boughline/README.md" \
      org.opencontainers.image.source="https://github.com/scholzdev/boughline" \
      org.opencontainers.image.version="${RELEASE_VERSION}" \
      org.opencontainers.image.vendor="Boughline" \
      org.opencontainers.image.licenses="GPL-3.0-or-later" \
      org.opencontainers.image.title="Boughline" \
      org.opencontainers.image.description="Self-hosted native source control with a Forgejo-based forge. Independent implementation; not an official Forgejo release."

EXPOSE 3000
RUN apk --no-cache add ca-certificates git sqlite su-exec tzdata && \
    addgroup -S -g 1000 git && \
    adduser -S -H -D -h /data/git -s /bin/sh -u 1000 -G git git
ENV USER=git GITEA_CUSTOM=/data/gitea GITEA_WORK_DIR=/data/gitea
WORKDIR /data/gitea
VOLUME ["/data"]
COPY --from=build-env /go/src/forgejo.org/gitea /usr/local/bin/forgejo
COPY deploy/boughline/forge-entrypoint.sh /usr/local/bin/boughline-forge-entrypoint
ENTRYPOINT ["/bin/sh", "/usr/local/bin/boughline-forge-entrypoint"]
CMD ["web", "--config", "/data/gitea/conf/app.ini"]

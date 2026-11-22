FROM ubuntu:22.04
# https://www.docker.com/blog/faster-multi-platform-builds-dockerfile-cross-compilation-guide/

ARG TARGETPLATFORM
ARG warp_env
ARG warp_version
ENV WARP_ENV=$warp_env
LABEL org.opencontainers.image.version=$warp_version

# see https://docs.docker.com/develop/develop-images/dockerfile_best-practices/
RUN apt-get update && apt-get install -y \
    ca-certificates \
 && rm -rf /var/lib/apt/lists/*
RUN update-ca-certificates

# the released provider, the sn miner
COPY build/$TARGETPLATFORM/provider /usr/local/libexec/bringyour-provider/provider
# runs it, keeping the commands and state directories of the images up to
# 2026.7.2 working (see the script)
COPY --chmod=0755 bringyour-provider.sh /usr/local/sbin/bringyour-provider

STOPSIGNAL SIGTERM
# additional arguments can be appended to the end
ENTRYPOINT ["/usr/local/sbin/bringyour-provider"]

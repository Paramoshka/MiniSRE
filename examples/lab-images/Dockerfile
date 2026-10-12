# Use an approved Alpine or Alpine-based FRR image pinned by digest.
ARG BASE_IMAGE
FROM ${BASE_IMAGE}
USER root
RUN apk add --no-cache iproute2 iputils traceroute tcpdump coreutils
# Keep the FRR base image's entrypoint and CMD. The helper supplies Alpine's CMD.

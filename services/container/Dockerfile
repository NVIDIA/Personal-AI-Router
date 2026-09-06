# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

# NVIDIA Personal AI Router — services-only node, for OpenShift.
#
# PAIR's headless build is thirteen statically linked Go binaries with no cgo, so the image is
# a base plus an unzip. `nvpair-tui` is the entrypoint: it launches and supervises the broker,
# which is the whole node. It is a bubbletea program and needs a PTY, so the pod sets
# tty: true -- without one it exits at startup rather than degrading.
#
# The release asset is pinned by digest. PAIR routes inference across a home LAN over mTLS;
# an unpinned download would be a supply-chain hole in exactly the wrong place.
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

ARG PAIR_VERSION=v0.1.1
ARG PAIR_SHA256=f8fb7dabec27bedc0d292e96aa170a94193552ccdee6ce4c146f976bb871c7dd
ARG PAIR_ASSET=service-binaries-linux-x64.zip

RUN microdnf install -y --setopt=install_weak_deps=0 tar gzip unzip jq python3 shadow-utils \
 && microdnf clean all

WORKDIR /opt/pair
RUN curl -fsSL -o /tmp/pair.zip \
      "https://github.com/NVIDIA/Personal-AI-Router/releases/download/${PAIR_VERSION}/${PAIR_ASSET}" \
 && echo "${PAIR_SHA256}  /tmp/pair.zip" | sha256sum -c - \
 && unzip -q /tmp/pair.zip -d /opt/pair/bin \
 && rm -f /tmp/pair.zip \
 && chmod 0755 /opt/pair/bin/* \
 && ls /opt/pair/bin

COPY loopback-relay.py /opt/pair/loopback-relay.py
COPY entrypoint.sh /opt/pair/entrypoint.sh
RUN chmod 0755 /opt/pair/entrypoint.sh /opt/pair/loopback-relay.py

# OpenShift assigns an arbitrary non-root UID in the namespace's range, so nothing may depend
# on a fixed uid. What it does guarantee is gid 0, hence group-writable everywhere the node
# writes: PAIR's per-user data root lives under $HOME/.config and must be writable by whoever
# the platform decides we are.
ENV HOME=/pairhome \
    XDG_CONFIG_HOME=/pairhome/.config \
    XDG_CACHE_HOME=/pairhome/.cache \
    XDG_RUNTIME_DIR=/tmp/pair-runtime \
    PATH=/opt/pair/bin:$PATH \
    NVPAIR_LOG_LEVEL=info
RUN mkdir -p /pairhome/.config /pairhome/.cache /tmp/pair-runtime \
 && chgrp -R 0 /pairhome /opt/pair /tmp/pair-runtime \
 && chmod -R g=u /pairhome /opt/pair /tmp/pair-runtime

USER 1001
ENTRYPOINT ["/opt/pair/entrypoint.sh"]

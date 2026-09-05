#!/usr/bin/env python3
# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Re-originate a routable TCP connection from loopback.

PAIR's proxy endpoints answer plaintext only when the connection arrives from 127.0.0.1 --
anything else gets `{"code":"loopback-only"}` and a 403. That is deliberate: on a node running
PAIR those ports are cluster front doors, and a peer is supposed to come in over mTLS.

Workloads inside OpenShift are neither. They are on the pod network, they have no client
certificate, and giving them one would mean handing a cluster identity to every consumer. So
this listens on a routable address in the same network namespace and opens a *fresh* connection
from 127.0.0.1, which the proxy accepts. The relay is the trust boundary: whatever reaches it
is already inside the cluster's NetworkPolicy.

Deliberately dependency-free and deliberately dumb -- it copies bytes and nothing else. It does
not parse HTTP, so streaming completions and long-lived connections pass through untouched.
"""

from __future__ import annotations

import argparse
import selectors
import socket
import sys
import threading

BUF = 65536


def _pump(src: socket.socket, dst: socket.socket) -> None:
    try:
        while True:
            data = src.recv(BUF)
            if not data:
                break
            dst.sendall(data)
    except OSError:
        pass
    finally:
        # Half-close so the far side sees EOF rather than waiting out a timeout.
        for s in (src, dst):
            try:
                s.shutdown(socket.SHUT_WR)
            except OSError:
                pass


def _serve_one(client: socket.socket, target: tuple[str, int], timeout: float) -> None:
    try:
        upstream = socket.create_connection(target, timeout=timeout)
    except OSError as err:
        print(f"relay: upstream {target[0]}:{target[1]} unreachable: {err}", file=sys.stderr)
        client.close()
        return
    upstream.settimeout(None)
    client.settimeout(None)
    for a, b in ((client, upstream), (upstream, client)):
        threading.Thread(target=_pump, args=(a, b), daemon=True).start()


def main(argv: list[str] | None = None) -> int:
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("--listen", default="0.0.0.0")
    ap.add_argument("--port", type=int, required=True, help="routable port to accept on")
    ap.add_argument("--target-host", default="127.0.0.1", help="must be loopback for PAIR")
    ap.add_argument("--target-port", type=int, required=True)
    ap.add_argument("--connect-timeout", type=float, default=10.0)
    a = ap.parse_args(argv)

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((a.listen, a.port))
    srv.listen(128)
    print(
        f"relay: {a.listen}:{a.port} -> {a.target_host}:{a.target_port} "
        "(re-originating from loopback)",
        flush=True,
    )
    sel = selectors.DefaultSelector()
    sel.register(srv, selectors.EVENT_READ)
    while True:
        for _key, _mask in sel.select(timeout=None):
            try:
                client, _addr = srv.accept()
            except OSError:
                continue
            _serve_one(client, (a.target_host, a.target_port), a.connect_timeout)


if __name__ == "__main__":
    raise SystemExit(main())

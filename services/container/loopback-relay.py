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
from 127.0.0.1, which the proxy accepts.

The relay is the trust boundary, and it has to enforce that boundary itself: the pod is on the
host network, and NetworkPolicy does not apply to host-network pods, so a listener on 0.0.0.0
here is a listener on the LAN. Every accepted connection is checked against --allow-cidr before
anything is forwarded; a peer outside the allowlist gets a canned 403 and is closed. The default
allowlist is loopback plus the cluster's pod network.

Deliberately dependency-free and deliberately dumb -- it copies bytes and nothing else. It does
not parse HTTP, so streaming completions and long-lived connections pass through untouched.
"""

from __future__ import annotations

import argparse
import ipaddress
import selectors
import socket
import sys
import threading

BUF = 65536

# Loopback, and OVN-Kubernetes' default cluster network. Override with --allow-cidr.
DEFAULT_ALLOW = ["127.0.0.0/8", "10.128.0.0/14"]

_REJECT_BODY = b'{"code":"source-not-allowed","error":"relay accepts cluster sources only"}\n'
REJECT = (
    b"HTTP/1.1 403 Forbidden\r\n"
    b"Content-Type: application/json\r\n"
    b"Connection: close\r\n"
    b"Content-Length: " + str(len(_REJECT_BODY)).encode() + b"\r\n"
    b"\r\n" + _REJECT_BODY
)


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


def _allowed(addr: str, allow: list[ipaddress.IPv4Network | ipaddress.IPv6Network]) -> bool:
    try:
        ip = ipaddress.ip_address(addr)
    except ValueError:
        return False
    return any(ip in net for net in allow)


def _reject(client: socket.socket, addr: str) -> None:
    # The caller is almost certainly speaking HTTP; answer in kind so a curl shows a 403 and a
    # reason rather than a bare reset. Not parsing the request is the point, so this is sent
    # blind and the connection closed.
    print(f"relay: rejected {addr}: outside --allow-cidr", file=sys.stderr, flush=True)
    try:
        client.settimeout(2.0)
        client.sendall(REJECT)
    except OSError:
        pass
    finally:
        client.close()


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
    ap.add_argument(
        "--allow-cidr",
        action="append",
        metavar="CIDR",
        help=f"source network allowed to use the relay; repeatable (default: {', '.join(DEFAULT_ALLOW)})",
    )
    a = ap.parse_args(argv)
    allow = [ipaddress.ip_network(c, strict=False) for c in (a.allow_cidr or DEFAULT_ALLOW)]

    srv = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind((a.listen, a.port))
    srv.listen(128)
    print(
        f"relay: {a.listen}:{a.port} -> {a.target_host}:{a.target_port} "
        f"(re-originating from loopback; sources allowed: {', '.join(map(str, allow))})",
        flush=True,
    )
    sel = selectors.DefaultSelector()
    sel.register(srv, selectors.EVENT_READ)
    while True:
        for _key, _mask in sel.select(timeout=None):
            try:
                client, (addr, _port) = srv.accept()
            except OSError:
                continue
            if not _allowed(addr, allow):
                threading.Thread(target=_reject, args=(client, addr), daemon=True).start()
                continue
            _serve_one(client, (a.target_host, a.target_port), a.connect_timeout)


if __name__ == "__main__":
    raise SystemExit(main())

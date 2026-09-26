# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Native fixed-rank plan admission for qualified RoCE auxiliary HCA names."""

import unittest

import vllm_rank_owner as owner


def qwen_plan():
    def lane(peer, local, remote, interface, index, device):
        return {
            "peerNodeId": peer, "localAddress": local, "peerAddress": remote,
            "interfaceName": interface, "interfaceIndex": index,
            "mac": "02:00:00:00:00:01", "switchId": "switch-a", "portName": "p0",
            "rdmaDevice": device, "gidPort": 1, "gidIndex": 3, "gidType": "RoCE v2",
        }
    return {
        "owner": owner.OWNER, "runId": "a" * 32, "generation": 1, "rank": 0,
        "planDigest": "b" * 64, "nodeId": "node-a", "model": owner.QWEN_MODEL,
        "modelDigest": "c" * 64, "runtimeDigest": "d" * 64,
        "configSha256": "e" * 64, "uid": 1000,
        "manager": {"pid": 2, "startTicks": "1", "uid": 1000},
        "runtimeDir": "/opt/rt", "modelPath": "/opt/model", "resources": {},
        "gpuUuid": "GPU-11111111-1111-1111-1111-111111111111",
        "peers": ["10.0.0.1", "10.0.0.2"],
        "localAddress": "10.0.0.1", "coordinatorAddress": "10.0.0.1",
        "apiPort": 8000, "masterPort": 29500,
        "topology": {"tensorParallel": 2, "pipelineParallel": 1,
                     "dataParallel": 1, "expertParallel": 2,
                     "contextLength": 32768,
                     "maxSequences": 2, "kvCacheMemoryBytes": 8 * 1024**3,
                     "mtp": False, "dflash": False, "flashinferAutotune": False},
        "transport": {"mode": "host-buffer-roce", "operationId": "f" * 32,
                      "qualificationSha256": "1" * 64, "netGdrLevel": 0,
                      "netGdrC2c": 0, "netGdrRead": 0, "netPlugin": "none",
                      "envPlugin": "none", "ginPlugin": "none",
                      "subnetAwareRouting": False, "subnetPrefixLength": 0,
                      "mergeNICs": True,
                      "socketPayloadFallback": False},
        "rdmaLanes": [
            lane("node-b", "10.253.0.1", "10.253.0.2", "enp1s0f0np0", 1, "rocep1s0f0"),
            lane("node-b", "10.253.0.5", "10.253.0.6", "enP2p1s0f0np0", 2, "rocep1s0f1"),
        ],
        "devices": ["/dev/nvidiactl", "/dev/nvidia-modeset", "/dev/nvidia-uvm",
                    "/dev/nvidia-uvm-tools", "/dev/nvidia0",
                    "/dev/infiniband/uverbs0", "/dev/infiniband/uverbs1"],
        "limits": owner.QWEN_LIMITS,
    }


class QwenHCANameTest(unittest.TestCase):
    def test_native_owner_accepts_qualified_auxiliary_and_legacy_names(self):
        plan = qwen_plan()
        self.assertIs(owner.check_plan(plan), plan)
        plan["rdmaLanes"][1]["rdmaDevice"] = "roceP2p1s0f1"
        self.assertIs(owner.check_plan(plan), plan)
        plan["rdmaLanes"][0]["rdmaDevice"] = "mlx5_0"
        plan["rdmaLanes"][1]["rdmaDevice"] = "mlx5_1"
        self.assertIs(owner.check_plan(plan), plan)

    def test_native_owner_rejects_untrusted_names_and_keeps_lane_checks(self):
        for name in ("rocep1s0f0/other", "../rocep1s0f0", "rocep1s0f0\n",
                     "rocep1s0f0\x00", "rocep1000s0f0", "roce1s0f0", "mlx5_0/other",
                     "roceP0p1s0f0", "roceP1p1s0f0", "roceP3p1s0f0", "roceP22p1s0f0",
                     "rocePp1s0f0", "roceP2s0f0", "roceP2p1s0f0x", "rocep2P1s0f0"):
            with self.subTest(name=repr(name)):
                plan = qwen_plan()
                plan["rdmaLanes"][0]["rdmaDevice"] = name
                with self.assertRaisesRegex(owner.Unavailable, "qualified_roce_lane_required"):
                    owner.check_plan(plan)
        for field, value in (("rdmaDevice", "rocep1s0f0"), ("gidIndex", 4),
                             ("interfaceIndex", 1)):
            with self.subTest(field=field):
                plan = qwen_plan()
                plan["rdmaLanes"][1][field] = value
                with self.assertRaisesRegex(owner.Unavailable, "qualified_roce_lane_required"):
                    owner.check_plan(plan)


if __name__ == "__main__":
    unittest.main()

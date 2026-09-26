# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0

"""Fixed Qwen3.8 rank command arguments."""

import unittest

import vllm_rank_owner as owner
from test_vllm_rank_owner_hca import qwen_plan


class QwenRankArgumentsTest(unittest.TestCase):
    def test_qwen_rank_keeps_fixed_memory_headroom_over_saved_settings(self):
        plan = qwen_plan()
        plan["resources"] = {"gpu_memory_utilization": 0.05, "max_model_len": 2048}
        args = owner.model_arguments(plan, "/opt/rt/venv/bin/vllm", owner.QWEN_RUNTIME)
        self.assertEqual(args.count("--gpu-memory-utilization"), 1)
        self.assertEqual(args[args.index("--gpu-memory-utilization") + 1], owner.QWEN_GPU_MEMORY_UTILIZATION)
        self.assertEqual(args.count("--max-model-len"), 1)
        self.assertEqual(args[args.index("--max-model-len") + 1], str(plan["topology"]["contextLength"]))
        self.assertLess(float(owner.QWEN_GPU_MEMORY_UTILIZATION), 0.92)

    def test_two_spark_qwen_rank_runs_tensor_and_expert_parallel(self):
        args = owner.model_arguments(qwen_plan(), "/opt/rt/venv/bin/vllm", owner.QWEN_RUNTIME)
        for flag, value in (("--nnodes", "2"), ("--tensor-parallel-size", "2"),
                            ("--pipeline-parallel-size", "1"), ("--data-parallel-size", "1")):
            self.assertEqual(args[args.index(flag) + 1], value)
        self.assertIn("--enable-expert-parallel", args)
        self.assertNotIn("--enable-eplb", args)

    def test_native_owner_rejects_every_three_spark_qwen_layout(self):
        layouts = (
            {"tensorParallel": 1, "pipelineParallel": 3, "dataParallel": 1, "expertParallel": 1},
            {"tensorParallel": 1, "pipelineParallel": 1, "dataParallel": 3, "expertParallel": 3,
             "eplb": True, "redundantExperts": 1},
            {"tensorParallel": 2, "pipelineParallel": 1, "dataParallel": 1, "expertParallel": 2},
        )
        for layout in layouts:
            with self.subTest(layout=layout):
                plan = qwen_plan()
                plan["peers"] = ["10.0.0.1", "10.0.0.2", "10.0.0.3"]
                plan["topology"].update(layout)
                with self.assertRaisesRegex(owner.Unavailable, "unsupported_fixed_topology"):
                    owner.check_plan(plan)

    def test_native_owner_rejects_subnet_routing_on_the_direct_fabric(self):
        plan = qwen_plan()
        plan["transport"].update({"subnetAwareRouting": True, "subnetPrefixLength": 31})
        with self.assertRaisesRegex(owner.Unavailable, "qualified_roce_transport_required"):
            owner.check_plan(plan)


if __name__ == "__main__":
    unittest.main()

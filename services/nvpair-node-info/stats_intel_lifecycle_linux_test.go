// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunXpuSmi_StopsWaitingOnInheritedOutput(t *testing.T) {
	root := newIntelDRMFixture(t)
	t.Setenv("FIXTURE", root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(`#!/bin/sh
/bin/sleep 5 &
echo started > "$FIXTURE/active"
wait
`), 0700))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := runXpuSmi(ctx, "discovery")
		done <- err
	}()
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "active"))
		return err == nil
	}, 2*time.Second, 10*time.Millisecond)
	cancel()
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(2 * statsTickInterval):
		t.Fatal("Intel command waited for a child that inherited its output")
	}
}

func TestStatsCollector_StopsDuringIntelDiscovery(t *testing.T) {
	root := newIntelDRMFixture(t)
	t.Setenv("FIXTURE", root)
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(`#!/bin/sh
echo discovery > "$FIXTURE/active"
exec /bin/sleep 20
`), 0700))

	start := time.Now()
	collector := startStatsCollector()
	t.Cleanup(collector.Stop)
	require.Less(t, time.Since(start), time.Second, "Intel discovery delayed collector startup")
	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "active"))
		return err == nil
	}, 2*time.Second, 10*time.Millisecond, "Intel discovery did not begin")

	start = time.Now()
	collector.Stop()
	require.Less(t, time.Since(start), time.Second, "stop did not cancel Intel discovery")
}

func TestStatsCollector_ContinuesPublishingWhileIntelWaits(t *testing.T) {
	root := newIntelDRMFixture(t)
	t.Setenv("FIXTURE", root)
	const xpu = `#!/bin/sh
if [ "$1" = discovery ]; then
  echo '{"device_list":[{"device_id":0,"pci_bdf_address":"03:00.0"}]}'
  exit
fi
if [ ! -f "$FIXTURE/sampled" ]; then
  /bin/sleep 0.2
  echo sampled > "$FIXTURE/sampled"
  printf 'DeviceId,GPU utilization (%%)\n0,73\n'
  exit
fi
echo waiting > "$FIXTURE/active"
exec /bin/sleep 20
`
	const nvidia = "#!/bin/sh\nprintf 'GPU-fixture, 53, 256\\n'\n"
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(xpu), 0700))
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte(nvidia), 0700))

	const intelKey = "intel-pci-0000:03:00.0"
	collector := startStatsCollector()
	t.Cleanup(collector.Stop)
	var first time.Time
	require.Eventually(t, func() bool {
		snap := collector.Snapshot()
		first = snap.GPUSampledAt
		return snap.GPU[intelKey].UtilizationPct == 73 && snap.GPU["GPU-fixture"].UtilizationPct == 53
	}, 3*time.Second, 10*time.Millisecond, "first tick did not publish both GPUs")

	require.Eventually(t, func() bool {
		_, err := os.Stat(filepath.Join(root, "active"))
		return err == nil
	}, time.Second, 10*time.Millisecond, "second Intel dump did not begin")
	require.Eventually(t, func() bool {
		snap := collector.Snapshot()
		_, hasIntel := snap.GPU[intelKey]
		return !hasIntel && len(snap.GPUInventory) == 1 && snap.GPU["GPU-fixture"].UtilizationPct == 53 && snap.GPUSampledAt.After(first)
	}, 1700*time.Millisecond, 10*time.Millisecond, "stale Intel reading accompanied a fresh NVIDIA tick")

	start := time.Now()
	collector.Stop()
	require.Less(t, time.Since(start), time.Second, "stop did not cancel the active Intel dump")
}

func TestStatsCollector_DropsOldIntelFieldsOnFreshNvidiaTick(t *testing.T) {
	root := newIntelDRMFixture(t)
	require.NoError(t, os.WriteFile(filepath.Join(root, "nvidia-smi"), []byte("#!/bin/sh\necho 'GPU-fixture, 5, 256'\n"), 0700))
	for _, tc := range []struct {
		name      string
		age       time.Duration
		busy      uint32
		wantIntel bool
	}{
		{"old utilization", time.Minute, 95, false},
		{"old memory only", time.Minute, 0, false},
		{"new memory only", 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := statsCollector{}
			c.latestIntel.Store(&intelGPUReading{
				stats:       map[string]gpuStat{"intel": {UtilizationPct: tc.busy, VRAMUsed: 4096}},
				collectedAt: time.Now().Add(-tc.age), hasUtilization: tc.busy > 0,
			})
			snap := c.decodeSnapshot()
			require.EqualValues(t, 5, snap.GPU["GPU-fixture"].UtilizationPct)
			_, present := snap.GPU["intel"]
			require.Equal(t, tc.wantIntel, present)
		})
	}
}

func TestStatsCollector_PublishesRecoveredIntelAdapter(t *testing.T) {
	root := newIntelDRMFixture(t)
	t.Setenv("FIXTURE", root)
	const cli = `#!/bin/sh
if [ "$1" = discovery ]; then
  [ -f "$FIXTURE/ready" ] || exit 1
  echo '{"device_list":[{"device_id":0,"device_name":"Arc fixture","pci_bdf_address":"03:00.0","memory_physical_size_byte":8192}]}'
else
  printf 'DeviceId,GPU utilization (%%)\n0,73\n'
fi
`
	require.NoError(t, os.WriteFile(filepath.Join(root, "xpu-smi"), []byte(cli), 0700))
	inventory := detectGPUs()
	require.Empty(t, inventory)
	require.NoError(t, os.WriteFile(filepath.Join(root, "ready"), nil, 0600))
	c := startStatsCollector()
	t.Cleanup(c.Stop)
	var snap statsSnapshot
	require.Eventually(t, func() bool {
		snap = c.Snapshot()
		return !snap.GPUSampledAt.IsZero() && snap.GPU["intel-pci-0000:03:00.0"].UtilizationPct == 73
	}, 3*time.Second, 10*time.Millisecond)
	for _, initial := range [][]GPUInfo{inventory, {{Name: "Unknown", pciAddress: "0000:03:00.0"}}} {
		response := buildResponse(initial, nil, 0, snap, "", nil)
		require.Len(t, response.GPUs, 1)
		require.Equal(t, "Arc fixture", response.GPUs[0].Name)
		require.EqualValues(t, 8192, response.GPUs[0].VramBytes)
		require.EqualValues(t, 73, response.GPUs[0].UtilizationPercent)
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// startTempEngine copies the fake engine into dir and runs it in its
// `noserve` mode (stays alive, binds no port). The copies run from paths
// spelled the way a managed install is spelled — under $TMPDIR, which macOS
// hides behind the /var → /private/var symlink — and one directory name
// carries a space, because macOS reports paths in their canonical shape and
// leaves spaces unquoted, and both are things the identity check survives in
// production.
func startTempEngine(t *testing.T, dir, name string) (*managedProc, string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(dir, 0o755))
	binPath := filepath.Join(dir, name)
	copyFile(t, fakeEngineBin, binPath)
	proc, err := startManagedProc(binPath, []string{"noserve"}, nil, nil)
	require.NoError(t, err)
	t.Cleanup(func() { proc.stop(0) })
	return proc, binPath
}

// waitForImage gives the kernel-backed lookup a moment after the process
// starts: on a loaded machine the first probe can still catch it mid-launch.
func waitForImage(t *testing.T, pid int) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		image := procImage(pid)
		if image != "" || !time.Now().Before(deadline) {
			return image
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestProcImage_IdentifiesOurManagedCopyUnderASymlinkedTempRoot proves the
// identity gate Stop uses for orphan reclaim can fire on macOS: the image
// read from the running process and the manifest's managed path name one
// file once isOurEngineImage canonicalises, even though macOS spells a
// temporary path /private/var/... while t.TempDir answers /var/....
func TestProcImage_IdentifiesOurManagedCopyUnderASymlinkedTempRoot(t *testing.T) {
	proc, binPath := startTempEngine(t, filepath.Join(t.TempDir(), "engine bin"), "fakeu")
	image := waitForImage(t, proc.cmd.Process.Pid)
	require.NotEmpty(t, image, "a live process must report its executable image on macOS")
	assert.Contains(t, image, filepath.Join("engine bin", "fakeu"),
		"the image must name the running copy with its full path, spaces included (image %q)", image)

	require.True(t, isOurEngineImage(image, binPath),
		"procImage and the manifest path name one file, but the check declined them: %q vs %q", image, binPath)

	// The fail-closed side: a same-named, byte-identical copy elsewhere is
	// foreign, and canonicalising must not blur the two.
	otherDir := t.TempDir()
	other := filepath.Join(otherDir, "fakeu")
	copyFile(t, fakeEngineBin, other)
	assert.False(t, isOurEngineImage(image, other),
		"an identical copy at a different path must not read as our managed binary (%q vs %q)", image, other)
}

func TestProcImage_ReportsNothingForProcessesThatAlreadyExited(t *testing.T) {
	assert.Empty(t, procImage(0), "pid 0 is not a process")
	assert.Empty(t, procImage(999_999_999), "a pid far above anything this machine assigns never existed")

	proc, _ := startTempEngine(t, t.TempDir(), "fakeu")
	pid := proc.cmd.Process.Pid
	require.NotEmpty(t, waitForImage(t, pid), "precondition: the copy runs and reports its image")

	proc.stop(0)
	select {
	case <-proc.done: // the managed-proc goroutine reaped the child
	case <-time.After(5 * time.Second):
		require.FailNow(t, "test engine did not exit")
	}
	assert.Empty(t, procImage(pid), "an exited process must fail closed, not report a stale image")
}

// The fixtures below are real `lsof -a -p <pid> -d txt -Fn` captures, kept
// verbatim so the parser is pinned to the format lsof actually emits.

// TestParseLsofExecName_PicksTheExecutedBinaryAheadOfItsLibraries covers the
// one contract the identity gate relies on: the first path field is the
// exec'd binary, not dyld or a library — including when the path carries
// spaces or the executable sits inside an app bundle.
func TestParseLsofExecName_PicksTheExecutedBinaryAheadOfItsLibraries(t *testing.T) {
	const copiedBinaryWithSpaces = "p87789\nftxt\nn/private/tmp/pair space dir/fake u\nftxt\nn/usr/lib/dyld\n"
	assert.Equal(t, "/private/tmp/pair space dir/fake u", parseLsofExecName(copiedBinaryWithSpaces))

	const pythonFramework = "p87795\nftxt\nn/opt/homebrew/Cellar/python@3.14/3.14.10/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python\nftxt\nn/usr/lib/dyld\n"
	assert.Equal(t, "/opt/homebrew/Cellar/python@3.14/3.14.10/Frameworks/Python.framework/Versions/3.14/Resources/Python.app/Contents/MacOS/Python",
		parseLsofExecName(pythonFramework))

	assert.Empty(t, parseLsofExecName("p12345\n"), "a record with no path fields reports nothing")
	assert.Empty(t, parseLsofExecName(""), "lsof on a dead pid prints nothing at all")
}

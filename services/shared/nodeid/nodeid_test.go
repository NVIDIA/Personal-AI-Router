// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package nodeid

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestResolve_MintsAndPersists(t *testing.T) {
	base := t.TempDir()
	first := Resolve(base)
	assert.NotEqual(t, "", first, "Resolve returned empty UUID")
	require.FileExists(t, filepath.Join(base, "node-id.json"), "node-id.json not persisted")
	// A second resolve must return the same persisted UUID.
	assert.Equal(t, first, Resolve(base), "Resolve not stable")
}

func TestResolve_PrefersClusterIdentity(t *testing.T) {
	base := t.TempDir()
	clusterDir := filepath.Join(base, "cluster")
	require.NoError(t, os.MkdirAll(clusterDir, 0o700))
	const want = "11111111-2222-4333-8444-555555555555"
	require.NoError(t, os.WriteFile(filepath.Join(clusterDir, "identity.json"),
		[]byte(`{"node_uuid":"`+want+`","created_at":1}`), 0o600))
	assert.Equal(t, want, Resolve(base))
	// It should not have written its own node-id.json when reusing the
	// cluster identity.
	_, err := os.Stat(filepath.Join(base, "node-id.json"))
	assert.ErrorIs(t, err, os.ErrNotExist, "node-id.json should not exist when cluster identity is present")
}

func TestResolve_UnwritableConfigDirStillAdvertisesAndWarns(t *testing.T) {
	base := t.TempDir()
	idPath := filepath.Join(base, "node-id.json")

	handler := &capturingHandler{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	t.Cleanup(func() {
		// t.TempDir's own teardown cannot delete a directory that is not
		// writable, so hand the mode back before that teardown runs.
		if err := os.Chmod(base, 0o700); err != nil {
			t.Errorf("restore mode on %s: %v", base, err)
		}
	})
	// A read-only config directory is the case the warning exists for. Inside
	// persist, os.MkdirAll reports success for the directory that already exists,
	// so the tight mode is not silently widened, and the write that then fails is
	// node-id.json.tmp beside the file it was staging.
	require.NoError(t, os.Chmod(base, 0o500))

	uuid := Resolve(base)

	// Running as root bypasses the directory mode, so the save succeeds and there
	// is no failed write to report; skip rather than fail such an environment.
	if _, err := os.Stat(idPath); err == nil {
		t.Skipf("%s was written although %s is read-only, so this process bypasses directory permissions", idPath, base)
	}

	assert.NotEqual(t, "", uuid, "a UUID that could not be saved is still usable for this run")

	warning := handler.recordAt(slog.LevelWarn)
	require.NotNil(t, warning, "Resolve must warn when the identity it minted could not be saved")
	assert.Contains(t, warning.Message, "node identity could not be persisted",
		"the warning must say what failed, not just that something did")

	var loggedPath, loggedErr string
	warning.Attrs(func(attr slog.Attr) bool {
		switch attr.Key {
		case "path":
			loggedPath = attr.Value.String()
		case "err":
			loggedErr = attr.Value.String()
		}
		return true
	})
	assert.Equal(t, idPath, loggedPath, "the warning must name the file it could not write")
	assert.NotEmpty(t, loggedErr, "the warning must carry the I/O error")
}

func TestResolve_PersistedIdentityLogsNoWarning(t *testing.T) {
	// The warning is about the write that failed, not about minting in general. A
	// first start that saves cleanly has a stable identity, so there is nothing
	// to report: warning anyway would point at a disk that is fine and train
	// whoever reads the log to ignore the one warning that matters. Only the level
	// is pinned. A benign notice on this path, such as "minted new node identity"
	// at Info level, would be a reasonable addition, so this test must not forbid
	// it — forbidding every record would be rejected for the wrong reason later.
	base := t.TempDir()

	handler := &capturingHandler{}
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(handler))
	t.Cleanup(func() { slog.SetDefault(previousLogger) })

	uuid := Resolve(base)

	require.NotEqual(t, "", uuid, "Resolve returned empty UUID")
	require.FileExists(t, filepath.Join(base, "node-id.json"), "node-id.json not persisted")

	var problems []string
	for _, record := range handler.records {
		if record.Level >= slog.LevelWarn {
			problems = append(problems, record.Level.String()+": "+record.Message)
		}
	}
	assert.Empty(t, problems, "a saved identity is not a problem, so nothing at warning level or above may be logged")
}

// capturingHandler keeps every record it is handed so a test can assert what a
// call logged. The package has no shared slog test helper yet, so this file
// owns the one it needs. records has no mutex, so a handler instance belongs
// to one test: it is not safe for t.Parallel(), which would let a second test
// log through the process-wide default logger while the first reads them. The
// package has no parallel tests today.
type capturingHandler struct {
	records []slog.Record
}

func (*capturingHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *capturingHandler) Handle(_ context.Context, record slog.Record) error {
	h.records = append(h.records, record.Clone())
	return nil
}

func (h *capturingHandler) WithAttrs([]slog.Attr) slog.Handler { return h }

func (h *capturingHandler) WithGroup(string) slog.Handler { return h }

// recordAt returns the first record logged at level, or nil when nothing was.
func (h *capturingHandler) recordAt(level slog.Level) *slog.Record {
	for i := range h.records {
		if h.records[i].Level == level {
			return &h.records[i]
		}
	}
	return nil
}

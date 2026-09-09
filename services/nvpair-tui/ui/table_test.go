// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui

import (
	"fmt"
	"strings"
	"testing"
)

// TestManualTableFitsViewportWidth guards against the table body rendering
// wider than its viewport: the viewport hard-truncates every body row at the
// terminal width, so any width miscalculation silently clips the rightmost
// column. The last column's value ("yes", unique to that column here) must
// survive rendering intact at every width in the sweep (168 is a real-world
// operator width).
func TestManualTableFitsViewportWidth(t *testing.T) {
	for _, w := range []int{44, 60, 80, 120, 168} {
		t.Run(fmt.Sprintf("width%03d", w), func(t *testing.T) {
			v := newManualView(nil)
			v.SetSize(w, 20)
			v.setNodes([]manualNode{{
				ID:         "manual:127.0.0.1:8888",
				Address:    "127.0.0.1:8888",
				OllamaUp:   false,
				NodeInfoUp: false,
				OpenAIUp:   true,
			}})
			if !strings.Contains(v.View(), "yes") {
				t.Errorf("w=%d: last column value clipped — table renders wider than the viewport", w)
			}
		})
	}
}

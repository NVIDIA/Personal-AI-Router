// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package enginesettings

import "testing"

func TestFullBaselineCoalescingAndLimits(t *testing.T) {
	h := &Hub{}
	h.Publish([]Snapshot{{Revision: 1}})
	ch, closeSub, ok := h.Subscribe()
	if !ok || (<-ch)[0].Revision != 1 {
		t.Fatal("baseline missing")
	}
	for i := uint64(2); i < 1000; i++ {
		h.Publish([]Snapshot{{Revision: i}})
	}
	if (<-ch)[0].Revision != 999 {
		t.Fatal("slow subscriber missed latest full state")
	}
	closeSub()
	var closeAll []func()
	for i := 0; i < 64; i++ {
		_, close, ok := h.Subscribe()
		if !ok {
			t.Fatal("early subscriber cap")
		}
		closeAll = append(closeAll, close)
	}
	if _, _, ok := h.Subscribe(); ok {
		t.Fatal("unbounded subscribers")
	}
	for _, close := range closeAll {
		close()
	}
	if _, close, ok := h.Subscribe(); !ok {
		t.Fatal("cleanup leaked capacity")
	} else {
		close()
	}
}

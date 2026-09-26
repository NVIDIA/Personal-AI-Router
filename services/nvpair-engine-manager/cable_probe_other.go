// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"errors"
	"time"

	"nvpair-shared/cableprobe"
)

func nativeCableProbeIO() cableProbeIO {
	unavailable := errors.New("native cable probing is unavailable on this platform")
	return cableProbeIO{
		validate:  func(cableprobe.Interface, cableprobe.PortRef) error { return unavailable },
		lock:      func() (int, error) { return -1, unavailable },
		open:      func() (int, error) { return -1, unavailable },
		configure: func(int, cableprobe.Interface, cableprobe.PortRef) error { return unavailable },
		read:      func([]int, time.Duration) (cableProbePacket, error) { return cableProbePacket{}, unavailable },
		write:     func(int, []byte) error { return unavailable },
		close:     func(int) error { return unavailable },
	}
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/binary"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

func TestXeVRAMTotalCapturedB580(t *testing.T) {
	text, err := os.ReadFile("testdata/b580-xe-memory.txt")
	if err != nil {
		t.Fatal(err)
	}
	buf, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil {
		t.Fatal(err)
	}
	total, ok := parseXeVRAMTotal(buf)
	if !ok || total != 12809404416 {
		t.Fatalf("total=%d known=%v", total, ok)
	}
	// The first region is 32GB system RAM; neither it nor raw used is VRAM total.
	if total == binary.LittleEndian.Uint64(buf[16:24]) {
		t.Fatal("system RAM selected")
	}
}

func TestXeVRAMTotalRejectsUnknown(t *testing.T) {
	region := func(class uint16, size uint64) []byte {
		b := make([]byte, 96)
		binary.LittleEndian.PutUint32(b, 1)
		binary.LittleEndian.PutUint16(b[8:10], class)
		binary.LittleEndian.PutUint64(b[16:24], size)
		return b
	}
	truncated := region(1, 12<<30)
	binary.LittleEndian.PutUint32(truncated, 2)
	for name, b := range map[string][]byte{"empty": nil, "header": make([]byte, 7), "system": region(0, 32<<30), "zero": region(1, 0), "truncated": truncated} {
		t.Run(name, func(t *testing.T) {
			if n, ok := parseXeVRAMTotal(b); ok || n != 0 {
				t.Fatalf("unknown became total=%d known=%v", n, ok)
			}
		})
	}
}

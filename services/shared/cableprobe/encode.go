// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package cableprobe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"strconv"
	"time"
)

// EncodeFrame produces only the fixed diagnostic profile. It neither sends the
// frame nor grants permission to send it; the marker is correlation, not trust.
func EncodeFrame(sourceMAC, marker string, sequence uint32) ([]byte, error) {
	mac, err := net.ParseMAC(sourceMAC)
	if err != nil || len(mac) != 6 || mac[0]&1 != 0 || bytes.Equal(mac, make([]byte, 6)) ||
		!validMarker(marker) || sequence < 1 || sequence > 100 {
		return nil, errors.New("invalid fixed-profile LLDP frame input")
	}
	data := append([]byte{1, 128, 194, 0, 0, 14}, mac...)
	data = append(data, 0x88, 0xcc)
	for _, tlv := range []struct {
		kind  uint16
		value []byte
	}{
		{1, append([]byte{4}, mac...)},
		{2, append([]byte{3}, mac...)},
		{3, binary.BigEndian.AppendUint16(nil, uint16(Window/time.Second))},
		{6, []byte(markerPrefix + marker + " " + strconv.FormatUint(uint64(sequence), 10))},
		{0, nil},
	} {
		data = binary.BigEndian.AppendUint16(data, tlv.kind<<9|uint16(len(tlv.value)))
		data = append(data, tlv.value...)
	}
	// The mandatory marker makes this profile longer than the 60-byte Ethernet
	// minimum without FCS, so no padding or additional payload is needed.
	return data, nil
}

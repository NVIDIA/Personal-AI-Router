// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"nvpair-shared/hostbootstrap"
)

var ErrPeerUnauthorized = errors.New("local helper peer is not authorized")

var helperHex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
var helperHex64 = regexp.MustCompile(`^[0-9a-f]{64}$`)

func strictObject(
	raw []byte,
	allowed map[string]bool,
) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	values := make(map[string]json.RawMessage)
	folded := make(map[string]bool)
	for decoder.More() {
		fieldToken, err := decoder.Token()
		field, ok := fieldToken.(string)
		if err != nil || !ok || !allowed[field] || folded[strings.ToLower(field)] {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		folded[strings.ToLower(field)] = true
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, hostbootstrap.ErrHelperProtocol
		}
		values[field] = value
	}
	if token, err := decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, hostbootstrap.ErrHelperProtocol
	}
	if err := requireJSONEOF(decoder); err != nil {
		return nil, err
	}
	return values, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return hostbootstrap.ErrHelperProtocol
	}
	return nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"os"
)

type stdioTransport struct {
	io.Reader
	io.Writer
}

func newStdioTransport() io.ReadWriteCloser {
	return &stdioTransport{
		Reader: os.Stdin,
		Writer: os.Stdout,
	}
}

func (s *stdioTransport) Close() error {
	return nil
}

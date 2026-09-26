// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

// syncDir is a no-op on Windows. There is no directory handle to flush:
// FlushFileBuffers rejects one, and NTFS journals the rename's metadata itself.
func syncDir(string) error { return nil }

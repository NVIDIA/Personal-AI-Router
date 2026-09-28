// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build !linux

package main

import (
	"context"
	"errors"
)

var errFabricNativePlatform = errors.New("nonpersistent fabric address configuration requires the reviewed Linux native worker")

func fabricNativeInspect(context.Context, []fabricInterface) (fabricNativeFacts, error) {
	return fabricNativeFacts{}, errFabricNativePlatform
}
func fabricNativeInspectRebind(context.Context, []fabricInterface, bool) (fabricNativeFacts, error) {
	return fabricNativeFacts{}, errFabricNativePlatform
}
func fabricNativeRoutes(context.Context) ([]string, error) { return nil, errFabricNativePlatform }
func fabricNativeRouteQualified(context.Context, fabricInterface, string) (fabricRDMABinding, error) {
	return fabricRDMABinding{}, errFabricNativePlatform
}
func fabricNativeRoutedQualified(context.Context, fabricInterface, fabricRoute) error {
	return errFabricNativePlatform
}
func fabricNativeAdd(context.Context, fabricInterface, string, int, bool) error {
	return errFabricNativePlatform
}
func fabricNativeRemove(context.Context, fabricInterface, string) error {
	return errFabricNativePlatform
}

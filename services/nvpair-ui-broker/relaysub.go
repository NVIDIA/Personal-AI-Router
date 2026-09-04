// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"

	"nvpair-shared/noderec"
	"nvpair-ui-broker/relay"
)

// relaySendFunc pushes a full filtered node snapshot down a worker's channel as
// a discovery:nodes notification. The proxy handle sends via its jsonrpc.Peer;
// the notification-only workload/errors/cluster handles send via their Forward
// (stdin) path. The worker replaces its set from the snapshot.
type relaySendFunc func(nodes []noderec.DirectoryNode)

// subscribeRelay registers a worker as a relay.Directory subscriber for the
// given discovery:subscribe params and returns the subscription id plus the
// subscriber handle. The caller owns the id's lifetime (Unsubscribe on worker
// exit / re-subscribe) and must request the initial snapshot via
// dir.Deliver(sub); Deliver schedules the send on the subscriber's pump, which
// captures the snapshot at send time, so the initial delivery can't be
// overtaken by a concurrent Apply and land a stale set.
func subscribeRelay(dir *relay.Directory, params json.RawMessage, send relaySendFunc) (int, *relay.Subscriber, error) {
	var sp noderec.SubscribeParams
	if err := json.Unmarshal(params, &sp); err != nil {
		return 0, nil, err
	}
	sub := &relay.Subscriber{Filter: sp, Send: send}
	id := dir.Subscribe(sub)
	return id, sub, nil
}

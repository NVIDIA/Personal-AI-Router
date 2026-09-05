// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package noderec

import (
	"reflect"
	"testing"
)

func TestLlamaCppServiceKey(t *testing.T) {
	if ServiceLlamaCpp != "ll" {
		t.Fatalf("ServiceLlamaCpp = %q, want ll", ServiceLlamaCpp)
	}
	if got := ServiceLlamaCpp.Transport(); got != TransportPlain {
		t.Fatalf(
			"ServiceLlamaCpp.Transport() = %v, want %v",
			got,
			TransportPlain,
		)
	}
	if ServiceLlamaCpp.UsesMTLS(true) {
		t.Fatal("llama.cpp service must use the proxy's split transport, not noderec-level mTLS")
	}

	record := NodeRecord{
		HostUUID: "host-llamacpp",
		Services: map[ServiceKey]int{
			ServiceNodeInfo: 14318,
			ServiceLlamaCpp: 8081,
		},
	}
	wantTXT := []string{
		"v=1",
		"uuid=host-llamacpp",
		"ni=14318",
		"ll=8081",
	}
	if got := record.TXT(); !reflect.DeepEqual(got, wantTXT) {
		t.Fatalf("TXT() = %v, want %v", got, wantTXT)
	}

	parsed := ParseTXT(record.TXT())
	if port, ok := parsed.Port(ServiceLlamaCpp); !ok || port != 8081 {
		t.Fatalf(
			"llama.cpp port = %d,%v, want 8081,true",
			port,
			ok,
		)
	}
}

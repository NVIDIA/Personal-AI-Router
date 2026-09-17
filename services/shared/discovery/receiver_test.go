// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package discovery

import (
	"net"
	"testing"

	"github.com/miekg/dns"
)

// testReceiver is a receiver with no socket, just the service identity decode needs.
func testReceiver() *receiver {
	return &receiver{service: "_nvpair-node._tcp", domain: "local"}
}

// browseResponse builds a canned mDNS browse response for one instance, shaped like
// the responder's appendBrowseRRs: a PTR in the answer, SRV/TXT (when non-empty) and
// the A records in extra. A ttl of 0 models a goodbye.
func browseResponse(instance, host string, port int, ttl uint32, txt []string, addrs ...string) *dns.Msg {
	svc := "_nvpair-node._tcp.local."
	m := new(dns.Msg)
	m.MsgHdr.Response = true
	m.Compress = true
	m.Answer = append(m.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: svc, Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: ttl},
		Ptr: instance + svc,
	})
	m.Extra = append(m.Extra, &dns.SRV{
		Hdr:    dns.RR_Header{Name: instance + svc, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: ttl},
		Target: host,
		Port:   uint16(port),
	})
	if len(txt) > 0 {
		m.Extra = append(m.Extra, &dns.TXT{
			Hdr: dns.RR_Header{Name: instance + svc, Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: ttl},
			Txt: txt,
		})
	}
	for _, a := range addrs {
		m.Extra = append(m.Extra, &dns.A{
			Hdr: dns.RR_Header{Name: host, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl},
			A:   net.ParseIP(a),
		})
	}
	return m
}

func TestDecodeBuildsNode(t *testing.T) {
	got := testReceiver().decode(browseResponse("mynode", "mynode.local.", 14318, 120, []string{"uuid=abc"}, "192.168.1.10"))
	if len(got) != 1 {
		t.Fatalf("decode returned %d nodes, want 1: %+v", len(got), got)
	}
	n := got[0]
	if n.ID != "mynode" {
		t.Errorf("ID = %q, want mynode", n.ID)
	}
	if n.Host != "mynode.local." {
		t.Errorf("Host = %q, want mynode.local.", n.Host)
	}
	if n.Port != 14318 {
		t.Errorf("Port = %d, want 14318", n.Port)
	}
	if len(n.Addresses) != 1 || n.Addresses[0] != "192.168.1.10" {
		t.Errorf("Addresses = %v, want [192.168.1.10]", n.Addresses)
	}
	if len(n.TXT) != 1 || n.TXT[0] != "uuid=abc" {
		t.Errorf("TXT = %v, want [uuid=abc]", n.TXT)
	}
}

func TestDecodeCollectsMultipleAddresses(t *testing.T) {
	got := testReceiver().decode(browseResponse("mynode", "mynode.local.", 14318, 120, nil, "192.168.1.10", "10.0.0.5"))
	if len(got) != 1 {
		t.Fatalf("decode returned %d nodes, want 1: %+v", len(got), got)
	}
	if len(got[0].Addresses) != 2 {
		t.Fatalf("Addresses = %v, want 2 entries", got[0].Addresses)
	}
}

func TestDecodeDropsGoodbye(t *testing.T) {
	got := testReceiver().decode(browseResponse("mynode", "mynode.local.", 14318, 0, nil, "192.168.1.10"))
	if len(got) != 0 {
		t.Fatalf("decode of a TTL=0 goodbye returned %d nodes, want 0: %+v", len(got), got)
	}
}

func TestDecodeDropsUnresolved(t *testing.T) {
	got := testReceiver().decode(browseResponse("mynode", "mynode.local.", 14318, 120, []string{"uuid=abc"}))
	if len(got) != 0 {
		t.Fatalf("decode without an address returned %d nodes, want 0: %+v", len(got), got)
	}
}

func TestDecodeIgnoresForeignService(t *testing.T) {
	m := new(dns.Msg)
	m.MsgHdr.Response = true
	m.Answer = append(m.Answer, &dns.PTR{
		Hdr: dns.RR_Header{Name: "_nvpair-ollama._tcp.local.", Rrtype: dns.TypePTR, Class: dns.ClassINET, Ttl: 120},
		Ptr: "other._nvpair-ollama._tcp.local.",
	})
	got := testReceiver().decode(m)
	if len(got) != 0 {
		t.Fatalf("decode of a foreign service returned %d nodes, want 0: %+v", len(got), got)
	}
}

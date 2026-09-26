// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDiagnosticMPIGoProfileAndPlanBindInNativePortWithoutEffects(t *testing.T) {
	record, facts, selection, now := mpiPlanFixture(t)
	p, plan, key, err := compileDiagnosticMPIPlan(record, facts, selection, now)
	defer clear(key)
	if err != nil {
		t.Fatal(err)
	}
	profileRaw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	adapterTests, err := os.ReadFile("test_mpi_socket_adapter.py")
	if err != nil {
		t.Fatal(err)
	}
	nativeTests, err := os.ReadFile("test_mpi_socket_native.py")
	if err != nil {
		t.Fatal(err)
	}
	var header diagnosticMPIHeader
	if err := json.Unmarshal(plan, &header); err != nil {
		t.Fatal(err)
	}
	request := diagnosticParticipantRequest{GroupID: header.GroupID, OperationID: header.OperationID, ProfileDigest: header.ProfileDigest, ExpiresAt: header.ExpiresAt, BootstrapPlanDigest: header.PlanDigest, ExecutionDeadlineAt: min(now.Add(diagnosticDeadline).UnixMilli(), header.ExpiresAt)}
	adapter, native, err := canonicalDiagnosticMPIPythonSources(diagnosticMPIAdapterPython, diagnosticMPINativePython)
	if err != nil {
		t.Fatal(err)
	}
	input, err := json.Marshal(map[string]any{"adapter": adapter, "native": native, "adapterTests": string(adapterTests), "nativeTests": string(nativeTests), "profileRaw": string(profileRaw), "plan": json.RawMessage(plan), "request": request, "now": now.UnixMilli()})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(input, key) {
		t.Fatal("private identity entered the native binding fixture")
	}
	python, err := exec.LookPath("python")
	if err != nil {
		t.Fatal("Python is required for the native public-wire binding check")
	}
	code := `import sys,json,types,tempfile
from unittest.mock import patch
x=json.load(sys.stdin)
def module(name,source):
    value=types.ModuleType(name)
    value.SHIPPED_SOURCE=source.encode()
    sys.modules[name]=value
    exec(source,value.__dict__)
    return value
a=module('mpi_socket_adapter',x['adapter'])
n=module('mpi_socket_native',x['native'])
module('test_mpi_socket_adapter',x['adapterTests'])
t=module('test_mpi_socket_native',x['nativeTests'])
p=x['plan']
profile=json.loads(x['profileRaw'])
with patch('subprocess.Popen',side_effect=AssertionError('native processes forbidden')):
    for member in p['members']:
        with tempfile.TemporaryDirectory() as temporary:
            fs=t.SandboxFiles(temporary)
            base=member['home']+'/.config/Nvidia Corporation/Personal AI Router'
            directory=base+'/engine-bin/diagnostic-runs/'+p['operationId']
            request=x['request']
            fs.put(directory+'/profile.json',x['profileRaw'].encode())
            fs.put(directory+'/bootstrap-plan.json',a.canonical(p))
            fs.put(directory+'/lease.json',a.canonical({'request':request,'member':next(m for m in profile['members'] if m['nodeId']==member['nodeId'])}))
            fs.put(base+'/cluster/identity.json',a.canonical({'node_uuid':member['principal']}))
            system=t.FakeSystem(fs,member)
            system.clock=x['now']
            native=n.Native(system,fs,x['native'].encode())
            system.native=native
            native.bind(p)
            assert native.member['nodeId']==member['nodeId']
            assert not system.calls
print('both Go-produced member leases bind in native port; no native effects')
`
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python, "-I", "-c", code)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "SYSTEMROOT=" + os.Getenv("SYSTEMROOT"), "PYTHONDONTWRITEBYTECODE=1"}
	cmd.Stdin = bytes.NewReader(input)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("native public-wire binding failed: %v; %s", err, output)
	}
	program, err := diagnosticMPIProgram()
	if err != nil || len(program) > 120<<10 {
		t.Fatalf("embedded native argument budget failed: %v", err)
	}
}

func TestDiagnosticMPIPythonSourcesCanonicalizeCrossPlatformLineEndings(t *testing.T) {
	lfAdapter, lfNative, err := canonicalDiagnosticMPIPythonSources("adapter\nsource\n", "native\nsource\n")
	if err != nil {
		t.Fatal(err)
	}
	crlfAdapter, crlfNative, err := canonicalDiagnosticMPIPythonSources("adapter\r\nsource\r\n", "native\r\nsource\r\n")
	if err != nil {
		t.Fatal(err)
	}
	if crlfAdapter != lfAdapter || crlfNative != lfNative {
		t.Fatal("LF and CRLF embedded MPI sources produced different canonical bytes")
	}
	if _, _, err := canonicalDiagnosticMPIPythonSources("adapter\rsource\n", "native\n"); err == nil {
		t.Fatal("lone carriage return was accepted in an embedded MPI source")
	}
}

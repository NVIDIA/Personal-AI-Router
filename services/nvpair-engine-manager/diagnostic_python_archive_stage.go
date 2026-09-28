// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0
package main

import (
	"bytes"
	"compress/zlib"
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

//go:embed diagnostic_python_archive_stage.py
var diagnosticPythonArchiveStage string

func diagnosticPythonArchiveStageProgram() (string, error) {
	ending := "\nif __name__ == \"__main__\":\n    main()\n"
	if !strings.HasSuffix(diagnosticPackagesPython, ending) {
		return "", errors.New("fixed diagnostic entrypoint changed")
	}
	program := "import sys,types\n_module=types.ModuleType('diagnostic_tools_remote')\nexec(" + strconv.Quote(diagnosticInspectPython) + ",_module.__dict__)\nsys.modules['diagnostic_tools_remote']=_module\n" + strings.TrimSuffix(diagnosticPackagesPython, ending) + "\n" + diagnosticPythonArchiveStage
	var compressed bytes.Buffer
	writer := zlib.NewWriter(&compressed)
	_, _ = writer.Write([]byte(program))
	_ = writer.Close()
	return "import base64,zlib;exec(zlib.decompress(base64.b64decode(" + strconv.Quote(base64.StdEncoding.EncodeToString(compressed.Bytes())) + ")))", nil
}

func (d *diagnosticService) stagePythonLocalArchives(ctx context.Context, target diagnosticInspectionTarget, access onboardingPrivateTarget) error {
	program, err := diagnosticPythonArchiveStageProgram()
	if err != nil {
		return err
	}
	input, err := json.Marshal(map[string]any{"nodeId": target.NodeID, "principal": target.Principal, "archives": diagnosticPythonLocalArchivePayloads})
	if err != nil || len(input) > 9<<20 {
		return errors.New("closed Python archive staging envelope changed")
	}
	defer clear(input)
	raw, err := d.participantProgram(ctx, target, access, program, input)
	if err != nil {
		return err
	}
	var receipt struct {
		State, Metadata string
		ErrorCode       string
		Identity        *diagnosticPackageIdentity
		Archives        map[string]string
	}
	if json.Unmarshal(raw, &receipt) != nil {
		return errors.New("invalid fixed archive staging response")
	}
	if receipt.State == "blocked" {
		switch receipt.ErrorCode {
		case "local_archive_stage_invalid", "local_archive_stage_changed", "local_archive_stage_failed", "local_archive_changed", "account_invalid", "identity_mismatch", "platform_unsupported":
			return fmt.Errorf("Python archive staging blocked: %s", receipt.ErrorCode)
		default:
			return errors.New("Python archive staging blocked; native observation unavailable")
		}
	}
	if receipt.State != "staged" || receipt.Metadata != "verified_local_archive" || receipt.Identity == nil || receipt.Identity.UID <= 0 || receipt.Identity.NodeID != target.NodeID || receipt.Identity.Principal != target.Principal || len(receipt.Archives) != 3 {
		return errors.New("fixed normal-UID Python archive staging was not confirmed")
	}
	for name, artifact := range diagnosticPythonSnapshotArtifacts {
		if receipt.Archives[name] != artifact.SHA256 {
			return errors.New("staged Python archive digest changed")
		}
	}
	return nil
}

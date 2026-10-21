// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package clustertrust

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// admissionFileName is nvpair-cluster-manager's durable admission record inside
// the cluster dir. Kept in sync with nvpair-cluster-manager/admission_store.go.
const admissionFileName = "admission.json"

// admissionRecord mirrors the subset of admission.json that answers "does this
// node currently belong to a cluster". nvpair-cluster-manager owns the full
// schema (a monotonic counter, retirement marker, etc.); the read side only
// needs the active pair. A live admission carries both a clusterId and a nonzero
// epoch; teardown (leave/removal) clears both while deliberately keeping the
// counter, so absence of an active pair is the authoritative "no longer a
// member" signal — even though the node.key/node.crt keypair persists.
type admissionRecord struct {
	ClusterID string `json:"clusterId"`
	Epoch     uint64 `json:"epoch"`
}

// readAdmission returns the durable membership and epoch, or an empty record
// without an error when the file is absent or malformed. Other filesystem errors
// are returned so callers can retain the last known admission during read
// failures. The epoch distinguishes a rejoin using the previous certificate.
func readAdmission(clusterDir string) (admissionRecord, error) {
	if clusterDir == "" {
		return admissionRecord{}, nil
	}
	data, err := os.ReadFile(filepath.Join(clusterDir, admissionFileName))
	if err != nil {
		if os.IsNotExist(err) {
			return admissionRecord{}, nil
		}
		return admissionRecord{}, err
	}
	var rec admissionRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return admissionRecord{}, nil
	}
	return rec, nil
}

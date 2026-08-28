// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package eapnoob

import "sync"

// Association is the persistent EAP-NOOB association created by a successful
// Completion Exchange (RFC 9140, Section 3.4.1). It holds the shared key Kz
// from which application secrets are exported.
type Association struct {
	PeerId       string
	NAI          string
	Cryptosuitep int
	Kz           []byte
}

// Store persists registered associations. Implementations must be safe for
// concurrent use if shared between goroutines.
type Store interface {
	Load(peerId string) (*Association, bool, error)
	Save(a *Association) error
	Delete(peerId string) error
}

// MemoryStore is an in-memory Store suitable for tests and single-process use.
type MemoryStore struct {
	mu   sync.RWMutex
	data map[string]*Association
}

// NewMemoryStore returns an empty in-memory association store.
func NewMemoryStore() *MemoryStore {
	return &MemoryStore{data: make(map[string]*Association)}
}

func (m *MemoryStore) Load(peerId string) (*Association, bool, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	a, ok := m.data[peerId]
	if !ok {
		return nil, false, nil
	}
	cp := *a
	cp.Kz = append([]byte(nil), a.Kz...)
	return &cp, true, nil
}

func (m *MemoryStore) Save(a *Association) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *a
	cp.Kz = append([]byte(nil), a.Kz...)
	m.data[a.PeerId] = &cp
	return nil
}

func (m *MemoryStore) Delete(peerId string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, peerId)
	return nil
}

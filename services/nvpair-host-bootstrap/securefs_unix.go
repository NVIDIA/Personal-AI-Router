// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type nativeSecureFS struct{}

func (nativeSecureFS) EnsurePrivateDirectory(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == "/" {
		return ErrUnsafeState
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		if component == "" || component == "." || component == ".." {
			return ErrUnsafeState
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Mkdir(current, 0700); err != nil {
				return err
			}
			if err := os.Chown(current, 0, 0); err != nil {
				return err
			}
			info, err = os.Lstat(current)
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*unix.Stat_t)
		if !ok ||
			!info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			stat.Uid != 0 ||
			info.Mode().Perm()&0022 != 0 {
			return ErrUnsafeState
		}
	}
	if err := os.Chmod(path, 0700); err != nil {
		return err
	}
	return nil
}

func (nativeSecureFS) ReadNoFollow(
	path string,
	limit int64,
) ([]byte, secureMetadata, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, secureMetadata{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, secureMetadata{}, ErrUnsafeState
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, secureMetadata{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, secureMetadata{}, err
	}
	if int64(len(raw)) > limit {
		return nil, secureMetadata{}, ErrUnsafeState
	}
	metadata := secureMetadata{
		Regular: stat.Mode&unix.S_IFMT == unix.S_IFREG,
		Links:   uint64(stat.Nlink),
		Mode:    fs.FileMode(stat.Mode & 0777),
		Owner:   strconv.FormatUint(uint64(stat.Uid), 10),
	}
	if stat.Uid == 0 {
		metadata.Owner = rootOwner
	}
	return raw, metadata, nil
}

func (filesystem nativeSecureFS) AtomicWriteNoFollow(
	path string,
	data []byte,
	mode fs.FileMode,
) error {
	if mode.Perm() != 0600 {
		return ErrUnsafeState
	}
	if _, metadata, err := filesystem.ReadNoFollow(path, maxStateFileBytes); err == nil {
		if !safeMetadata(metadata) {
			return ErrUnsafeState
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if err := filesystem.EnsurePrivateDirectory(directory); err != nil {
		return err
	}
	file, err := os.CreateTemp(directory, ".nvpair-state-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chown(0, 0); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (filesystem nativeSecureFS) RemoveNoFollow(path string) error {
	_, metadata, err := filesystem.ReadNoFollow(path, maxStateFileBytes)
	if err != nil {
		return err
	}
	if !safeMetadata(metadata) {
		return ErrUnsafeState
	}
	return os.Remove(path)
}

func safeMetadata(metadata secureMetadata) bool {
	return metadata.Regular &&
		!metadata.Symlink &&
		!metadata.Reparse &&
		metadata.Links == 1 &&
		metadata.Mode.Perm() == 0600 &&
		metadata.Owner == rootOwner
}

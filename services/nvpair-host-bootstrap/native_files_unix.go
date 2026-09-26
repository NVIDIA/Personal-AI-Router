// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build darwin || linux

package main

import (
	"errors"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
	"nvpair-shared/hostbootstrap"
)

func inspectHelperEndpointNative(
	request hostbootstrap.Request,
	endpoint localEndpoint,
) (helperComponentState, error) {
	principal, err := resolveReviewedPrincipal(request.Binding.Account.Name)
	if err != nil {
		return helperComponentState{}, err
	}
	info, err := os.Lstat(endpoint.Address)
	if errors.Is(err, fs.ErrNotExist) {
		return helperComponentState{}, nil
	}
	if err != nil {
		return helperComponentState{}, err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Mode()&os.ModeSocket == 0 {
		return helperComponentState{Present: true}, nil
	}
	return helperComponentState{
		Present: true,
		Exact: info.Mode().Perm() == endpoint.Mode.Perm() &&
			stat.Uid == 0 &&
			stat.Gid == principal.GID,
	}, nil
}

func validateReviewedAccount(account hostbootstrap.AccountIdentity) error {
	resolved, err := user.Lookup(account.Name)
	if err != nil {
		return err
	}
	home := filepath.Clean(resolved.HomeDir)
	if account.HomePath != home ||
		account.AuthorizedKeysPath != filepath.Join(home, ".ssh", "authorized_keys") {
		return ErrUnsupportedIdentity
	}
	return nil
}

func resolveReviewedPrincipal(account string) (reviewedPrincipalState, error) {
	resolved, err := user.Lookup(account)
	if err != nil {
		return reviewedPrincipalState{}, err
	}
	uid, err := strconv.ParseUint(resolved.Uid, 10, 32)
	if err != nil {
		return reviewedPrincipalState{}, ErrUnsupportedIdentity
	}
	gid, err := strconv.ParseUint(resolved.Gid, 10, 32)
	if err != nil {
		return reviewedPrincipalState{}, ErrUnsupportedIdentity
	}
	if uid == 0 {
		return reviewedPrincipalState{}, ErrUnsupportedIdentity
	}
	return reviewedPrincipalState{UID: uint32(uid), GID: uint32(gid)}, nil
}

func nativeGroupName(principal reviewedPrincipalState) string {
	return strconv.FormatUint(uint64(principal.GID), 10)
}

func validateAuthorizedKeyRead(path, account string) error {
	principal, err := resolveReviewedPrincipal(account)
	if err != nil {
		return err
	}
	if err := validateAuthorizedKeyAncestors(
		filepath.Dir(path),
		principal,
	); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != principal.UID ||
		stat.Nlink != 1 ||
		info.Mode().Perm()&0022 != 0 {
		return ErrUnsafeState
	}
	return nil
}

func validateAuthorizedKeyAncestors(
	directory string,
	principal reviewedPrincipalState,
) error {
	if !filepath.IsAbs(directory) ||
		filepath.Clean(directory) != directory {
		return ErrUnsafeState
	}
	current := string(filepath.Separator)
	components := strings.Split(
		strings.TrimPrefix(directory, current),
		current,
	)
	for index, component := range components {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*unix.Stat_t)
		if !ok ||
			!info.IsDir() ||
			info.Mode()&os.ModeSymlink != 0 ||
			(stat.Uid != 0 && stat.Uid != principal.UID) ||
			info.Mode().Perm()&0022 != 0 {
			return ErrUnsafeState
		}
		if index == len(components)-1 &&
			stat.Uid != principal.UID {
			return ErrUnsafeState
		}
	}
	return nil
}

func writeAuthorizedKeysNoFollow(
	path string,
	body []byte,
	principal reviewedPrincipalState,
	expected []byte,
	operationID string,
) error {
	directory := filepath.Dir(path)
	if err := rejectUnixSymlinkComponents(directory); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	info, err := os.Lstat(directory)
	if errors.Is(err, fs.ErrNotExist) {
		if err := os.Mkdir(directory, 0700); err != nil {
			return err
		}
		if err := os.Chown(directory, int(principal.UID), int(principal.GID)); err != nil {
			return err
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != principal.UID ||
		info.Mode().Perm()&0022 != 0 {
		return ErrUnsafeState
	}
	if existing, err := os.Lstat(path); err == nil {
		existingStat, ok := existing.Sys().(*unix.Stat_t)
		if !ok ||
			!existing.Mode().IsRegular() ||
			existing.Mode()&os.ModeSymlink != 0 ||
			existingStat.Uid != principal.UID ||
			existingStat.Nlink != 1 ||
			existing.Mode().Perm()&0022 != 0 {
			return ErrUnsafeState
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(directory, ".nvpair-authorized-keys-*")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if err := file.Chmod(0600); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chown(int(principal.UID), int(principal.GID)); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(body); err != nil {
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
	return publishAuthorizedKeysCAS(temporary, path, expected, operationID)
}

func nativeFileLinkCount(_ string, info fs.FileInfo) (uint64, error) {
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok {
		return 0, ErrUnsafeState
	}
	return uint64(stat.Nlink), nil
}

func validateRootDefinition(path string, mode fs.FileMode) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 ||
		info.Mode().Perm() != mode.Perm() {
		return ErrUnsafeState
	}
	return nil
}

func ensureRootDefinitionDirectory(path string) error {
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
			if err := os.Mkdir(current, 0755); err != nil {
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
	return nil
}

func rootDefinitionDirectoryExact(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	stat, ok := info.Sys().(*unix.Stat_t)
	if !ok ||
		!info.IsDir() ||
		info.Mode()&os.ModeSymlink != 0 ||
		stat.Uid != 0 ||
		info.Mode().Perm()&0022 != 0 {
		return false, ErrUnsafeState
	}
	return true, nil
}

func rejectUnixSymlinkComponents(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return ErrUnsafeState
	}
	current := string(filepath.Separator)
	for _, component := range strings.Split(strings.TrimPrefix(path, current), current) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return ErrUnsafeState
		}
	}
	return nil
}

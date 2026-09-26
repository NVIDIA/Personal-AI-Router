// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func publishAuthorizedKeysCAS(temporary, destination string, expected []byte, operationID string) error {
	if !stateHex32.MatchString(operationID) {
		return ErrStateIdentity
	}
	replacement, err := readBoundedRegularFile(temporary, 1<<20)
	if err != nil {
		return err
	}
	adopted := false
	if err := recoverAuthorizedKeysBackup(
		destination,
		operationID,
		func(displaced, current []byte) (bool, error) {
			exact := bytes.Equal(displaced, expected) &&
				bytes.Equal(current, replacement)
			adopted = adopted || exact
			return exact, nil
		},
	); err != nil {
		return err
	}
	if adopted {
		return nil
	}
	directory := filepath.Dir(destination)
	dirfd, err := unix.Open(directory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	tempName, destinationName := filepath.Base(temporary), filepath.Base(destination)
	backupName := destinationName + ".backup-" + operationID
	var stat unix.Stat_t
	statErr := unix.Fstatat(dirfd, destinationName, &stat, unix.AT_SYMLINK_NOFOLLOW)
	switch {
	case statErr == nil:
		if err := unix.Renameat2(dirfd, destinationName, dirfd, backupName, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		backupFD, openErr := unix.Openat(dirfd, backupName, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
		if openErr != nil {
			_ = unix.Renameat2(dirfd, backupName, dirfd, destinationName, unix.RENAME_NOREPLACE)
			return openErr
		}
		backup := os.NewFile(uintptr(backupFD), backupName)
		displaced, readErr := io.ReadAll(io.LimitReader(backup, (1<<20)+1))
		_ = backup.Close()
		if readErr != nil || !bytes.Equal(displaced, expected) {
			restoreErr := unix.Renameat2(dirfd, backupName, dirfd, destinationName, unix.RENAME_NOREPLACE)
			if restoreErr != nil {
				return errors.New("authorized_keys changed; original retained in operation backup")
			}
			return ErrStateIdentity
		}
		if err := unix.Renameat2(dirfd, tempName, dirfd, destinationName, unix.RENAME_NOREPLACE); err != nil {
			_ = unix.Renameat2(dirfd, backupName, dirfd, destinationName, unix.RENAME_NOREPLACE)
			return ErrStateIdentity
		}
		if err := unix.Unlinkat(dirfd, backupName, 0); err != nil {
			return err
		}
	case errors.Is(statErr, unix.ENOENT):
		if len(expected) != 0 {
			return ErrStateIdentity
		}
		if err := unix.Renameat2(dirfd, tempName, dirfd, destinationName, unix.RENAME_NOREPLACE); err != nil {
			return ErrStateIdentity
		}
	default:
		return statErr
	}
	return unix.Fsync(dirfd)
}

func recoverAuthorizedKeysBackup(
	destination string,
	operationID string,
	validate func([]byte, []byte) (bool, error),
) error {
	if !stateHex32.MatchString(operationID) {
		return ErrStateIdentity
	}
	directory := filepath.Dir(destination)
	dirfd, err := unix.Open(directory, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer unix.Close(dirfd)
	destinationName := filepath.Base(destination)
	backupName := destinationName + ".backup-" + operationID
	conflictName := destinationName + ".conflict-" + operationID
	var destinationStat, backupStat, conflictStat unix.Stat_t
	destinationErr := unix.Fstatat(dirfd, destinationName, &destinationStat, unix.AT_SYMLINK_NOFOLLOW)
	backupErr := unix.Fstatat(dirfd, backupName, &backupStat, unix.AT_SYMLINK_NOFOLLOW)
	conflictErr := unix.Fstatat(dirfd, conflictName, &conflictStat, unix.AT_SYMLINK_NOFOLLOW)
	if errors.Is(destinationErr, unix.ENOENT) && backupErr == nil {
		if err := unix.Renameat2(dirfd, backupName, dirfd, destinationName, unix.RENAME_NOREPLACE); err != nil {
			return err
		}
		if err := unix.Fsync(dirfd); err != nil {
			return err
		}
		if conflictErr == nil {
			return ErrStateIdentity
		}
		return nil
	}
	if destinationErr == nil && errors.Is(backupErr, unix.ENOENT) {
		if conflictErr == nil {
			return ErrStateIdentity
		}
		return nil
	}
	if errors.Is(destinationErr, unix.ENOENT) &&
		errors.Is(backupErr, unix.ENOENT) &&
		errors.Is(conflictErr, unix.ENOENT) {
		return nil
	}
	if destinationErr != nil || backupErr != nil || !errors.Is(conflictErr, unix.ENOENT) {
		return ErrStateIdentity
	}
	displaced, err := readAuthorizedKeysAt(dirfd, backupName)
	if err != nil {
		return err
	}
	current, err := readAuthorizedKeysAt(dirfd, destinationName)
	if err != nil {
		return err
	}
	exact, validationErr := validate(displaced, current)
	if validationErr == nil && exact {
		if err := unix.Unlinkat(dirfd, backupName, 0); err != nil {
			return err
		}
		return unix.Fsync(dirfd)
	}
	if err := unix.Renameat2(dirfd, destinationName, dirfd, conflictName, unix.RENAME_NOREPLACE); err != nil {
		return err
	}
	if err := unix.Renameat2(dirfd, backupName, dirfd, destinationName, unix.RENAME_NOREPLACE); err != nil {
		_ = unix.Renameat2(dirfd, conflictName, dirfd, destinationName, unix.RENAME_NOREPLACE)
		return err
	}
	if err := unix.Fsync(dirfd); err != nil {
		return err
	}
	if validationErr != nil {
		return validationErr
	}
	return ErrStateIdentity
}

func readAuthorizedKeysAt(dirfd int, name string) ([]byte, error) {
	fd, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), name)
	if file == nil {
		_ = unix.Close(fd)
		return nil, ErrUnsafeState
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil ||
		stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Nlink != 1 {
		return nil, ErrUnsafeState
	}
	raw, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil || len(raw) > 1<<20 {
		return nil, ErrUnsafeState
	}
	return raw, nil
}

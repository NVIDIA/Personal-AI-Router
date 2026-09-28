// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const cableCleanupLockPath = "/run/lock/nvpair-cable-probe.lock"

func cableCleanupReadFile(name string, limit int) ([]byte, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return cableCleanupReadFD(fd, limit)
}

func cableCleanupReadAt(dir int, name string, limit int) ([]byte, error) {
	fd, err := unix.Openat(dir, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return cableCleanupReadFD(fd, limit)
}

func cableCleanupReadFD(fd, limit int) ([]byte, error) {
	file := os.NewFile(uintptr(fd), "cleanup-metadata")
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	err = errors.Join(err, file.Close())
	if len(data) > limit {
		return nil, cableCleanupFault("inspection-limit")
	}
	return data, err
}

type cableCleanupStat struct {
	pid, parent  int
	start        string
	kernel, dead bool
}

func parseCableCleanupStat(data []byte, pid int) (cableCleanupStat, error) {
	var result cableCleanupStat
	start := bytes.IndexByte(data, '(')
	end := bytes.LastIndexByte(data, ')')
	if start < 1 || end < start {
		return result, cableCleanupFault("proc-unavailable")
	}
	id, err := strconv.Atoi(strings.TrimSpace(string(data[:start])))
	fields := strings.Fields(string(data[end+1:]))
	if err != nil || id != pid || len(fields) < 20 {
		return result, cableCleanupFault("proc-unavailable")
	}
	parent, err := strconv.Atoi(fields[1])
	flags, flagErr := strconv.ParseUint(fields[6], 10, 64)
	ticks, tickErr := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || parent < 0 || flagErr != nil || tickErr != nil {
		return result, cableCleanupFault("proc-unavailable")
	}
	// PF_KTHREAD is the Linux task flag; an unreadable/empty user argv is not
	// evidence that a task is a kernel thread or has exited.
	return cableCleanupStat{pid: pid, parent: parent, start: strconv.FormatUint(ticks, 10), kernel: flags&0x00200000 != 0, dead: fields[0] == "Z" || fields[0] == "X"}, nil
}

func cableCleanupUID(status []byte) (int, error) {
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "Uid:") {
			parts := strings.Fields(line)
			if len(parts) != 5 {
				return 0, cableCleanupFault("proc-unavailable")
			}
			uid, err := strconv.Atoi(parts[2])
			if err != nil || uid < 0 {
				return 0, cableCleanupFault("proc-unavailable")
			}
			return uid, nil
		}
	}
	return 0, cableCleanupFault("proc-unavailable")
}

func nativeCableCleanupProcess(ctx context.Context, pid int) (cableCleanupProcess, error) {
	result := cableCleanupProcess{PID: pid}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if pid < 1 || pid > 2147483647 {
		return result, cableCleanupFault("proc-unavailable")
	}
	dir, err := unix.Open("/proc/"+strconv.Itoa(pid), unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if errors.Is(err, unix.ENOENT) {
		result.Gone = true
		return result, nil
	}
	if err != nil {
		return result, cableCleanupFault("candidate-unreadable")
	}
	defer unix.Close(dir)
	statData, err := cableCleanupReadAt(dir, "stat", 8192)
	if errors.Is(err, unix.ENOENT) {
		result.Gone = true
		return result, nil
	}
	if err != nil {
		return result, cableCleanupFault("candidate-unreadable")
	}
	stat, err := parseCableCleanupStat(statData, pid)
	if err != nil {
		return result, err
	}
	result.Parent, result.StartTicks, result.Kernel, result.Gone = stat.parent, stat.start, stat.kernel, stat.dead
	result.Bytes = len(statData)
	if result.Kernel || result.Gone {
		return result, nil
	}
	// This proc directory FD remains tied to the original process generation.
	// ENOENT from its stat after a metadata failure proves that generation gone;
	// a denied read or a live unreadable task never becomes absence.
	failed := func() (cableCleanupProcess, error) {
		after, e := cableCleanupReadAt(dir, "stat", 8192)
		if errors.Is(e, unix.ENOENT) {
			result.Gone = true
			return result, nil
		}
		if e == nil {
			if state, e := parseCableCleanupStat(after, pid); e == nil && state.dead {
				result.Gone = true
				return result, nil
			}
		}
		return result, cableCleanupFault("candidate-unreadable")
	}
	link := make([]byte, 4097)
	n, err := unix.Readlinkat(dir, "exe", link)
	if err != nil {
		return failed()
	}
	if n >= len(link) {
		return result, cableCleanupFault("inspection-limit")
	}
	result.Executable = string(link[:n])
	result.Bytes += n
	args, err := cableCleanupReadAt(dir, "cmdline", cableCleanupArgLimit)
	if err != nil {
		if errors.Is(err, cableCleanupFault("inspection-limit")) {
			return result, err
		}
		return failed()
	}
	result.Bytes += len(args)
	if len(args) == 0 || args[len(args)-1] != 0 {
		return failed()
	}
	for _, arg := range bytes.Split(args[:len(args)-1], []byte{0}) {
		result.Args = append(result.Args, string(arg))
	}
	status, err := cableCleanupReadAt(dir, "status", 16<<10)
	if err != nil {
		return failed()
	}
	result.Bytes += len(status)
	result.UID, err = cableCleanupUID(status)
	if err != nil {
		return result, err
	}
	after, err := cableCleanupReadAt(dir, "stat", 8192)
	if err != nil {
		return failed()
	}
	result.Bytes += len(after)
	last, err := parseCableCleanupStat(after, pid)
	if err != nil || last.start != stat.start {
		return result, cableCleanupFault("candidate-ambiguous")
	}
	if last.dead {
		result.Gone = true
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func nativeCableCleanupProcesses(ctx context.Context) ([]int, error) {
	file, err := os.Open("/proc")
	if err != nil {
		return nil, cableCleanupFault("proc-unavailable")
	}
	defer file.Close()
	pids := []int{}
	entries := 0
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rows, err := file.ReadDir(128)
		if err != nil && err != io.EOF {
			return nil, cableCleanupFault("proc-unavailable")
		}
		entries += len(rows)
		if entries > cableCleanupProcessLimit+512 {
			return nil, cableCleanupFault("inspection-limit")
		}
		for _, row := range rows {
			pid, e := strconv.Atoi(row.Name())
			if e != nil || pid < 1 {
				continue
			}
			pids = append(pids, pid)
			if len(pids) > cableCleanupProcessLimit {
				return nil, cableCleanupFault("inspection-limit")
			}
		}
		if err == io.EOF {
			return pids, nil
		}
	}
}

func cableCleanupProcProfile() error {
	var fs unix.Statfs_t
	if unix.Statfs("/proc", &fs) != nil || uint64(fs.Type) != uint64(unix.PROC_SUPER_MAGIC) {
		return cableCleanupFault("unsupported")
	}
	uidMap, err := cableCleanupReadFile("/proc/self/uid_map", 256)
	if err != nil || strings.Join(strings.Fields(string(uidMap)), " ") != "0 0 4294967295" {
		return cableCleanupFault("unsupported")
	}
	mounts, err := cableCleanupReadFile("/proc/self/mountinfo", 256<<10)
	if err != nil {
		return cableCleanupFault("unsupported")
	}
	return cableCleanupProcMounts(mounts)
}

func cableCleanupProcMounts(mounts []byte) error {
	matched := 0
	for _, line := range strings.Split(string(mounts), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 {
			continue
		}
		if fields[4] != "/proc" {
			continue
		}
		matched++
		separator := -1
		for i, field := range fields {
			if field == "-" {
				separator = i
				break
			}
		}
		if fields[3] != "/" || separator < 6 || len(fields) <= separator+3 || fields[separator+1] != "proc" {
			return cableCleanupFault("unsupported")
		}
		for _, option := range strings.Split(fields[5]+","+fields[separator+3], ",") {
			if (strings.HasPrefix(option, "hidepid=") && option != "hidepid=0") || strings.HasPrefix(option, "subset=") {
				return cableCleanupFault("unsupported")
			}
		}
	}
	if matched != 1 {
		return cableCleanupFault("unsupported")
	}
	return nil
}

func nativeCableCleanupScope(pid int) (cableCleanupScope, error) {
	var scope cableCleanupScope
	if err := cableCleanupProcProfile(); err != nil {
		return scope, err
	}
	base := "/proc/" + strconv.Itoa(pid)
	data, err := cableCleanupReadFile(base+"/stat", 8192)
	if err != nil {
		return scope, cableCleanupFault("scope-mismatch")
	}
	stat, err := parseCableCleanupStat(data, pid)
	if err != nil || stat.dead || stat.kernel {
		return scope, cableCleanupFault("scope-mismatch")
	}
	boot, err := cableCleanupReadFile("/proc/sys/kernel/random/boot_id", 64)
	if err != nil {
		return scope, cableCleanupFault("scope-mismatch")
	}
	scope.PID, scope.StartTicks, scope.BootID = pid, stat.start, strings.TrimSpace(string(boot))
	for _, entry := range []struct {
		name string
		out  *string
	}{{"pid", &scope.PIDNS}, {"mnt", &scope.MountNS}, {"net", &scope.NetNS}, {"user", &scope.UserNS}} {
		value, err := os.Readlink(base + "/ns/" + entry.name)
		if err != nil || len(value) > 4096 {
			return scope, cableCleanupFault("scope-mismatch")
		}
		*entry.out = value
	}
	after, err := cableCleanupReadFile(base+"/stat", 8192)
	if err != nil {
		return scope, cableCleanupFault("scope-mismatch")
	}
	last, err := parseCableCleanupStat(after, pid)
	if err != nil || last.start != stat.start || !cableCleanupScopeValid(scope) {
		return scope, cableCleanupFault("scope-mismatch")
	}
	return scope, nil
}

// This qualifies the current proc/native deployment view; it is not a claim
// about arbitrary containers or the old operation's unrecorded namespace.
func cableCleanupHostScope() (*cableCleanupScope, error) {
	if os.Getuid() <= 0 || os.Geteuid() != os.Getuid() {
		return nil, cableCleanupFault("unsupported")
	}
	scope, err := nativeCableCleanupScope(os.Getpid())
	if err != nil {
		return nil, err
	}
	return &scope, nil
}

func nativeCableCleanupDigest(ctx context.Context, pid int) (string, error) {
	file, err := os.Open("/proc/" + strconv.Itoa(pid) + "/exe")
	if err != nil {
		return "", cableCleanupFault("normal-process-changed")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() <= 0 || stat.Size() > cableWorkerMaxBytes {
		return "", cableCleanupFault("normal-process-changed")
	}
	hash := sha256.New()
	buffer := make([]byte, 64<<10)
	var total int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := file.Read(buffer)
		total += int64(n)
		if total > cableWorkerMaxBytes {
			return "", cableCleanupFault("inspection-limit")
		}
		_, _ = hash.Write(buffer[:n])
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", cableCleanupFault("normal-process-changed")
		}
	}
	if total != stat.Size() {
		return "", cableCleanupFault("normal-process-changed")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func nativeCableCleanupVerify(ctx context.Context, request cableCleanupRequest) (cableCleanupScope, cableCleanupProcess, error) {
	var zero cableCleanupScope
	var empty cableCleanupProcess
	if os.Geteuid() != 0 {
		return zero, empty, cableCleanupFault("unsupported")
	}
	if err := ctx.Err(); err != nil {
		return zero, empty, err
	}
	normal, err := nativeCableCleanupScope(request.Scope.PID)
	if err != nil {
		return zero, empty, err
	}
	selfScope, err := nativeCableCleanupScope(os.Getpid())
	if err != nil {
		return zero, empty, err
	}
	if normal != request.Scope || !cableCleanupSameNamespace(normal, selfScope) {
		return zero, empty, cableCleanupFault("scope-mismatch")
	}
	normalProcess, err := nativeCableCleanupProcess(ctx, normal.PID)
	if err != nil {
		return zero, empty, err
	}
	if normalProcess.Gone || normalProcess.UID != request.UID || normalProcess.StartTicks != normal.StartTicks || cableCleanupCandidate(normalProcess) != "" {
		return zero, empty, cableCleanupFault("normal-process-changed")
	}
	self, err := nativeCableCleanupProcess(ctx, os.Getpid())
	if err != nil {
		return zero, empty, err
	}
	if self.Gone || self.UID != 0 || len(self.Args) != 8 || self.Args[0] != "nvpair-engine-manager" || self.Args[1] != cableCleanupInspectorFlag || self.Args[2] != "--node-id" || self.Args[3] != request.NodeID || self.Args[4] != "--node-info-port" || self.Args[5] != "14318" || self.Args[6] != "--cluster-dir" || !strings.HasPrefix(self.Args[7], "/") {
		return zero, empty, cableCleanupFault("scope-mismatch")
	}
	for _, pid := range []int{normal.PID, self.PID} {
		digest, err := nativeCableCleanupDigest(ctx, pid)
		if err != nil {
			return zero, empty, err
		}
		if digest != request.WorkerSHA256 {
			return zero, empty, cableCleanupFault("normal-process-changed")
		}
	}
	after, err := nativeCableCleanupScope(normal.PID)
	if err != nil || after != normal {
		return zero, empty, cableCleanupFault("scope-mismatch")
	}
	selfAfter, err := nativeCableCleanupScope(self.PID)
	if err != nil || selfAfter != selfScope {
		return zero, empty, cableCleanupFault("scope-mismatch")
	}
	return normal, self, ctx.Err()
}

func cableCleanupLockStat(stat unix.Stat_t) bool {
	return stat.Mode&unix.S_IFMT == unix.S_IFREG && stat.Mode&0777 == 0600 && stat.Uid == 0 && stat.Nlink == 1 && stat.Size == 0
}

func nativeCableCleanupCheckLock(ctx context.Context, lock cableCleanupLock) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var fdStat, pathStat unix.Stat_t
	if unix.Fstat(lock.FD, &fdStat) != nil || unix.Lstat(cableCleanupLockPath, &pathStat) != nil || !cableCleanupLockStat(fdStat) || !cableCleanupLockStat(pathStat) || uint64(fdStat.Dev) != lock.Device || fdStat.Ino != lock.Inode || fdStat.Dev != pathStat.Dev || fdStat.Ino != pathStat.Ino {
		return cableCleanupFault("lock-changed")
	}
	return nil
}

func nativeCableCleanupLock(ctx context.Context) (cableCleanupLock, error) {
	var lock cableCleanupLock
	if err := ctx.Err(); err != nil {
		return lock, err
	}
	if os.Geteuid() != 0 {
		return lock, cableCleanupFault("unsupported")
	}
	for _, parent := range []string{"/run", "/run/lock"} {
		var stat unix.Stat_t
		if unix.Lstat(parent, &stat) != nil || stat.Mode&unix.S_IFMT != unix.S_IFDIR || stat.Uid != 0 || (stat.Mode&0022 != 0 && stat.Mode&unix.S_ISVTX == 0) {
			return lock, cableCleanupFault("lock-unavailable")
		}
	}
	fd, err := unix.Open(cableCleanupLockPath, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0)
	if err != nil {
		return lock, cableCleanupFault("lock-unavailable")
	}
	fail := func(code string) (cableCleanupLock, error) {
		if unix.Close(fd) != nil {
			return lock, cableCleanupFault("close-unconfirmed")
		}
		return lock, cableCleanupFault(code)
	}
	var stat unix.Stat_t
	if unix.Fstat(fd, &stat) != nil || !cableCleanupLockStat(stat) {
		return fail("lock-unavailable")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return fail("lock-busy")
		}
		return fail("lock-unavailable")
	}
	lock = cableCleanupLock{FD: fd, Device: uint64(stat.Dev), Inode: stat.Ino}
	if err := nativeCableCleanupCheckLock(ctx, lock); err != nil {
		return fail(cableCleanupCode(err))
	}
	return lock, nil
}

func nativeCableCleanupIO() cableCleanupIO {
	return cableCleanupIO{verify: nativeCableCleanupVerify, processes: nativeCableCleanupProcesses, process: nativeCableCleanupProcess, lock: nativeCableCleanupLock, checkLock: nativeCableCleanupCheckLock, close: func(lock cableCleanupLock) error { return unix.Close(lock.FD) }}
}

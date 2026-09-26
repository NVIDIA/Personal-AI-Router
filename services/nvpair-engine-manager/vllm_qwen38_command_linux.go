//go:build linux

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const qwen38BoundedCommandWorker = `import base64,json,os,signal,stat,subprocess,sys,time
p=json.loads(base64.b64decode(sys.argv[1]))
stage=p['stage']; roots=[stage,p['bundle']]; cap=p['stageMax']; reserve=p['reserve']; device=os.lstat(stage).st_dev
if os.lstat(p['bundle']).st_dev!=device: raise RuntimeError('stage device changed')
def check():
 total=0
 for root in roots:
  total+=max(os.lstat(root).st_blocks*512,4096)
  for base,dirs,files in os.walk(root,followlinks=False):
   for name in dirs+files:
    path=os.path.join(base,name)
    try: s=os.lstat(path)
    except FileNotFoundError: continue
    if s.st_dev!=device: raise RuntimeError('stage device changed')
    if stat.S_ISLNK(s.st_mode):
     if root!=stage or path!=os.path.join(stage,'venv','lib64') or os.readlink(path)!='lib': raise RuntimeError('owned stage changed')
    elif name in files and (not stat.S_ISREG(s.st_mode) or s.st_nlink!=1): raise RuntimeError('owned stage changed')
    total+=max(s.st_blocks*512,4096)
 if total>cap: raise RuntimeError('stage cap exceeded')
 fs=os.statvfs(root)
 if fs.f_bavail*fs.f_frsize<reserve: raise RuntimeError('free reserve crossed')
check()
child=subprocess.Popen(p['command'],cwd=p['cwd'],env=p['env'],start_new_session=True)
failure=None
while child.poll() is None:
 try: check()
 except Exception as exc:
  failure=str(exc)
  try: os.killpg(child.pid,signal.SIGTERM)
  except ProcessLookupError: pass
  try: child.wait(5)
  except subprocess.TimeoutExpired:
   try: os.killpg(child.pid,signal.SIGKILL)
   except ProcessLookupError: pass
   child.wait()
  break
 time.sleep(.25)
if failure:
 print('PAIR_QWEN38_LIMIT:'+failure,file=sys.stderr); raise SystemExit(90)
check(); raise SystemExit(child.returncode)
`

func qwen38BoundedCommand(bin string, args, childEnv []string, dir string, policy vllmQwen38CommandPolicy) (string, []string, []string, error) {
	for _, path := range []string{"/usr/bin/systemd-run", "/usr/bin/systemctl", "/usr/bin/python3"} {
		if info, err := os.Stat(path); err != nil || !info.Mode().IsRegular() {
			return "", nil, nil, errors.New("Qwen3.8 bounded transient-unit owner is unavailable")
		}
	}
	env := map[string]string{}
	for _, entry := range childEnv {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" || env[key] != "" {
			return "", nil, nil, errors.New("Qwen3.8 child environment is invalid")
		}
		env[key] = value
	}
	payload, err := json.Marshal(struct {
		Command  []string          `json:"command"`
		Env      map[string]string `json:"env"`
		CWD      string            `json:"cwd"`
		Stage    string            `json:"stage"`
		Bundle   string            `json:"bundle"`
		StageMax uint64            `json:"stageMax"`
		Reserve  uint64            `json:"reserve"`
	}{append([]string{bin}, args...), env, dir, policy.StageRoot, policy.BundleRoot, policy.StageMaxBytes, policy.FreeReserveBytes})
	if err != nil {
		return "", nil, nil, err
	}
	argv := []string{"--user", "--wait", "--pipe", "--collect", "--quiet", "--service-type=exec", "--unit=" + policy.Unit, "--description=NVPAIR Qwen3.8 " + policy.Unit, "--property=MemoryAccounting=yes", "--property=TasksAccounting=yes", "--property=MemoryMax=" + strconv.FormatUint(policy.MemoryMaxBytes, 10), "--property=TasksMax=" + strconv.Itoa(policy.TasksMax), "--property=RuntimeMaxSec=" + strconv.FormatInt(int64(policy.Timeout.Seconds()), 10) + "s", "--property=KillMode=control-group", "--property=TimeoutStopSec=10s", "--property=SendSIGKILL=yes", "--property=CollectMode=inactive-or-failed", "--working-directory=" + dir, "/usr/bin/python3", "-I", "-B", "-c", qwen38BoundedCommandWorker, base64.StdEncoding.EncodeToString(payload)}
	uid := strconv.Itoa(os.Getuid())
	wrapperEnv := []string{"PATH=/usr/bin:/bin", "HOME=" + env["HOME"], "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/" + uid, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus"}
	return "/usr/bin/systemd-run", argv, wrapperEnv, nil
}

func verifyQwen38BoundedCommand(ctx context.Context, policy vllmQwen38CommandPolicy) error {
	uid := strconv.Itoa(os.Getuid())
	env := []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "LC_ALL=C.UTF-8", "XDG_RUNTIME_DIR=/run/user/" + uid, "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + uid + "/bus"}
	cleanupCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	show := func() (string, error) {
		command := exec.CommandContext(cleanupCtx, "/usr/bin/systemctl", "--user", "show", policy.Unit, "-p", "Description", "-p", "LoadState", "-p", "ActiveState", "-p", "SubState", "-p", "TasksCurrent", "-p", "MemoryCurrent")
		command.Env = env
		raw, err := command.Output()
		return string(raw), err
	}
	text, err := show()
	if err == nil && !strings.Contains(text, "LoadState=not-found") {
		if !strings.Contains(text, "Description=NVPAIR Qwen3.8 "+policy.Unit) {
			return errors.New("Qwen3.8 transient unit identity changed; refusing foreign cleanup")
		}
		stop := exec.CommandContext(cleanupCtx, "/usr/bin/systemctl", "--user", "stop", policy.Unit)
		stop.Env = env
		if stop.Run() != nil {
			return errors.New("Qwen3.8 transient install cgroup could not be stopped")
		}
		reset := exec.CommandContext(cleanupCtx, "/usr/bin/systemctl", "--user", "reset-failed", policy.Unit)
		reset.Env = env
		_ = reset.Run()
		text, err = show()
	}
	if err != nil || !strings.Contains(text, "LoadState=not-found") || !strings.Contains(text, "ActiveState=inactive") || !strings.Contains(text, "SubState=dead") || !strings.Contains(text, "TasksCurrent=[not set]") || !strings.Contains(text, "MemoryCurrent=[not set]") {
		return fmt.Errorf("Qwen3.8 transient install cgroup cleanup is unconfirmed")
	}
	return nil
}

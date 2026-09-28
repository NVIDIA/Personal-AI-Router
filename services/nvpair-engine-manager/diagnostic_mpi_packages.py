# SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
# SPDX-License-Identifier: Apache-2.0
"""PAIR's fixed Ubuntu ARM64 OpenMPI package review/install. No GPU launch.

Uses system python3-apt's in-memory resolver and existing authenticated metadata.
It never refreshes APT metadata, changes repositories or installs python3-apt.
"""

import hashlib
import base64
from contextlib import contextmanager
import importlib.util
import json
import os
import platform
import re
import selectors
import signal
import stat
import subprocess
import sys
import time
from pathlib import Path
from urllib.parse import urlsplit

from diagnostic_tools_remote import Native, ProbeError, MPI_PACKAGES, TOKEN, decode, parse_os_release, probe_parent_death

RECIPE = "dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v3"
UPGRADE_RECIPE = "dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v4"
DEPENDENCY_UPGRADES = {
    **{name: ("13.3.0-6ubuntu2~24.04", "13.3.0-6ubuntu2~24.04.1") for name in (
        "cpp-13", "cpp-13-aarch64-linux-gnu", "g++-13", "g++-13-aarch64-linux-gnu",
        "gcc-13", "gcc-13-aarch64-linux-gnu", "gcc-13-base", "libgcc-13-dev",
        "libobjc-13-dev", "libstdc++-13-dev")},
    **{name: ("2.1.12-stable-9ubuntu2", "2.1.12-stable-9ubuntu2.1") for name in (
        "libevent-2.1-7t64", "libevent-core-2.1-7t64")},
    **{name: ("2.0.18-1build1", "2.0.18-1ubuntu0.24.04.1") for name in ("libnuma1", "numactl")},
}
DEPENDENCY_CANDIDATES = {
    **{name: (versions[1], "arm64") for name, versions in DEPENDENCY_UPGRADES.items()},
    **{name: ("13.3.0-6ubuntu2~24.04.1", "arm64") for name in (
        "gfortran-13", "gfortran-13-aarch64-linux-gnu", "libgfortran-13-dev")},
    **{name: ("2.1.12-stable-9ubuntu2.1", "arm64") for name in (
        "libevent-dev", "libevent-extra-2.1-7t64", "libevent-openssl-2.1-7t64", "libevent-pthreads-2.1-7t64")},
    **{name: ("2.10.0-1build1", "arm64") for name in ("libhwloc-dev", "libhwloc-plugins", "libhwloc15")},
    **{name: ("4.1.6-7ubuntu2", "arm64") for name in ("libopenmpi-dev", "libopenmpi3t64", "openmpi-bin")},
    **{name: ("5.0.1-4.1build1", "arm64") for name in ("libpmix-dev", "libpmix2t64")},
    "libfabric1": ("1.17.0-3build2", "arm64"),
    "libjs-jquery-ui": ("1.13.2+dfsg-1", "all"),
    "libmunge2": ("0.5.15-4ubuntu0.1", "arm64"),
    "libnuma-dev": ("2.0.18-1ubuntu0.24.04.1", "arm64"),
    "openmpi-common": ("4.1.6-7ubuntu2", "all"),
}
RISKY_PACKAGE_PREFIXES = ("linux-", "nvidia-", "libnvidia-", "cuda-", "libcuda", "firmware-",
                          "grub-", "shim-", "initramfs-", "systemd", "udev", "libc6", "libc-bin", "libc-dev")
RISKY_PACKAGES = {"linux", "linux-firmware", "firmware", "intel-microcode", "amd64-microcode",
                  "openssh-server", "network-manager", "netplan.io", "init", "sysvinit-core"}
ROOT_PACKAGE = "libopenmpi-dev"
PYTHON_RECIPE = "dgx-spark-ubuntu24.04-arm64-python312-headers-v1"
PYTHON_ROOT_PACKAGE = "python3.12-dev"
PYTHON_PACKAGES = {
    "python3.12-dev": ("3.12.3-1ubuntu0.16", "arm64"),
    "libpython3.12-dev": ("3.12.3-1ubuntu0.16", "arm64"),
    "libexpat1-dev": ("2.6.1-2ubuntu0.4", "arm64"),
}
MAX_PACKAGES = 128
MAX_DOWNLOAD = 256 << 20
MAX_INSTALLED = 1 << 30
MAX_PLAN_BYTES = 16 << 10
MAX_ATTEMPTS = 3
PACKAGE_NAME = re.compile(r"[a-z0-9][a-z0-9+.-]{0,127}\Z")
VERSION = re.compile(r"[0-9][A-Za-z0-9.+:~_-]{0,127}\Z")
UBUNTU_SITES = {"ports.ubuntu.com", "archive.ubuntu.com", "security.ubuntu.com"}
UBUNTU_ARCHIVES = {"noble", "noble-updates", "noble-security"}
MAX_INSTALL_SECONDS = 180
JOURNAL_OWNER = "pair-mpi-packages-v1"
UNIT_POLICY = {"type": "exec", "exitType": "cgroup", "runtimeMaxSec": 180, "timeoutStartSec": 10, "timeoutStopSec": 10,
               "killMode": "control-group", "restart": "no", "standardInput": "null",
               "remainAfterExit": True, "collectMode": "inactive",
               "runtimeDirectoryMode": "0700", "runtimeDirectoryPreserve": "yes"}
EFFECTS = ["Install only the listed missing Ubuntu packages and their listed dependencies.",
           "APT package scripts may run during installation; no OS or driver upgrade is approved.",
           "Create one temporary administrator systemd service for this exact operation, with 10-second startup, 180-second runtime, whole-cgroup stop and 10-second kill escalation.",
           "Keep only completed unit metadata until its owned result and empty cgroup are recorded; stop/reset only that exact transient unit to collect it.",
           "Download the exact closure into this operation's root-owned private /run cache, verify actual .deb bytes and package identities, then install with downloads disabled.",
           "Archive acquisition runs as root inside the reviewed administrator unit and private cache; the APT sandbox-user override applies only to these fixed commands.",
           "A retry may download the original approved archives again; its installation step does not reinstall already-completed packages.",
           "Preserve the measured archive receipt before deleting only this owned unit's private runtime cache; never clear the global APT cache.",
           "Existing shared packages are preserved; no automatic package rollback or autoremove."]
UPGRADE_EFFECTS = [
    "Install only the listed missing OpenMPI prerequisites and the separately approved exact dependency upgrades.",
    "Standard trusted Ubuntu package scripts and triggers may reload or restart services. This recipe requests no reboot, unrelated-service restart, or startup-policy change.",
    "Use noninteractive needrestart list-only mode; this does not suppress every maintainer-script or trigger effect.",
    *EFFECTS[2:8],
    "Preserve conffiles and unlisted packages; no automatic package rollback or autoremove.",
]
PYTHON_EFFECTS = [
    "Install only the listed matching Python 3.12 development packages from trusted Ubuntu 24.04 metadata.",
    "No upgrades, removals, driver changes, model actions or engine restart are included.",
    *EFFECTS[2:8],
    "Keep installed development packages; no automatic rollback or autoremove.",
    "Recheck the same managed interpreter as its normal user only after the administrator unit has ended and cleanup is confirmed.",
]
PYTHON_INSTALL_EFFECTS = [
    *PYTHON_EFFECTS[:-1],
    "Recheck the fixed system Python as its normal user after administrator cleanup; confirm no managed runtime exists, then use normal Install.",
]

# Executed only by the reviewed root transient unit or its exact receipt/cleanup
# actions. No shell, caller-selected paths, credential input or generic commands.
ARCHIVE_WORKER = r'''
import base64,hashlib,json,os,re,shutil,stat,subprocess,sys
from pathlib import Path
LIMIT=268435456
class Failure(Exception): pass
def enc(x): return json.dumps(x,sort_keys=True,separators=(',',':')).encode()
def empty_group(base):
 group=Path('/sys/fs/cgroup/system.slice')/(base.name+'.service')
 if not Path('/sys/fs/cgroup/cgroup.controllers').is_file(): raise Failure('cgroup_unknown')
 if group.exists() and not re.search(r'^populated 0$',(group/'cgroup.events').read_text(),re.M): raise Failure('cgroup_populated')
def regular(path,limit):
 fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb') as f:
  s=os.fstat(f.fileno())
  if not stat.S_ISREG(s.st_mode) or s.st_uid!=0 or s.st_mode&0o022 or s.st_size>limit: raise Failure('unsafe_archive_file')
  return f.read(limit+1)
def save(base,name,data):
 p=base/(name+'.tmp'); fd=os.open(p,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
 with os.fdopen(fd,'wb') as f: f.write(enc(data)); f.flush(); os.fsync(f.fileno())
 os.replace(p,base/name)
def verify(cache,expected):
 remaining={(e['sha256'],e['size']):e for e in expected}; measured=[]
 if len(remaining)!=len(expected): raise Failure('duplicate_approved_archive')
 for path in sorted(cache.iterdir()):
  if path.name=='lock':
   regular(path,4096); continue
  if path.name=='partial':
   s=path.lstat()
   if not stat.S_ISDIR(s.st_mode) or s.st_uid!=0 or list(path.iterdir()): raise Failure('unexpected_partial_archive')
   continue
  if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_.+%:~@-]*\.deb',path.name): raise Failure('extra_archive')
  fd=os.open(path,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(fd,'rb') as f:
   s=os.fstat(f.fileno())
   if not stat.S_ISREG(s.st_mode) or s.st_uid!=0 or s.st_mode&0o022 or s.st_size>LIMIT: raise Failure('unsafe_archive_file')
   h=hashlib.sha256(); size=0
   for part in iter(lambda:f.read(1048576),b''):
    size+=len(part)
    if size>LIMIT: raise Failure('archive_size_mismatch')
    h.update(part)
  sha=h.hexdigest(); entry=remaining.pop((sha,size),None)
  if entry is None: raise Failure('archive_hash_or_size_mismatch')
  p=subprocess.run(['/usr/bin/dpkg-deb','--field',str(path),'Package','Version','Architecture'],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5)
  if p.returncode or len(p.stdout)>4096: raise Failure('archive_control_invalid')
  fields={}
  for line in p.stdout.decode('utf-8').splitlines():
   key,sep,value=line.partition(':')
   if not sep or key in fields: raise Failure('archive_control_invalid')
   fields[key]=value.strip()
  if fields!={'Package':entry['name'],'Version':entry['version'],'Architecture':entry['architecture']}: raise Failure('archive_identity_mismatch')
  measured.append(dict(name=fields['Package'],version=fields['Version'],architecture=fields['Architecture'],fileName=path.name,size=size,sha256=sha))
 if remaining: raise Failure('missing_archive')
 return sorted(measured,key=lambda e:e['name'])
def main(m):
 if os.geteuid()!=0 or not re.fullmatch('[a-f0-9]{32}',m.get('operationId','')) or not re.fullmatch('[a-f0-9]{64}',m.get('planDigest','')): raise Failure('worker_binding_invalid')
 if m.get('action') not in ('install','read-receipt','cleanup'): raise Failure('worker_action_invalid')
 base=Path('/run')/('pair-nccl-mpi-'+m['operationId'])
 marker=dict(operationId=m['operationId'],planDigest=m['planDigest'])
 def missing(): return dict(schemaVersion=1,operationId=m['operationId'],planDigest=m['planDigest'],workerSha256=hashlib.sha256(_workerSource).hexdigest(),state='failed',phase='download',verified=False,downloadExitCode=None,installExitCode=None,archives=[],errorCode='archive_receipt_missing')
 try: s=base.lstat()
 except FileNotFoundError:
  if m['action']=='read-receipt': return missing()
  if m['action']=='cleanup':
   empty_group(base); return dict(operationId=m['operationId'],planDigest=m['planDigest'],cacheRemoved=True)
  raise Failure('runtime_directory_missing')
 if not stat.S_ISDIR(s.st_mode) or s.st_uid!=0 or stat.S_IMODE(s.st_mode)!=0o700: raise Failure('unsafe_runtime_directory')
 if m['action']!='install':
  empty=not list(base.iterdir())
  if not empty and json.loads(regular(base/'owner.json',4096))!=marker: raise Failure('cache_owner_mismatch')
  if m['action']=='read-receipt':
   if empty: return missing()
   try: return json.loads(regular(base/'receipt.json',98304))
   except FileNotFoundError: return missing()
  empty_group(base)
  if empty:
   base.rmdir(); return dict(operationId=m['operationId'],planDigest=m['planDigest'],cacheRemoved=True)
  if any(p.is_symlink() for p in base.rglob('*')): raise Failure('unsafe_cache_entry')
  shutil.rmtree(base)
  return dict(operationId=m['operationId'],planDigest=m['planDigest'],cacheRemoved=True)
 entries=m.get('packages',[])
 if not 0<len(entries)<=128 or sum(e.get('size',LIMIT+1) for e in entries)>LIMIT: raise Failure('worker_packages_invalid')
 for e in entries:
  if set(e)!={'name','version','architecture','sha256','size'} or not re.fullmatch(r'[a-z0-9][a-z0-9+.-]{0,127}',e['name']) or not re.fullmatch(r'[0-9][A-Za-z0-9.+:~_-]{0,127}',e['version']) or e['architecture'] not in ('arm64','all') or not re.fullmatch('[a-f0-9]{64}',e['sha256']) or type(e['size']) is not int or e['size']<=0: raise Failure('worker_packages_invalid')
 install_entries=m.get('installPackages',[])
 if not install_entries or any(e not in entries for e in install_entries): raise Failure('worker_packages_invalid')
 if list(base.iterdir()): raise Failure('runtime_directory_not_empty')
 save(base,'owner.json',marker); cache=base/'archives'; cache.mkdir(mode=0o700)
 r=dict(schemaVersion=1,operationId=m['operationId'],planDigest=m['planDigest'],workerSha256=hashlib.sha256(_workerSource).hexdigest(),state='failed',phase='download',verified=False,downloadExitCode=None,installExitCode=None,archives=[],errorCode=None)
 args=['/usr/bin/apt-get','-y','--no-install-recommends','--no-remove','--no-upgrade','-o','APT::Get::AllowUnauthenticated=false','-o','Acquire::AllowInsecureRepositories=false','-o','Acquire::Retries=0','-o','Acquire::http::Timeout=15','-o','Acquire::https::Timeout=15','-o','DPkg::Lock::Timeout=5','-o','Dpkg::Options::=--force-confold','-o','APT::Sandbox::User=root','-o','APT::Keep-Downloaded-Packages=true','-o','Dir::Cache::archives='+str(cache)+'/', 'install']+[e['name']+':'+e['architecture']+'='+e['version'] for e in entries]
 try:
  download_args=args[:args.index('install')]+['download']+args[args.index('install')+1:]
  r['downloadExitCode']=subprocess.run(download_args,cwd=str(cache),stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120).returncode
  if r['downloadExitCode']: raise Failure('archive_download_failed')
  r['phase']='verify'; r['archives']=verify(cache,entries); r['verified']=True; save(base,'receipt.json',r)
  install_args=args[:args.index('install')+1]+[e['name']+':'+e['architecture']+'='+e['version'] for e in install_entries]
  r['phase']='install'; r['installExitCode']=subprocess.run(install_args[:1]+['--no-download']+install_args[1:],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120).returncode
  if r['installExitCode']: raise Failure('archive_install_failed')
  r.update(state='succeeded',phase='complete')
 except Failure as e: r['errorCode']=str(e)
 except Exception: r['errorCode']='archive_worker_failed'
 save(base,'receipt.json',r)
 return r
if __name__=='__main__':
 try:
  m=json.loads(base64.b64decode(sys.argv[1],validate=True)); result=main(m)
  if m['action']!='install': print(enc(result).decode())
 except Exception: print('{"errorCode":"archive_worker_failed"}'); sys.exit(65)
'''
WORKER_SHA256 = hashlib.sha256(ARCHIVE_WORKER.encode()).hexdigest()
ARCHIVE_POLICY = {"workerSha256": WORKER_SHA256, "cacheRoot": "/run", "runtimeDirectoryMode": "0700",
                  "maxDownloadBytes": MAX_DOWNLOAD, "installDownloadDisabled": True}

# Keep every v3 byte above intact: retained units bind their original source in
# ExecStart. V4 shares only those archive I/O definitions and supplies its own
# closed install policy; read/cleanup retain the same operation-scoped behavior.
UPGRADE_ARCHIVE_WORKER = (ARCHIVE_WORKER.partition("if __name__=='__main__':")[0] +
    "\nUPGRADES=" + repr(DEPENDENCY_UPGRADES) + "\nCANDIDATES=" + repr(DEPENDENCY_CANDIDATES) + r'''
legacy_main=main
def installed(e):
 p=subprocess.run(['/usr/bin/dpkg-query','-W','-f=${Status}\t${Version}\t${Architecture}',e['name']],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=5)
 if p.returncode==1 and not p.stdout: return None
 if p.returncode or len(p.stdout)>4096: raise Failure('installed_state_unknown')
 fields=p.stdout.decode('utf-8').strip().split('\t')
 if len(fields)!=3 or fields[0]!='install ok installed': raise Failure('installed_state_unknown')
 return fields[1],fields[2]
def pending_entries(entries,requested):
 pending=[]
 for e in entries:
  current=installed(e)
  if current==(e['version'],e['architecture']): continue
  old=e['installedVersion']
  if e not in requested or (old is None and current is not None) or (old is not None and current!=(old,e['architecture'])): raise Failure('installed_state_changed')
  if old is not None and subprocess.run(['/usr/bin/dpkg','--compare-versions',old,'lt',e['version']],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=5).returncode: raise Failure('worker_upgrade_invalid')
  pending.append(e)
 return pending
def main(m):
 if m.get('action')!='install': return legacy_main(m)
 if set(m)!={'action','recipeId','operationId','planDigest','packages','installPackages','dependencyUpgradesApproved'}: raise Failure('worker_binding_invalid')
 if os.geteuid()!=0 or m.get('recipeId')!='dgx-spark-ubuntu24.04-arm64-openmpi4-packages-v4' or not re.fullmatch('[a-f0-9]{32}',m.get('operationId','')) or not re.fullmatch('[a-f0-9]{64}',m.get('planDigest','')): raise Failure('worker_binding_invalid')
 entries=m.get('packages',[]); requested=m.get('installPackages',[])
 if not 0<len(entries)<=128 or sum(e.get('size',LIMIT+1) for e in entries)>LIMIT: raise Failure('worker_packages_invalid')
 names=set()
 for e in entries:
  if set(e)!={'name','version','architecture','sha256','size','installedVersion'} or e['name'] in names or CANDIDATES.get(e['name'])!=(e['version'],e['architecture']) or not re.fullmatch('[a-f0-9]{64}',e['sha256']) or type(e['size']) is not int or e['size']<=0: raise Failure('worker_packages_invalid')
  old=e['installedVersion']
  if old is not None and (e['architecture']!='arm64' or UPGRADES.get(e['name'])!=(old,e['version'])): raise Failure('worker_upgrade_invalid')
  names.add(e['name'])
 needs=any(e['installedVersion'] is not None for e in entries)
 if type(m.get('dependencyUpgradesApproved')) is not bool or m['dependencyUpgradesApproved']!=needs: raise Failure('worker_upgrade_consent_missing')
 if not requested or any(e not in entries for e in requested) or len({e['name'] for e in requested})!=len(requested): raise Failure('worker_packages_invalid')
 pending_entries(entries,requested)
 base=Path('/run')/('pair-nccl-mpi-'+m['operationId']); s=base.lstat()
 if not stat.S_ISDIR(s.st_mode) or s.st_uid!=0 or stat.S_IMODE(s.st_mode)!=0o700 or list(base.iterdir()): raise Failure('unsafe_runtime_directory')
 save(base,'owner.json',dict(operationId=m['operationId'],planDigest=m['planDigest'])); cache=base/'archives'; cache.mkdir(mode=0o700)
 r=dict(schemaVersion=1,operationId=m['operationId'],planDigest=m['planDigest'],workerSha256=hashlib.sha256(_workerSource).hexdigest(),state='failed',phase='download',verified=False,downloadExitCode=None,installExitCode=None,archives=[],errorCode=None)
 args=['/usr/bin/apt-get','-y','--no-install-recommends','--no-remove','-o','APT::Get::AllowUnauthenticated=false','-o','APT::Get::allow-downgrades=false','-o','APT::Get::ReInstall=false','-o','Acquire::AllowInsecureRepositories=false','-o','Acquire::AllowDowngradeToInsecureRepositories=false','-o','Acquire::Retries=0','-o','Acquire::http::Timeout=15','-o','Acquire::https::Timeout=15','-o','DPkg::Lock::Timeout=5','-o','Dpkg::Options::=--force-confold','-o','APT::Sandbox::User=root','-o','APT::Keep-Downloaded-Packages=true','-o','Dir::Cache::archives='+str(cache)+'/', 'install']
 env=dict(os.environ,DEBIAN_FRONTEND='noninteractive',NEEDRESTART_MODE='l')
 try:
  download_args=args[:-1]+['download']+[e['name']+':'+e['architecture']+'='+e['version'] for e in entries]
  r['downloadExitCode']=subprocess.run(download_args,cwd=str(cache),stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120,env=env).returncode
  if r['downloadExitCode']: raise Failure('archive_download_failed')
  r['phase']='verify'; r['archives']=verify(cache,entries); r['verified']=True; save(base,'receipt.json',r)
  pending=pending_entries(entries,requested)
  r['phase']='install'
  install_args=args[:1]+['--no-download']+args[1:]+[e['name']+':'+e['architecture']+'='+e['version'] for e in pending]
  r['installExitCode']=subprocess.run(install_args,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120,env=env).returncode if pending else 0
  if r['installExitCode']: raise Failure('archive_install_failed')
  r.update(state='succeeded',phase='complete')
 except Failure as e: r['errorCode']=str(e)
 except Exception: r['errorCode']='archive_worker_failed'
 save(base,'receipt.json',r); return r
if __name__=='__main__':
 try:
  m=json.loads(base64.b64decode(sys.argv[1],validate=True)); result=main(m)
  if m['action']!='install': print(enc(result).decode())
 except Exception: print('{"errorCode":"archive_worker_failed"}'); sys.exit(65)
''')
UPGRADE_WORKER_SHA256 = hashlib.sha256(UPGRADE_ARCHIVE_WORKER.encode()).hexdigest()
UPGRADE_ARCHIVE_POLICY = {**ARCHIVE_POLICY, "workerSha256": UPGRADE_WORKER_SHA256, "needrestartMode": "l"}

# A separate closed recipe reuses v3's measured archive I/O. The original v3/v4
# worker bytes remain unchanged for their already-retained operation records.
PYTHON_ARCHIVE_WORKER = (ARCHIVE_WORKER.partition("if __name__=='__main__':")[0] +
    "\nPYTHON_CANDIDATES=" + repr(PYTHON_PACKAGES) + r'''
python_legacy_main=main
def main(m):
 if m.get('action')=='install':
  if set(m)!={'action','recipeId','operationId','planDigest','packages','installPackages'} or m.get('recipeId')!='dgx-spark-ubuntu24.04-arm64-python312-headers-v1': raise Failure('worker_binding_invalid')
  entries=m.get('packages',[])
  if not 0<len(entries)<=3 or len({e.get('name') for e in entries})!=len(entries): raise Failure('worker_packages_invalid')
  for e in entries:
   if PYTHON_CANDIDATES.get(e.get('name'))!=(e.get('version'),e.get('architecture')): raise Failure('worker_packages_invalid')
 return python_legacy_main(m)
if __name__=='__main__':
 try:
  m=json.loads(base64.b64decode(sys.argv[1],validate=True)); result=main(m)
  if m['action']!='install': print(enc(result).decode())
 except Exception: print('{"errorCode":"archive_worker_failed"}'); sys.exit(65)
''')
PYTHON_WORKER_SHA256 = hashlib.sha256(PYTHON_ARCHIVE_WORKER.encode()).hexdigest()
PYTHON_ARCHIVE_POLICY = {**ARCHIVE_POLICY, "workerSha256": PYTHON_WORKER_SHA256}

# A closed transport for the exact approved bytes; trusted metadata origin is unchanged.
PYTHON_SNAPSHOT_FILES = {
    "libexpat1-dev": {"version": "2.6.1-2ubuntu0.4", "architecture": "arm64", "size": 128426,
        "sha256": "38b2c2c024947d0dea10bcd21333ea9f942aeb423da62c3b67c17ac92ed57004",
        "url": "https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/e/expat/libexpat1-dev_2.6.1-2ubuntu0.4_arm64.deb"},
    "libpython3.12-dev": {"version": "3.12.3-1ubuntu0.16", "architecture": "arm64", "size": 5540320,
        "sha256": "644c40641fc40da4f8205fb96b60b404677e28402d8b2b55d437697cae1d7f96",
        "url": "https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/p/python3.12/libpython3.12-dev_3.12.3-1ubuntu0.16_arm64.deb"},
    "python3.12-dev": {"version": "3.12.3-1ubuntu0.16", "architecture": "arm64", "size": 497934,
        "sha256": "424a323ebfacc1454c805cb3486525b73dfb684d21f548ccfb96ba0eaf3a96a8",
        "url": "https://snapshot.ubuntu.com/ubuntu/20260901T000000Z/pool/main/p/python3.12/python3.12-dev_3.12.3-1ubuntu0.16_arm64.deb"},
}
PYTHON_SNAPSHOT_SOURCE = {"kind": "ubuntu-snapshot", "snapshot": "20260901T000000Z",
                          "urls": {name: value["url"] for name, value in PYTHON_SNAPSHOT_FILES.items()}}
PYTHON_SNAPSHOT_FETCH = r'''
import time,urllib.request,urllib.error
class NoArchiveRedirect(urllib.request.HTTPRedirectHandler):
 def redirect_request(self,*args,**kwargs): return None
def snapshot_download(cache,entries):
 opener=urllib.request.build_opener(NoArchiveRedirect())
 deadline=time.monotonic()+120
 for entry in entries:
  expected=SNAPSHOT_FILES.get(entry['name'])
  if expected is None or any(entry.get(k)!=expected[k] for k in ('version','architecture','size','sha256')): raise Failure('archive_source_binding_invalid')
  if time.monotonic()>=deadline: raise Failure('archive_download_timeout')
  filename=expected['url'].rsplit('/',1)[1]
  fd=os.open(cache/filename,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(fd,'wb') as out:
   try: response=opener.open(expected['url'],timeout=15)
   except urllib.error.HTTPError as error: raise Failure('archive_http_'+str(error.code)) from None
   except urllib.error.URLError: raise Failure('archive_transport_failed') from None
   with response:
    if response.status!=200 or response.geturl()!=expected['url']: raise Failure('archive_source_changed')
    length=response.headers.get('Content-Length')
    if length is not None and (not length.isdecimal() or int(length)!=expected['size']): raise Failure('archive_size_mismatch')
    count=0
    while True:
     if time.monotonic()>=deadline: raise Failure('archive_download_timeout')
     chunk=response.read(65536)
     if not chunk: break
     count+=len(chunk)
     if count>expected['size']: raise Failure('archive_size_mismatch')
     out.write(chunk)
    if count!=expected['size']: raise Failure('archive_size_mismatch')
 return 0
'''
_python_download_line = "r['downloadExitCode']=subprocess.run(download_args,cwd=str(cache),stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120).returncode"
assert PYTHON_ARCHIVE_WORKER.count(_python_download_line) == 1
# Preserve old Python and MPI bytes for retained operation ExecStart reconstruction.
PYTHON_SNAPSHOT_WORKER = (PYTHON_ARCHIVE_WORKER.partition("if __name__=='__main__':")[0]
    .replace(_python_download_line, "r['downloadExitCode']=snapshot_download(cache,entries)")
    + "\nSNAPSHOT_FILES=" + repr(PYTHON_SNAPSHOT_FILES) + PYTHON_SNAPSHOT_FETCH
    + "\nif __name__=='__main__':" + PYTHON_ARCHIVE_WORKER.partition("if __name__=='__main__':")[2])
PYTHON_SNAPSHOT_WORKER_SHA256 = hashlib.sha256(PYTHON_SNAPSHOT_WORKER.encode()).hexdigest()
PYTHON_SNAPSHOT_ARCHIVE_POLICY = {**ARCHIVE_POLICY, "workerSha256": PYTHON_SNAPSHOT_WORKER_SHA256,
                                 "archiveSource": PYTHON_SNAPSHOT_SOURCE}
PYTHON_SNAPSHOT_EFFECT = "Retrieve only the listed byte-identical packages over verified HTTPS from the official Ubuntu snapshot 20260901T000000Z; no redirects, APT source changes or metadata refresh."

# C's first-install path has no .16 candidate metadata. These bytes have their
# own explicit provenance; never label a local archive as cached APT trust.
PYTHON_LOCAL_SUBDIR = ".cache/nvpair/python312-headers-20260901"
PYTHON_LOCAL_ORIGIN = {"kind": "verified_local_archive", "source": "ubuntu-snapshot-20260901T000000Z", "trusted": False}
PYTHON_LOCAL_INSTALLED_KIB = {"libexpat1-dev": 712, "libpython3.12-dev": 27505, "python3.12-dev": 502}
PYTHON_LOCAL_DEPENDENCIES = {
    "python3.12": "3.12.3-1ubuntu0.16", "python3.12-minimal": "3.12.3-1ubuntu0.16",
    "libpython3.12-stdlib": "3.12.3-1ubuntu0.16", "libpython3.12t64": "3.12.3-1ubuntu0.16",
    "libexpat1": "2.6.1-2ubuntu0.4", "zlib1g-dev": "1:1.3.dfsg-3.1ubuntu2.2", "libc6-dev": "2.39-0ubuntu8.8",
}
PYTHON_LOCAL_EFFECTS = [
    "Install only the missing subset of three byte-verified local Python development archives; no cached-APT-origin claim is made.",
    "No upgrades, removals, reinstalls, dependency additions, downloads, source edits, model actions or runtime upgrade are authorized.",
    *EFFECTS[2:4],
    "Copy only the reviewed local archives into the owned root cache, verify bytes and DEB control fields, repeat exact local-path APT simulation, and install those same paths with downloads disabled.",
    "Preserve all existing packages and the bound installed dependency baseline; APT maintainer scripts may run for the listed packages.",
    "Keep installed development packages and recheck the same system Python as its normal user after the administrator unit and cache are fully cleaned.",
]

def python_local_path(binding, name):
    artifact = PYTHON_SNAPSHOT_FILES[name]
    return str(Path(binding["home"]) / PYTHON_LOCAL_SUBDIR / artifact["url"].rsplit("/", 1)[1])

def python_local_entries(names):
    return [{"name": name, **{key: PYTHON_SNAPSHOT_FILES[name][key] for key in ("version", "architecture", "sha256", "size")},
             "installedSize": PYTHON_LOCAL_INSTALLED_KIB[name] * 1024, "origin": dict(PYTHON_LOCAL_ORIGIN)} for name in sorted(names)]

def python_local_simulation_output(output, expected):
    wanted = {entry["name"]: entry for entry in expected}
    actual = set()
    for line in output.splitlines():
        if line.startswith(("Remv ", "Purg ")):
            raise ProbeError("local_archive_extra_change", "The local archive simulation removes an existing package")
        if line.startswith("Inst "):
            fields = line.split(" ", 2)
            name = fields[1].removesuffix(":arm64")
            rest = fields[2]
            version = rest[1:].split(" ", 1)[0] if rest.startswith("(") else ""
            architecture = re.search(r"\[([^\]]+)\]\)$", rest)
            if name not in wanted or name in actual or version != wanted[name]["version"] or not architecture or architecture[1] != "arm64":
                raise ProbeError("local_archive_extra_change", "APT local simulation differs from the missing exact three-package subset")
            actual.add(name)
        if line.startswith("Conf "):
            _, name, rest = line.split(" ", 2)
            name = name.removesuffix(":arm64")
            if name not in wanted or not rest.startswith("(") or rest[1:].split(" ", 1)[0] != wanted[name]["version"]:
                raise ProbeError("local_archive_extra_change", "APT would configure an unlisted package")
        if re.match(r"^\d+ upgraded,", line) and not re.match(r"^0 upgraded, \d+ newly installed, 0 to remove", line):
            raise ProbeError("local_archive_extra_change", "APT local simulation reports an existing-package change")
    if actual != set(wanted):
        raise ProbeError("local_archive_simulation_incomplete", "APT did not simulate exactly the listed missing packages")

def python_local_simulate(native, binding, entries):
    argv = [native.resolve("apt-get"), "--simulate", "--no-download", "--no-install-recommends", "--no-remove", "--no-upgrade",
            "-o", "APT::Get::ReInstall=false", "-o", "APT::Get::AllowUnauthenticated=false", "-o", "Dir::Cache::archives=" + str(Path(binding["home"]) / PYTHON_LOCAL_SUBDIR) + "/", "install",
            *[python_local_path(binding, entry["name"]) for entry in entries]]
    code, output = native.run(argv)
    if code:
        raise ProbeError("local_archive_simulation_failed", "Exact local-path APT simulation failed; no package action occurred")
    python_local_simulation_output(output, entries)

def python_local_validate_files(native, binding):
    for name, artifact in PYTHON_SNAPSHOT_FILES.items():
        path = Path(python_local_path(binding, name))
        if str(path.resolve(strict=True)) != str(path):
            raise ProbeError("local_archive_changed", "Local package staging path is redirected")
        info = path.stat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid not in (0, binding["uid"]) or info.st_mode & 0o022 or info.st_size != artifact["size"]:
            raise ProbeError("local_archive_changed", "Local package staging file ownership or size changed")
        with path.open("rb") as stream:
            if hashlib.sha256(stream.read(artifact["size"] + 1)).hexdigest() != artifact["sha256"]:
                raise ProbeError("local_archive_changed", "Local package bytes differ from the closed verified archive set")
        code, fields = native.run([native.resolve("dpkg-deb"), "--field", str(path), "Package", "Version", "Architecture", "Installed-Size"])
        observed = dict(line.split(":", 1) for line in fields.splitlines() if ":" in line)
        observed = {key: value.strip() for key, value in observed.items()}
        expected = {"Package": name, "Version": artifact["version"], "Architecture": artifact["architecture"], "Installed-Size": str(PYTHON_LOCAL_INSTALLED_KIB[name])}
        if code or observed != expected:
            raise ProbeError("local_archive_changed", "Local DEB control fields differ from the verified archive identity")

def resolve_python_local_transaction(cache, native, binding):
    if cache.broken_count:
        raise ProbeError("apt_state_broken", "Local archives do not authorize repair of broken package state")
    for name, version in PYTHON_LOCAL_DEPENDENCIES.items():
        package = cache[name] if name in cache else None
        installed = package.installed if package else None
        if installed is None or installed.version != version or installed.architecture != "arm64" or getattr(getattr(package, "_pkg", None), "current_state", None) != 6:
            raise ProbeError("local_archive_dependency_changed", "A required installed dependency differs from the fixed local archive dialect")
    baseline, missing = [], []
    for name, (version, architecture) in PYTHON_PACKAGES.items():
        package = cache[name] if name in cache else None
        old = package.installed if package else None
        if old:
            if old.version != version or old.architecture != architecture or getattr(getattr(package, "_pkg", None), "current_state", None) != 6:
                raise ProbeError("local_archive_existing_change", "An existing development package cannot be upgraded, downgraded or reinstalled")
            baseline.append({"name": name, "version": version, "architecture": architecture})
        else:
            missing.append(name)
    entries = python_local_entries(missing)
    python_local_validate_files(native, binding)
    if entries:
        python_local_simulate(native, binding, entries)
    return {"rootPackage": {"name": PYTHON_ROOT_PACKAGE, "version": PYTHON_PACKAGES[PYTHON_ROOT_PACKAGE][0]},
            "installedBaseline": baseline, "installedBaselineDigest": baseline_digest(cache, set(missing)), "packages": entries}

PYTHON_LOCAL_WORKER_HELPERS = r'''
import pwd
def local_state(m):
 p=subprocess.run(['/usr/bin/dpkg-query','-W','-f=${Package}\t${Version}\t${Architecture}\t${Status}\n'],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=10)
 if p.returncode or len(p.stdout)>4194304: raise Failure('local_installed_state_unknown')
 rows={}
 for line in p.stdout.decode().splitlines():
  fields=line.split('\t')
  if len(fields)!=4 or fields[0] in rows: raise Failure('local_installed_state_unknown')
  rows[fields[0]]=fields[1:]
 for name,version in LOCAL_DEPENDENCIES.items():
  if rows.get(name)!=[version,'arm64','install ok installed']: raise Failure('local_dependency_changed')
 excluded={e['name'] for e in m['packages']}
 baseline=sorted([name,*values[:2]] for name,values in rows.items() if values[2]=='install ok installed' and name not in excluded)
 if hashlib.sha256(enc(baseline)).hexdigest()!=m['installedBaselineDigest']: raise Failure('local_installed_baseline_changed')
 pending=[]
 for e in m['packages']:
  old=rows.get(e['name'])
  if old is None or old[2]=='deinstall ok config-files': pending.append(e)
  elif old!=[e['version'],e['architecture'],'install ok installed']: raise Failure('local_existing_package_change')
 return pending

def local_binding(m):
 identity=m['identity']; binding=m['runtimeBinding']; uid=identity.get('uid')
 if type(uid) is not int or uid<=0 or pwd.getpwuid(uid).pw_dir!=identity.get('home'): raise Failure('local_uid_binding_changed')
 home=Path(identity['home']); root=home/'.config/Nvidia Corporation/Personal AI Router/engine-bin/vllm'
 expected={'schemaVersion':2,'installRoot':str(root),'sourceExecutable':'/usr/bin/python3.12','sourceExecutableSHA256':binding.get('sourceExecutableSHA256'),'sourcePackageVersion':'3.12.3-1ubuntu0.16','pythonVersion':'3.12.3','includePath':'/usr/include/python3.12','configHeaderPath':'/usr/include/python3.12/pyconfig.h'}
 if binding!=expected or not re.fullmatch('[a-f0-9]{64}',str(binding.get('sourceExecutableSHA256'))): raise Failure('local_python_binding_changed')
 if hashlib.sha256(regular(Path('/usr/bin/python3.12'),16777216)).hexdigest()!=binding['sourceExecutableSHA256']: raise Failure('local_python_binding_changed')
 if (root/'active-runtime.json').exists() or ((root/'environments').exists() and list((root/'environments').iterdir())): raise Failure('local_installation_appeared')
 public=home/'.config/Nvidia Corporation/Personal AI Router/cluster/identity.json'
 fd=os.open(public,os.O_RDONLY|os.O_NOFOLLOW)
 with os.fdopen(fd,'rb') as f:
  raw=f.read(8193)
 if len(raw)>8192 or hashlib.sha256(raw).hexdigest()!=identity.get('publicIdentitySha256') or json.loads(raw).get('node_uuid')!=identity.get('principal'): raise Failure('local_public_identity_changed')
 return home

def local_copy(cache,entries,m):
 home=local_binding(m); local_state(m)
 for e in entries:
  expected=LOCAL_FILES.get(e['name'])
  if expected is None or any(e.get(k)!=expected[k] for k in ('version','architecture','sha256','size')): raise Failure('local_archive_binding_changed')
  filename=expected['url'].rsplit('/',1)[1]; source=home/LOCAL_SUBDIR/filename
  if str(source.resolve(strict=True))!=str(source): raise Failure('local_archive_redirected')
  fd=os.open(source,os.O_RDONLY|os.O_NOFOLLOW)
  with os.fdopen(fd,'rb') as f:
   info=os.fstat(f.fileno());data=f.read(expected['size']+1)
   if info.st_uid not in (0,m['identity']['uid']) or info.st_mode&0o022 or not stat.S_ISREG(info.st_mode) or len(data)!=expected['size'] or hashlib.sha256(data).hexdigest()!=expected['sha256']: raise Failure('local_archive_changed')
  out=os.open(cache/filename,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW,0o600)
  with os.fdopen(out,'wb') as f:f.write(data);f.flush();os.fsync(f.fileno())
 return 0

def local_install(args,m,cache):
 local_binding(m);pending=local_state(m)
 if not pending:return 0
 paths=[str(cache/LOCAL_FILES[e['name']]['url'].rsplit('/',1)[1]) for e in pending]
 install=args[:args.index('install')+1]+paths
 env=dict(os.environ,LANG='C',LC_ALL='C')
 dry=subprocess.run(install[:1]+['--simulate','--no-download']+install[1:],stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.PIPE,timeout=30,env=env)
 if dry.returncode or len(dry.stdout)>65536:raise Failure('local_simulation_failed')
 wanted={e['name']:e for e in pending}; actual=set()
 for line in dry.stdout.decode().splitlines():
  if line.startswith(('Remv ','Purg ')):raise Failure('local_extra_package_change')
  if line.startswith('Inst '):
   _,name,rest=line.split(' ',2);name=name.removesuffix(':arm64');version=rest[1:].split(' ',1)[0] if rest.startswith('(') else '';arch=re.search(r'\[([^\]]+)\]\)$',rest)
   if name not in wanted or name in actual or version!=wanted[name]['version'] or not arch or arch[1]!='arm64':raise Failure('local_extra_package_change')
   actual.add(name)
  if line.startswith('Conf '):
   _,name,rest=line.split(' ',2);name=name.removesuffix(':arm64')
   if name not in wanted or not rest.startswith('(') or rest[1:].split(' ',1)[0]!=wanted[name]['version']:raise Failure('local_extra_package_configuration')
  if re.match(r'^\d+ upgraded,',line) and not re.match(r'^0 upgraded, \d+ newly installed, 0 to remove',line):raise Failure('local_extra_package_change')
 if actual!=set(wanted):raise Failure('local_simulation_incomplete')
 if local_state(m)!=pending:raise Failure('local_installed_state_changed')
 return subprocess.run(install[:1]+['--no-download','-o','APT::Get::ReInstall=false']+install[1:],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120,env=env).returncode
'''
_python_install_line = "r['installExitCode']=subprocess.run(install_args[:1]+['--no-download']+install_args[1:],stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=120).returncode"
_python_manifest_fields = "{'action','recipeId','operationId','planDigest','packages','installPackages'}"
assert PYTHON_ARCHIVE_WORKER.count(_python_install_line) == 1
PYTHON_LOCAL_WORKER = (PYTHON_ARCHIVE_WORKER.partition("if __name__=='__main__':")[0]
    .replace(_python_download_line, "r['downloadExitCode']=local_copy(cache,entries,m)")
    .replace(_python_install_line, "r['installExitCode']=local_install(install_args,m,cache)")
    .replace(_python_manifest_fields, "{'action','recipeId','operationId','planDigest','packages','installPackages','identity','runtimeBinding','installedBaselineDigest'}")
    + "\nLOCAL_FILES=" + repr(PYTHON_SNAPSHOT_FILES) + "\nLOCAL_DEPENDENCIES=" + repr(PYTHON_LOCAL_DEPENDENCIES) + "\nLOCAL_SUBDIR=" + repr(PYTHON_LOCAL_SUBDIR)
    + PYTHON_LOCAL_WORKER_HELPERS + "\nif __name__=='__main__':" + PYTHON_ARCHIVE_WORKER.partition("if __name__=='__main__':")[2])
PYTHON_LOCAL_WORKER_SHA256 = hashlib.sha256(PYTHON_LOCAL_WORKER.encode()).hexdigest()
PYTHON_LOCAL_ARCHIVE_POLICY = {**ARCHIVE_POLICY, "workerSha256": PYTHON_LOCAL_WORKER_SHA256,
    "archiveSource": {"kind": "verified-local-archive", "snapshot": "20260901T000000Z", "subdir": PYTHON_LOCAL_SUBDIR, "dependencies": PYTHON_LOCAL_DEPENDENCIES}}


def python_archive_policy(plan):
    policy = plan.get("archivePolicy")
    if policy == PYTHON_ARCHIVE_POLICY:
        return PYTHON_ARCHIVE_POLICY
    if policy == PYTHON_SNAPSHOT_ARCHIVE_POLICY:
        return PYTHON_SNAPSHOT_ARCHIVE_POLICY
    if policy == PYTHON_LOCAL_ARCHIVE_POLICY:
        return PYTHON_LOCAL_ARCHIVE_POLICY
    raise ProbeError("approval_mismatch", "Python archive source or worker policy changed")


def python_plan_effects(first_install, snapshot=False):
    effects = PYTHON_INSTALL_EFFECTS if first_install else PYTHON_EFFECTS
    return effects + [PYTHON_SNAPSHOT_EFFECT] if snapshot else effects


def validate_snapshot_packages(packages):
    if not isinstance(packages, list) or not 0 < len(packages) <= 3 or any(not isinstance(entry, dict) for entry in packages):
        raise ProbeError("archive_source_binding_invalid", "The fixed archive source requires its bounded package list")
    for entry in packages:
        expected = PYTHON_SNAPSHOT_FILES.get(entry.get("name"))
        if expected is None or any(entry.get(key) != expected[key] for key in ("version", "architecture", "size", "sha256")):
            raise ProbeError("archive_source_binding_invalid", "Trusted package metadata no longer matches the fixed official archive bytes")


def archive_worker(plan):
    if plan.get("recipeId") == PYTHON_RECIPE:
        if python_archive_policy(plan) == PYTHON_LOCAL_ARCHIVE_POLICY:
            return PYTHON_LOCAL_WORKER, PYTHON_LOCAL_WORKER_SHA256
        if python_archive_policy(plan) == PYTHON_SNAPSHOT_ARCHIVE_POLICY:
            return PYTHON_SNAPSHOT_WORKER, PYTHON_SNAPSHOT_WORKER_SHA256
        return PYTHON_ARCHIVE_WORKER, PYTHON_WORKER_SHA256
    if plan.get("recipeId") == RECIPE:
        return ARCHIVE_WORKER, WORKER_SHA256
    if plan.get("recipeId") == UPGRADE_RECIPE:
        return UPGRADE_ARCHIVE_WORKER, UPGRADE_WORKER_SHA256
    raise ProbeError("approval_mismatch", "Unknown package worker recipe")


def canonical(value):
    return json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True, allow_nan=False).encode()


def digest(plan):
    return hashlib.sha256(canonical({key: value for key, value in plan.items() if key != "planDigest"})).hexdigest()


def validate_request(request):
    if not isinstance(request, dict):
        raise ProbeError("invalid_request", "MPI package request must be an object")
    allowed = {"action", "nodeId", "principal"}
    if "runtimePreparation" in request:
        allowed.add("runtimePreparation")
        if request["runtimePreparation"] is not True:
            raise ProbeError("invalid_request", "Runtime preparation must select the fixed Python recipe")
    if "firstInstall" in request:
        allowed.add("firstInstall")
        if request["firstInstall"] is not True or request.get("runtimePreparation") is not True:
            raise ProbeError("invalid_request", "First-install preparation requires the explicit Python-header purpose")
    action = request.get("action")
    if action in ("provision", "status", "cancel", "reconcile"):
        allowed.add("operationId")
    if action in ("status", "cancel", "reconcile"):
        allowed.add("expectedPlanDigest")
    if action == "provision":
        allowed.update({"expectedUID", "expectedHome", "approvedPlan", "elevationPassword"})
        allowed.add("dependencyUpgradesApproved")
    if action == "review":
        allowed.add("dependencyUpgradeReview")
    if action in ("status", "cancel", "reconcile"):
        allowed.add("elevationPassword")
    if set(request) - allowed:
        raise ProbeError("invalid_request", "Unexpected MPI package request fields")
    for flag in ("dependencyUpgradeReview", "dependencyUpgradesApproved"):
        if flag in request and type(request[flag]) is not bool:
            raise ProbeError("invalid_request", "Dependency upgrade consent must be explicit boolean input")
    if request.get("runtimePreparation") and any(key in request for key in ("dependencyUpgradeReview", "dependencyUpgradesApproved")):
        raise ProbeError("invalid_request", "Python-header preparation never authorizes dependency upgrades")
    if action not in ("review", "provision", "status", "cancel", "reconcile"):
        raise ProbeError("unsupported_action", "Unsupported MPI package action")
    if any(not isinstance(request.get(key), str) or not TOKEN.fullmatch(request[key]) for key in ("nodeId", "principal")):
        raise ProbeError("identity_invalid", "Valid paired node and principal identities are required")
    if request["nodeId"] != request["principal"]:
        raise ProbeError("identity_mismatch", "This recipe requires matching node and principal identities")
    if action != "review" and (not isinstance(request.get("operationId"), str) or not re.fullmatch(r"[a-f0-9]{32}", request["operationId"])):
        raise ProbeError("invalid_request", "Mutation and recovery actions require a 32-hex operation identifier")
    if action in ("status", "cancel", "reconcile") and (not isinstance(request.get("expectedPlanDigest"), str) or not re.fullmatch(r"[a-f0-9]{64}", request["expectedPlanDigest"])):
        raise ProbeError("approval_mismatch", "Recovery requires the original expected plan digest")
    if "elevationPassword" in request:
        password = request["elevationPassword"]
        if not isinstance(password, str) or not password or len(password) > 4096 or any(char in password for char in "\x00\r\n"):
            raise ProbeError("invalid_request", "Separate volatile administrator access is invalid")


def command(native, name, args):
    code, output = native.run([native.resolve(name), *args])
    if code:
        raise ProbeError("observation_failed", name + " observation exited with status " + str(code))
    return output


def identity(request, native):
    label = "Python-header" if request.get("runtimePreparation") else "MPI"
    if platform.system() != "Linux" or platform.machine() != "aarch64":
        raise ProbeError("platform_unsupported", label + " package recipe requires native Linux ARM64")
    if parse_os_release(native.os_release()) != {"ID": "ubuntu", "VERSION_ID": "24.04"}:
        raise ProbeError("platform_unsupported", label + " package recipe requires observed Ubuntu 24.04")
    if command(native, "dpkg", ["--print-architecture"]) != "arm64":
        raise ProbeError("platform_unsupported", "Native Debian architecture arm64 was not confirmed")
    user = native.account()
    if type(user.get("uid")) is not int or user["uid"] <= 0 or not user.get("home", "").startswith("/") or user["home"] == "/":
        raise ProbeError("account_invalid", label + " package review requires the selected normal account")
    path = os.path.join(user["home"], ".config", "Nvidia Corporation", "Personal AI Router", "cluster", "identity.json")
    raw = native.read(path, 8192)
    current = decode(raw)
    if not isinstance(current, dict) or current.get("node_uuid") != request["principal"]:
        raise ProbeError("identity_mismatch", "Public PAIR identity differs from the selected participant")
    return {"nodeId": request["nodeId"], "principal": request["principal"], "uid": user["uid"],
            "home": user["home"], "publicIdentitySha256": hashlib.sha256(raw.encode()).hexdigest()}


def installed_family(native, python=False):
    rows = []
    path = native.resolve("dpkg-query")
    for name in (PYTHON_PACKAGES if python else MPI_PACKAGES):
        code, output = native.run([path, "-W", "-f=${Status}\t${Version}\t${Architecture}", name])
        row = {"name": name, "status": "missing", "version": None, "architecture": None}
        if code == 0:
            fields = output.split("\t")
            if len(fields) != 3 or "\n" in output or not VERSION.fullmatch(fields[1]):
                raise ProbeError("package_status_unknown", "Malformed installed " + ("Python-header" if python else "MPI") + " package status")
            row.update(status=fields[0], version=fields[1], architecture=fields[2])
        elif code != 1 or output:
            raise ProbeError("package_status_unknown", "Installed " + ("Python-header" if python else "MPI") + " package status is unavailable")
        rows.append(row)
    return rows


def matching_family(rows, python=False):
    if python:
        return (len(rows) == len(PYTHON_PACKAGES) and {row["name"] for row in rows} == set(PYTHON_PACKAGES) and
                all(row["status"] == "install ok installed" and PYTHON_PACKAGES[row["name"]] ==
                    (row["version"], row["architecture"]) for row in rows))
    return (len(rows) == 4 and {row["name"] for row in rows} == set(MPI_PACKAGES) and
            all(row["status"] == "install ok installed" and row["architecture"] ==
                ("all" if row["name"] == "openmpi-common" else "arm64") and
                isinstance(row["version"], str) and row["version"].startswith("4.1.") for row in rows) and
            len({row["version"] for row in rows}) == 1)


PYTHON_BINDING_FIELDS = {
    "schemaVersion", "environmentId", "pythonPath", "pythonSHA256", "runtimeRecordSHA256",
    "runtimeReceiptSHA256", "pipReportSHA256", "pyvenvConfigSHA256", "sourceExecutable",
    "sourceExecutableSHA256", "sourcePackageVersion", "pythonVersion", "includePath", "configHeaderPath",
}
PYTHON_INSTALL_BINDING_FIELDS = {
    "schemaVersion", "installRoot", "sourceExecutable", "sourceExecutableSHA256",
    "sourcePackageVersion", "pythonVersion", "includePath", "configHeaderPath",
}
PYTHON_HEADERS_PROBE = (
    "import json,os,shutil,sys,sysconfig; "
    "scheme=sysconfig.get_default_scheme(); scheme='posix_prefix' if scheme=='posix_local' else scheme; "
    "include=sysconfig.get_paths(scheme=scheme)['include']; config=sysconfig.get_config_h_filename(); "
    "print(json.dumps(dict(uid=os.geteuid(),pythonVersion='.'.join(map(str,sys.version_info[:3])),"
    "sourceExecutable=os.path.realpath(sys._base_executable),includePath=include,configHeaderPath=config,"
    "compiler=bool(shutil.which('gcc',path='/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin') or shutil.which('clang',path='/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin')),"
    "pythonHeaders=os.path.isfile(os.path.join(include,'Python.h')) and os.access(os.path.join(include,'Python.h'),os.R_OK),"
    "configHeader=os.path.isfile(config) and os.access(config,os.R_OK))))"
)


def python_runtime_root(binding):
    return Path(binding["home"]) / ".config/Nvidia Corporation/Personal AI Router/engine-bin/vllm"


def validate_python_binding(value, identity_binding):
    if isinstance(value, dict) and type(value.get("schemaVersion")) is int and value["schemaVersion"] == 2:
        if (set(value) != PYTHON_INSTALL_BINDING_FIELDS or
                value.get("installRoot") != python_runtime_root(identity_binding).as_posix() or
                value.get("sourceExecutable") != "/usr/bin/python3.12" or
                not isinstance(value.get("sourceExecutableSHA256"), str) or
                not re.fullmatch(r"[a-f0-9]{64}", value["sourceExecutableSHA256"]) or
                value.get("sourcePackageVersion") != "3.12.3-1ubuntu0.16" or value.get("pythonVersion") != "3.12.3" or
                value.get("includePath") != "/usr/include/python3.12" or
                value.get("configHeaderPath") != "/usr/include/python3.12/pyconfig.h"):
            raise ProbeError("install_source_invalid", "First-install binding must describe only the fixed system Python source and installation root")
        return
    if not isinstance(value, dict) or set(value) != PYTHON_BINDING_FIELDS or type(value.get("schemaVersion")) is not int or value["schemaVersion"] != 1:
        raise ProbeError("runtime_identity_invalid", "Managed interpreter binding is incomplete")
    environment = value.get("environmentId")
    if not isinstance(environment, str) or not re.fullmatch(r"v[0-9][A-Za-z0-9._-]{1,100}", environment):
        raise ProbeError("runtime_identity_invalid", "Managed environment identity is invalid")
    python = python_runtime_root(identity_binding) / "environments" / environment / "bin/python"
    if (value.get("pythonPath") != python.as_posix() or value.get("sourceExecutable") != "/usr/bin/python3.12" or
            value.get("sourcePackageVersion") != "3.12.3-1ubuntu0.16" or value.get("pythonVersion") != "3.12.3" or
            value.get("includePath") != "/usr/include/python3.12" or
            value.get("configHeaderPath") != "/usr/include/python3.12/pyconfig.h"):
        raise ProbeError("runtime_identity_unsupported", "This fixed recipe requires the recorded Ubuntu Python 3.12 interpreter and header locations")
    for name in ("pythonSHA256", "runtimeRecordSHA256", "runtimeReceiptSHA256", "pipReportSHA256",
                 "pyvenvConfigSHA256", "sourceExecutableSHA256"):
        if not isinstance(value.get(name), str) or not re.fullmatch(r"[a-f0-9]{64}", value[name]):
            raise ProbeError("runtime_identity_invalid", "Managed interpreter provenance digest is invalid")
    if value["pythonSHA256"] != value["sourceExecutableSHA256"]:
        raise ProbeError("runtime_identity_changed", "Managed Python no longer matches the fixed source interpreter")


def python_owned_file(path, root, uid, limit, content=False):
    """Read one fixed runtime body; reject links and changes across the read."""
    path, root = Path(path), Path(root)
    if not path.is_relative_to(root):
        raise ProbeError("runtime_identity_invalid", "Managed runtime path escaped its fixed root")
    current = path.parent
    while True:
        info = current.lstat()
        if not stat.S_ISDIR(info.st_mode) or stat.S_ISLNK(info.st_mode) or info.st_uid != uid or info.st_mode & 0o022:
            raise ProbeError("runtime_identity_invalid", "Managed runtime ancestry is not safely owned")
        if current == root:
            break
        current = current.parent
    fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
    with os.fdopen(fd, "rb") as stream:
        info = os.fstat(stream.fileno())
        if not stat.S_ISREG(info.st_mode) or info.st_uid != uid or info.st_mode & 0o6022 or info.st_size > limit:
            raise ProbeError("runtime_identity_invalid", "Managed runtime body is not safely owned or bounded")
        h, chunks, size = hashlib.sha256(), [], 0
        for chunk in iter(lambda: stream.read(1 << 20), b""):
            size += len(chunk)
            if size > limit:
                raise ProbeError("runtime_identity_invalid", "Managed runtime body exceeds its bound")
            h.update(chunk)
            if content:
                chunks.append(chunk)
        after = os.fstat(stream.fileno())
    named = path.lstat()
    if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (after.st_dev, after.st_ino, after.st_size, after.st_mtime_ns) or (named.st_dev, named.st_ino) != (info.st_dev, info.st_ino):
        raise ProbeError("runtime_identity_changed", "Managed runtime changed during observation")
    return h.hexdigest(), b"".join(chunks) if content else None


def python_runtime_files(native, binding):
    root = python_runtime_root(binding)
    if str(root) != os.path.realpath(root):
        raise ProbeError("runtime_identity_invalid", "Managed runtime root is not its canonical owned directory")
    active_hash, raw = python_owned_file(root / "active-runtime.json", root, binding["uid"], 1 << 20, True)
    active = decode(raw.decode("utf-8"))
    environment = active.get("active") if isinstance(active, dict) else None
    if not isinstance(active, dict) or type(active.get("schema")) is not int or active["schema"] != 1 or active.get("removing") or not isinstance(environment, str) or not re.fullmatch(r"v[0-9][A-Za-z0-9._-]{1,100}", environment):
        raise ProbeError("runtime_unavailable", "Prepare runtime requires an existing active managed vLLM environment")
    folder = root / "environments" / environment
    receipt_hash, raw = python_owned_file(folder / "pair-runtime.json", root, binding["uid"], 16384, True)
    receipt = decode(raw.decode("utf-8"))
    if (not isinstance(receipt, dict) or type(receipt.get("schema")) is not int or receipt["schema"] != 1 or receipt.get("engine") != "vllm" or
            receipt.get("ownerRoot") != str(root) or receipt.get("environment") != str(folder) or
            receipt.get("architecture") != "arm64" or receipt.get("version") != "0.28.0" or
            receipt.get("pythonSource") != "/usr/bin/python3.12" or
            receipt.get("wheelUrl") != "https://github.com/vllm-project/vllm/releases/download/v0.28.0/vllm-0.28.0-cp38-abi3-manylinux_2_28_aarch64.whl" or
            receipt.get("wheelSha256") != "817b8181f7f61b4a62dc1d5d9ab39f2bfb60a6cb86c29879a78a147b85756787"):
        raise ProbeError("runtime_identity_unsupported", "Managed runtime receipt does not match this fixed Python-header recipe")
    python_hash, _ = python_owned_file(folder / "bin/python", root, binding["uid"], 32 << 20)
    pip_hash, _ = python_owned_file(folder / "pip-report.json", root, binding["uid"], 16 << 20)
    config_hash, _ = python_owned_file(folder / "pyvenv.cfg", root, binding["uid"], 8192)
    if python_hash != receipt.get("pythonSha256") or pip_hash != receipt.get("pipReportSha256"):
        raise ProbeError("runtime_identity_changed", "Managed interpreter or dependency report no longer matches its receipt")
    system_python = native.resolve("python3.12", preferred=("/usr/bin/python3.12",))
    if system_python != "/usr/bin/python3.12":
        raise ProbeError("runtime_identity_unsupported", "Only the fixed Ubuntu system Python source is supported")
    source_hash, _ = python_owned_file(system_python, Path("/usr"), 0, 32 << 20)
    status = command(native, "dpkg-query", ["-W", "-f=${Status}\t${Version}\t${Architecture}", "python3.12-minimal"])
    if status != "install ok installed\t3.12.3-1ubuntu0.16\tarm64":
        raise ProbeError("runtime_identity_unsupported", "The fixed source Python package/version is not installed")
    return {"schemaVersion": 1, "environmentId": environment, "pythonPath": str(folder / "bin/python"),
            "pythonSHA256": python_hash, "runtimeRecordSHA256": active_hash, "runtimeReceiptSHA256": receipt_hash,
            "pipReportSHA256": pip_hash, "pyvenvConfigSHA256": config_hash, "sourceExecutable": system_python,
            "sourceExecutableSHA256": source_hash, "sourcePackageVersion": "3.12.3-1ubuntu0.16"}


def python_install_absence(binding):
    """Observe the ordinary installer state without creating a runtime record."""
    root, home = python_runtime_root(binding), Path(binding["home"])
    if str(root) != os.path.realpath(root) or not root.is_relative_to(home):
        raise ProbeError("install_state_invalid", "The installation root is not the account's canonical directory")
    current = root
    while True:
        try:
            info = current.lstat()
        except FileNotFoundError:
            if current == home:
                raise ProbeError("install_state_invalid", "The selected account home is unavailable") from None
        else:
            if not stat.S_ISDIR(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022:
                raise ProbeError("install_state_invalid", "Existing installation ancestry is not safely owned")
        if current == home:
            break
        current = current.parent
    record = root / "active-runtime.json"
    if os.path.lexists(record):
        _, raw = python_owned_file(record, root, binding["uid"], 1 << 20, True)
        state = decode(raw.decode("utf-8"))
        if state != {"schema": 1} or type(state.get("schema")) is not int:
            raise ProbeError("install_state_changed", "A managed runtime or retained recovery state exists; use its normal repair or recovery controls")
    environments = root / "environments"
    if os.path.lexists(environments):
        info = environments.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022 or next(environments.iterdir(), None) is not None:
            raise ProbeError("install_state_changed", "An environment is present or being staged; finish its normal installation or recovery first")


def python_install_files(native, binding):
    python_install_absence(binding)
    system_python = native.resolve("python3.12", preferred=("/usr/bin/python3.12",))
    if system_python != "/usr/bin/python3.12":
        raise ProbeError("install_source_unsupported", "Only the fixed Ubuntu system Python source is supported")
    source_hash, _ = python_owned_file(system_python, Path("/usr"), 0, 32 << 20)
    status = command(native, "dpkg-query", ["-W", "-f=${Status}\t${Version}\t${Architecture}", "python3.12-minimal"])
    if status != "install ok installed\t3.12.3-1ubuntu0.16\tarm64":
        raise ProbeError("install_source_unsupported", "The fixed source Python package/version is not installed")
    return {"schemaVersion": 2, "installRoot": python_runtime_root(binding).as_posix(),
            "sourceExecutable": system_python, "sourceExecutableSHA256": source_hash,
            "sourcePackageVersion": "3.12.3-1ubuntu0.16"}


def python_runtime_observation(native, binding, expected=None, first_install=False):
    # The package helper remains the selected user; only the separate archive
    # worker is privileged. Never execute managed Python from that root worker.
    account = native.account()
    if type(account.get("uid")) is not int or account["uid"] <= 0 or account["uid"] != binding["uid"] or account.get("home") != binding["home"]:
        raise ProbeError("runtime_privilege_invalid", "Managed Python must be checked as the selected nonroot account")
    observe_files = python_install_files if first_install else python_runtime_files
    files = observe_files(native, binding)
    executable = files["sourceExecutable"] if first_install else files["pythonPath"]
    code, raw = native.run([executable, "-I", "-B", "-c", PYTHON_HEADERS_PROBE])
    if code or len(raw) > 8192:
        raise ProbeError("runtime_probe_failed", "The managed interpreter prerequisite observation did not complete")
    try:
        observed = decode(raw)
    except ProbeError:
        raise ProbeError("runtime_probe_failed", "The managed interpreter observation was malformed") from None
    fields = {"uid", "pythonVersion", "sourceExecutable", "includePath", "configHeaderPath", "compiler", "pythonHeaders", "configHeader"}
    if (not isinstance(observed, dict) or set(observed) != fields or observed["uid"] != binding["uid"] or
            any(type(observed[key]) is not bool for key in ("compiler", "pythonHeaders", "configHeader"))):
        raise ProbeError("runtime_probe_failed", "The managed interpreter observation did not bind the normal account")
    value = {**files, **{key: observed[key] for key in ("pythonVersion", "sourceExecutable", "includePath", "configHeaderPath")}}
    validate_python_binding(value, binding)
    if observe_files(native, binding) != files or expected is not None and value != expected:
        raise ProbeError("runtime_identity_changed", "The reviewed managed interpreter source changed")
    return value, {"checkedAsUID": binding["uid"], "sourceBindingUnchanged": True,
                   **{key: observed[key] for key in ("compiler", "pythonHeaders", "configHeader")}}


def python_prerequisites_ready(facts):
    return all(facts.get(key) is True for key in ("sourceBindingUnchanged", "compiler", "pythonHeaders", "configHeader"))


def python_runtime_postcheck(native, binding, plan, journal):
    if not journal.get("cleanupConfirmed") or journal.get("unit") and not cleanup_complete(journal):
        raise ProbeError("cleanup_unconfirmed", "Managed Python postcheck requires the administrator unit to be finished and collected")
    runtime_binding, facts = python_bound_observation(native, binding, plan["runtimeBinding"])
    if not python_prerequisites_ready(facts):
        raise ProbeError("runtime_headers_unavailable", "Matching packages are present but managed interpreter prerequisites did not pass")
    return {"runtimeBinding": runtime_binding, "runtimePrerequisites": facts}


def python_bound_observation(native, binding, expected):
    # Retained plans select the source kind; recovery never guesses from live state.
    if expected.get("schemaVersion") == 2:
        return python_runtime_observation(native, binding, expected, first_install=True)
    return python_runtime_observation(native, binding, expected)


def validate_python_preparation_mode(request, plan):
    expected = plan.get("recipeId") == PYTHON_RECIPE and plan.get("runtimeBinding", {}).get("schemaVersion") == 2
    if request.get("firstInstall", False) != expected:
        raise ProbeError("approval_mismatch", "First-install or repair mode differs from the retained source binding")


def trusted_module(name):
    found = importlib.util.find_spec(name)
    if not found or not found.origin or not found.origin.startswith("/usr/lib/python3/dist-packages/"):
        raise ProbeError("apt_metadata_unavailable", "Existing system python3-apt is unavailable; it was not installed")
    current = Path(os.path.realpath(found.origin))
    while str(current) != "/":
        info = current.stat()
        if info.st_uid != 0 or info.st_mode & 0o022:
            raise ProbeError("apt_metadata_unavailable", "System python3-apt ownership could not be trusted")
        current = current.parent


def reject_apt_trust_bypass():
    """Do not mistake locally forced trust for authenticated repository metadata."""
    files = [Path("/etc/apt/sources.list")]
    directory = Path("/etc/apt/sources.list.d")
    if directory.is_dir():
        files.extend(path for path in directory.iterdir() if path.suffix in (".list", ".sources"))
    if len(files) > 65:
        raise ProbeError("apt_metadata_unavailable", "APT source inventory exceeds the review bound")
    for path in files:
        if not path.exists():
            continue
        info = path.lstat()
        if not stat.S_ISREG(info.st_mode) or info.st_uid != 0 or info.st_mode & 0o022 or info.st_size > 65536:
            raise ProbeError("apt_metadata_unavailable", "APT source configuration is not safely owned or bounded")
        with path.open(encoding="utf-8") as stream:
            lines = [line.split("#", 1)[0] for line in stream.read(65537).splitlines()]
        content = "\n".join(lines)
        if re.search(r"(?im)(?:\btrusted\s*=\s*['\"]?(?:yes|true|1)\b|^\s*Trusted\s*:\s*(?:yes|true|1)\b)", content):
            raise ProbeError("apt_origin_untrusted", "APT source configuration forces trust without requiring signed metadata")


def load_cache():
    if os.environ.get("APT_CONFIG"):
        raise ProbeError("apt_metadata_unavailable", "A custom APT_CONFIG is outside this fixed recipe")
    reject_apt_trust_bypass()
    trusted_module("apt")
    trusted_module("apt_pkg")
    import apt
    import apt_pkg
    apt_pkg.init_config()
    # In-process resolver policy only; no host configuration is written.
    apt_pkg.config.set("APT::Install-Recommends", "false")
    apt_pkg.config.set("APT::Install-Suggests", "false")
    apt_pkg.config.set("APT::Get::AllowUnauthenticated", "false")
    apt_pkg.config.set("Acquire::AllowInsecureRepositories", "false")
    apt_pkg.config.set("Acquire::AllowDowngradeToInsecureRepositories", "false")
    apt_pkg.init_system()
    return apt.Cache(memonly=True)


def baseline_digest(cache, excluded):
    installed = []
    for package in cache:
        if package.installed and package.shortname not in excluded:
            version = package.installed
            installed.append([package.shortname, version.version, version.architecture])
            if len(installed) > 32768:
                raise ProbeError("apt_metadata_unavailable", "Installed package inventory exceeds the review bound")
    return hashlib.sha256(canonical(sorted(installed))).hexdigest()


def package_entry(package):
    version = package.candidate
    name = package.shortname
    if not version or not PACKAGE_NAME.fullmatch(name) or not VERSION.fullmatch(version.version):
        raise ProbeError("apt_candidate_invalid", "APT candidate name or version is invalid")
    if version.architecture not in ("arm64", "all"):
        raise ProbeError("apt_architecture_invalid", "APT transaction contains a foreign architecture")
    origins = [origin for origin in version.origins if origin.trusted and origin.origin == "Ubuntu" and
               origin.label == "Ubuntu" and origin.site in UBUNTU_SITES and origin.archive in UBUNTU_ARCHIVES]
    uri = urlsplit(version.uri or "")
    origins = [origin for origin in origins if origin.site == uri.hostname]
    if not origins or uri.scheme not in ("http", "https") or uri.username or uri.password:
        raise ProbeError("apt_origin_untrusted", "Candidate is not from existing trusted Ubuntu metadata")
    origin = sorted(origins, key=lambda value: (value.site, value.archive, value.component))[0]
    sha = version.record.get("SHA256", "")
    if not re.fullmatch(r"[a-f0-9]{64}", sha) or type(version.size) is not int or version.size <= 0:
        raise ProbeError("apt_hash_unknown", "Authenticated package SHA256 or archive byte size is unavailable")
    if type(version.installed_size) is not int or version.installed_size <= 0:
        raise ProbeError("apt_size_unknown", "Installed package byte size is unavailable")
    return {"name": name, "version": version.version, "architecture": version.architecture,
            "sha256": sha, "size": version.size, "installedSize": version.installed_size,
            "origin": {"site": origin.site, "archive": origin.archive, "component": origin.component,
                       "label": origin.label, "origin": origin.origin, "trusted": True}}


def new_install_only(package):
    # python-apt can mark new installs as upgrades too. Both review and resume
    # must classify by installed state plus install intent, not that lone flag.
    return (not package.is_installed and package.installed is None and package.marked_install and
            not package.marked_delete and not package.marked_downgrade and not package.marked_reinstall)


def risky_dependency(name):
    return name in RISKY_PACKAGES or name.startswith(RISKY_PACKAGE_PREFIXES)


def debian_version_compare(left, right):
    import apt_pkg  # load_cache already verifies the system module's ownership.
    return apt_pkg.version_compare(left, right)


def dependency_upgrade_eligible(package):
    old, candidate = package.installed, package.candidate
    if (not package.is_installed or old is None or candidate is None or not package.marked_upgrade or
            package.marked_delete or package.marked_downgrade or package.marked_reinstall or
            getattr(getattr(package, "_pkg", None), "current_state", None) != 6 or
            old.architecture != "arm64" or candidate.architecture != "arm64" or
            DEPENDENCY_UPGRADES.get(package.shortname) != (old.version, candidate.version)):
        return False
    return debian_version_compare(old.version, candidate.version) < 0


def package_change(package):
    old, candidate = package.installed, package.candidate
    name = package.shortname
    architecture = candidate.architecture if candidate is not None else old.architecture if old is not None else None
    installed = old.version if old is not None else None
    proposed = candidate.version if candidate is not None else None
    if (not isinstance(name, str) or not PACKAGE_NAME.fullmatch(name) or architecture not in ("arm64", "all") or
            any(value is not None and (not isinstance(value, str) or not VERSION.fullmatch(value)) for value in (installed, proposed))):
        raise ProbeError("apt_candidate_invalid", "Package change diagnostics are unavailable or malformed")
    change = ("remove" if package.marked_delete else "downgrade" if package.marked_downgrade else
              "reinstall" if package.marked_reinstall else "install" if new_install_only(package) else
              "upgrade" if package.is_installed and old is not None and candidate is not None and package.marked_upgrade and installed != proposed else "unknown")
    eligible = False
    if change == "upgrade" and dependency_upgrade_eligible(package):
        try:
            package_entry(package)  # Eligibility also requires trusted full candidate metadata.
            eligible = True
        except ProbeError:
            pass
    return {"name": name, "architecture": architecture, "installedVersion": installed,
            "candidateVersion": proposed, "change": change, "upgradeEligible": eligible}


def transaction_entry(package, dependency_upgrades=False):
    if not new_install_only(package) and not (dependency_upgrades and dependency_upgrade_eligible(package)):
        raise ProbeError("apt_existing_package_change", "The transaction contains an unapproved existing-package change")
    entry = package_entry(package)
    if dependency_upgrades:
        if risky_dependency(entry["name"]) or DEPENDENCY_CANDIDATES.get(entry["name"]) != (entry["version"], entry["architecture"]):
            raise ProbeError("apt_risky_dependency", "The OpenMPI closure contains an unapproved package or candidate")
        entry["installedVersion"] = package.installed.version if package.installed is not None else None
    return entry


def package_limits(dependency_upgrades=False, python=False):
    limits = {"maxPackages": len(PYTHON_PACKAGES) if python else MAX_PACKAGES, "maxDownloadBytes": MAX_DOWNLOAD,
              "maxInstalledBytes": MAX_INSTALLED, "maxInstallSeconds": MAX_INSTALL_SECONDS,
              "maxPlanBytes": MAX_PLAN_BYTES, "maxAttempts": MAX_ATTEMPTS,
              "recommends": False, "upgrades": dependency_upgrades, "removals": False}
    if dependency_upgrades:
        limits["maxDependencyUpgrades"] = len(DEPENDENCY_UPGRADES)
    return limits


def plan_has_upgrades(plan):
    return plan.get("recipeId") == UPGRADE_RECIPE and any(entry.get("installedVersion") is not None for entry in plan.get("packages", []))


def validate_dependency_consent(plan, approved=False):
    if type(approved) is not bool or approved != plan_has_upgrades(plan):
        raise ProbeError("approval_mismatch", "Explicit additional approval must match this exact dependency-upgrade plan")


def validate_journal_consent(journal):
    plan = journal.get("approvedPlan", {})
    if plan.get("recipeId") == UPGRADE_RECIPE:
        if "dependencyUpgradesApproved" not in journal:
            raise ProbeError("approval_mismatch", "Original dependency-upgrade consent is unavailable")
        validate_dependency_consent(plan, journal["dependencyUpgradesApproved"])


def resolve_python_transaction(cache, changes_out=None):
    if cache.broken_count:
        raise ProbeError("apt_state_broken", "Existing APT dependencies are broken; this fixed recipe will not repair them")
    if PYTHON_ROOT_PACKAGE not in cache or not cache[PYTHON_ROOT_PACKAGE].candidate:
        raise ProbeError("apt_candidate_missing", "Trusted metadata has no fixed Python-header candidate")
    root = cache[PYTHON_ROOT_PACKAGE]
    root_entry = package_entry(root)
    if (root_entry["version"], root_entry["architecture"]) != PYTHON_PACKAGES[PYTHON_ROOT_PACKAGE]:
        raise ProbeError("apt_candidate_unsupported", "The current Python-header candidate differs from the fixed reviewed recipe")
    root.mark_install(auto_fix=True, auto_inst=True, from_user=True)
    if cache.broken_count:
        raise ProbeError("apt_resolution_failed", "The fixed Python-header transaction cannot resolve without other changes")
    changes = cache.get_changes()
    if not 0 < len(changes) <= len(PYTHON_PACKAGES):
        raise ProbeError("apt_transaction_size", "The Python-header transaction is empty or exceeds its three-package policy")
    entries = []
    for package in changes:
        entry = transaction_entry(package)
        if PYTHON_PACKAGES.get(entry["name"]) != (entry["version"], entry["architecture"]):
            raise ProbeError("apt_candidate_unsupported", "The Python-header closure contains a package/version outside the fixed recipe")
        entries.append(entry)
    if len({e["name"] for e in entries}) != len(entries) or sum(e["size"] for e in entries) > MAX_DOWNLOAD or sum(e["installedSize"] for e in entries) > MAX_INSTALLED:
        raise ProbeError("apt_transaction_size", "The fixed Python-header transaction exceeds its bounds")
    if changes_out is not None:
        changes_out.extend(sorted((package_change(p) for p in changes), key=lambda row: row["name"]))
    baseline = []
    for name in PYTHON_PACKAGES:
        if name in cache and cache[name].installed:
            old = cache[name].installed
            if PYTHON_PACKAGES[name] != (old.version, old.architecture):
                raise ProbeError("apt_existing_package_change", "An existing Python-header package has a different version; no upgrade or downgrade is allowed")
            baseline.append({"name": name, "version": old.version, "architecture": old.architecture})
    return {"rootPackage": {"name": PYTHON_ROOT_PACKAGE, "version": root_entry["version"]},
            "installedBaseline": baseline, "installedBaselineDigest": baseline_digest(cache, {e["name"] for e in entries}),
            "packages": sorted(entries, key=lambda entry: entry["name"])}


def resolve_transaction(cache, dependency_upgrades=False, changes_out=None, python=False):
    if python:
        if dependency_upgrades:
            raise ProbeError("approval_mismatch", "Python-header preparation has no upgrade policy")
        return resolve_python_transaction(cache, changes_out)
    if cache.broken_count:
        raise ProbeError("apt_state_broken", "Existing APT dependencies are broken; this recipe will not repair them")
    if ROOT_PACKAGE not in cache or not cache[ROOT_PACKAGE].candidate:
        raise ProbeError("apt_candidate_missing", "Trusted metadata has no libopenmpi-dev candidate")
    root = cache[ROOT_PACKAGE]
    root_entry = package_entry(root)
    if not root_entry["version"].startswith("4.1."):
        raise ProbeError("apt_candidate_unsupported", "This recipe supports the Ubuntu OpenMPI 4.1 family")
    root.mark_install(auto_fix=True, auto_inst=True, from_user=True)
    if cache.broken_count:
        raise ProbeError("apt_resolution_failed", "The no-recommends package transaction cannot be resolved")
    changes = cache.get_changes()
    if not 0 < len(changes) <= MAX_PACKAGES:
        raise ProbeError("apt_transaction_size", "The transaction is empty or exceeds the bounded package count")
    if changes_out is not None:
        changes_out.extend(sorted((package_change(package) for package in changes), key=lambda row: row["name"]))
    entries = []
    for package in changes:
        entries.append(transaction_entry(package, dependency_upgrades))
    if len({entry["name"] for entry in entries}) != len(entries):
        raise ProbeError("apt_transaction_invalid", "The transaction contains duplicate packages")
    if sum(entry["size"] for entry in entries) > MAX_DOWNLOAD or sum(entry["installedSize"] for entry in entries) > MAX_INSTALLED:
        raise ProbeError("apt_transaction_size", "The package transaction exceeds its download or installed-size bound")
    # Capture installed dependencies actually available to the resolver, without
    # exposing a machine-wide package inventory in the product review.
    baseline = []
    for name in MPI_PACKAGES:
        if name in cache and cache[name].installed:
            version = cache[name].installed
            baseline.append({"name": name, "version": version.version, "architecture": version.architecture})
    return {"rootPackage": {"name": ROOT_PACKAGE, "version": root_entry["version"]},
            "installedBaseline": baseline, "installedBaselineDigest": baseline_digest(cache, {entry["name"] for entry in entries}),
            "packages": sorted(entries, key=lambda entry: entry["name"])}


def review(request, native=None, cache_loader=load_cache):
    validate_request(request)
    native = native or Native()
    result = {"schemaVersion": 1, "action": "review", "state": "blocked", "effectsApplied": False,
              "runtimeValidated": False, "identity": None, "installedFamily": [], "plan": None,
              "access": {"metadata": "unknown", "packageAccess": "not_checked", "administrator": "not_checked"},
               "errors": []}
    dependency_upgrades = request.get("dependencyUpgradeReview", False)
    python = request.get("runtimePreparation", False)
    local_archives = False
    changes = []
    try:
        result["identity"] = identity(request, native)
        if python:
            if request.get("firstInstall"):
                result["runtimeBinding"], result["runtimePrerequisites"] = python_runtime_observation(native, result["identity"], first_install=True)
            else:
                result["runtimeBinding"], result["runtimePrerequisites"] = python_runtime_observation(native, result["identity"])
            if not result["runtimePrerequisites"]["compiler"]:
                raise ProbeError("compiler_missing", "An existing gcc or clang is required; this fixed recipe installs only Python development headers")
        result["installedFamily"] = installed_family(native, python)
        if matching_family(result["installedFamily"], python):
            if python and not python_prerequisites_ready(result["runtimePrerequisites"]):
                raise ProbeError("runtime_headers_unavailable", "Packages are installed but the active interpreter headers are unavailable; no blind reinstall is allowed")
            result["state"] = "already_installed"
            result["access"]["metadata"] = "not_needed"
            return result
        cache = cache_loader()
        try:
            try:
                transaction = resolve_transaction(cache, dependency_upgrades, changes, python)
            except ProbeError as error:
                if not (python and request.get("firstInstall") and error.code in ("apt_candidate_missing", "apt_candidate_unsupported")):
                    raise
                transaction = resolve_python_local_transaction(cache, native, result["identity"])
                local_archives = True
        finally:
            cache.close()
        plan = {"schemaVersion": 1, "recipeId": PYTHON_RECIPE if python else UPGRADE_RECIPE if dependency_upgrades else RECIPE,
                "identity": result["identity"], **transaction, "unitPolicy": UNIT_POLICY,
                "archivePolicy": PYTHON_LOCAL_ARCHIVE_POLICY if local_archives else PYTHON_SNAPSHOT_ARCHIVE_POLICY if python else UPGRADE_ARCHIVE_POLICY if dependency_upgrades else ARCHIVE_POLICY,
                "limits": package_limits(dependency_upgrades, python), "effects": PYTHON_LOCAL_EFFECTS if local_archives else python_plan_effects(request.get("firstInstall"), True) if python else UPGRADE_EFFECTS if dependency_upgrades else EFFECTS}
        if python:
            validate_snapshot_packages(plan["packages"])
            plan["runtimeBinding"] = result["runtimeBinding"]
        plan["planDigest"] = digest(plan)
        if len(canonical(plan)) > MAX_PLAN_BYTES:
            raise ProbeError("apt_transaction_size", "Package review exceeds the bound needed for durable unit and package receipts")
        result.update(state="reviewed", plan=plan)
        result["access"]["metadata"] = "verified_local_archive" if local_archives else "apt_trusted_cached"
    except ProbeError as error:
        result["errors"].append({"code": error.code, "phase": "review", "message": str(error)})
    except (OSError, ValueError, KeyError, TypeError, AttributeError, ImportError):
        result["errors"].append({"code": "apt_metadata_unavailable", "phase": "review",
                                  "message": "Native package metadata or dependencies could not be read safely"})
    if changes and (result["state"] == "blocked" or dependency_upgrades):
        result["changes"] = changes
    return result


def validate_approval(plan, current_identity):
    if not isinstance(plan, dict) or plan.get("schemaVersion") != 1 or plan.get("recipeId") not in (RECIPE, UPGRADE_RECIPE, PYTHON_RECIPE) or plan.get("identity") != current_identity:
        raise ProbeError("approval_mismatch", "Approved package recipe or participant binding changed")
    dependency_upgrades = plan["recipeId"] == UPGRADE_RECIPE
    python = plan["recipeId"] == PYTHON_RECIPE
    root_name = PYTHON_ROOT_PACKAGE if python else ROOT_PACKAGE
    family_names = PYTHON_PACKAGES if python else MPI_PACKAGES
    if not isinstance(plan.get("planDigest"), str) or plan["planDigest"] != digest(plan):
        raise ProbeError("approval_mismatch", "Approved package plan digest is invalid")
    if len(canonical(plan)) > MAX_PLAN_BYTES:
        raise ProbeError("approval_mismatch", "Approved package plan exceeds the durable receipt bound")
    fields = {"schemaVersion", "recipeId", "identity", "rootPackage", "installedBaseline", "installedBaselineDigest", "packages", "limits", "unitPolicy", "archivePolicy", "effects", "planDigest"} | ({"runtimeBinding"} if python else set())
    first_install = python and isinstance(plan.get("runtimeBinding"), dict) and plan["runtimeBinding"].get("schemaVersion") == 2
    policy = python_archive_policy(plan) if python else UPGRADE_ARCHIVE_POLICY if dependency_upgrades else ARCHIVE_POLICY
    local_archives = python and policy == PYTHON_LOCAL_ARCHIVE_POLICY
    if local_archives and not first_install:
        raise ProbeError("approval_mismatch", "Local archive preparation requires the bound absent-installation Python source")
    effects = PYTHON_LOCAL_EFFECTS if local_archives else python_plan_effects(first_install, policy == PYTHON_SNAPSHOT_ARCHIVE_POLICY) if python else UPGRADE_EFFECTS if dependency_upgrades else EFFECTS
    if set(plan) != fields or plan.get("effects") != effects or plan.get("unitPolicy") != UNIT_POLICY or plan.get("archivePolicy") != policy:
        raise ProbeError("approval_mismatch", "Approved package plan contains unrecognized effects or fields")
    if python:
        if policy in (PYTHON_SNAPSHOT_ARCHIVE_POLICY, PYTHON_LOCAL_ARCHIVE_POLICY):
            validate_snapshot_packages(plan.get("packages", []))
        validate_python_binding(plan.get("runtimeBinding"), current_identity)
    if not isinstance(plan.get("installedBaselineDigest"), str) or not re.fullmatch(r"[a-f0-9]{64}", plan["installedBaselineDigest"]):
        raise ProbeError("approval_mismatch", "Approved installed package baseline is invalid")
    root = plan.get("rootPackage")
    if not isinstance(root, dict) or set(root) != {"name", "version"} or not isinstance(root.get("version"), str) or not VERSION.fullmatch(root["version"]):
        raise ProbeError("approval_mismatch", "Approved MPI root candidate is malformed")
    baseline = plan.get("installedBaseline")
    if not isinstance(baseline, list) or len(baseline) > len(family_names) or any(not isinstance(row, dict) or set(row) != {"name", "version", "architecture"} or row["name"] not in family_names or not isinstance(row["version"], str) or not VERSION.fullmatch(row["version"]) or row["architecture"] not in ("arm64", "all") for row in baseline):
        raise ProbeError("approval_mismatch", "Approved installed MPI baseline is malformed")
    if python and (root.get("version") != PYTHON_PACKAGES[PYTHON_ROOT_PACKAGE][0] or
                   any(PYTHON_PACKAGES[row["name"]] != (row["version"], row["architecture"]) for row in baseline)):
        raise ProbeError("approval_mismatch", "The approved Python-header baseline or root version changed")
    expected_limits = package_limits(dependency_upgrades, python)
    if plan.get("limits") != expected_limits or plan.get("rootPackage", {}).get("name") != root_name:
        raise ProbeError("approval_mismatch", "Approved package limits or root package changed")
    entries = plan.get("packages")
    if not isinstance(entries, list) or not 0 < len(entries) <= expected_limits["maxPackages"]:
        raise ProbeError("approval_mismatch", "Approved transaction has an invalid package count")
    names = set()
    for entry in entries:
        if not isinstance(entry, dict) or not isinstance(entry.get("name"), str) or not PACKAGE_NAME.fullmatch(entry["name"]) or entry["name"] in names:
            raise ProbeError("approval_mismatch", "Approved transaction has invalid or duplicate package names")
        fields = {"name", "version", "architecture", "sha256", "size", "installedSize", "origin"} | ({"installedVersion"} if dependency_upgrades else set())
        if set(entry) != fields or not isinstance(entry.get("version"), str) or not VERSION.fullmatch(entry["version"]) or entry.get("architecture") not in ("arm64", "all") or not isinstance(entry.get("sha256"), str) or not re.fullmatch(r"[a-f0-9]{64}", entry["sha256"]):
            raise ProbeError("approval_mismatch", "Approved package artifact fields are malformed")
        if python and PYTHON_PACKAGES.get(entry["name"]) != (entry["version"], entry["architecture"]):
            raise ProbeError("approval_mismatch", "Approved Python-header package is outside the fixed name/version policy")
        if dependency_upgrades:
            old = entry["installedVersion"]
            if risky_dependency(entry["name"]) or DEPENDENCY_CANDIDATES.get(entry["name"]) != (entry["version"], entry["architecture"]) or (old is not None and (not isinstance(old, str) or entry["architecture"] != "arm64" or DEPENDENCY_UPGRADES.get(entry["name"]) != (old, entry["version"]))):
                raise ProbeError("approval_mismatch", "Approved dependency upgrade is outside the exact private policy")
        origin = entry.get("origin")
        if local_archives:
            if origin != PYTHON_LOCAL_ORIGIN or entry["installedSize"] != PYTHON_LOCAL_INSTALLED_KIB[entry["name"]] * 1024:
                raise ProbeError("approval_mismatch", "Local archive provenance or control size changed")
        elif not isinstance(origin, dict) or set(origin) != {"site", "archive", "component", "label", "origin", "trusted"} or origin["site"] not in UBUNTU_SITES or origin["archive"] not in UBUNTU_ARCHIVES or origin["component"] not in ("main", "universe", "restricted", "multiverse") or origin["label"] != "Ubuntu" or origin["origin"] != "Ubuntu" or origin["trusted"] is not True:
            raise ProbeError("approval_mismatch", "Approved package origin is outside the fixed Ubuntu recipe")
        if any(type(entry[field]) is not int or entry[field] <= 0 for field in ("size", "installedSize")):
            raise ProbeError("approval_mismatch", "Approved package byte sizes are invalid")
        names.add(entry["name"])
    if sum(entry["size"] for entry in entries) > MAX_DOWNLOAD or sum(entry["installedSize"] for entry in entries) > MAX_INSTALLED:
        raise ProbeError("approval_mismatch", "Approved package transaction exceeds fixed byte bounds")
    if root_name not in names and not any(row.get("name") == root_name for row in plan.get("installedBaseline", [])):
        raise ProbeError("approval_mismatch", "Approved transaction does not include the fixed MPI root")


def pending_transaction(cache, plan):
    """Allow completed members of this transaction, while rejecting foreign drift."""
    approved = {entry["name"]: entry for entry in plan["packages"]}
    dependency_upgrades = plan["recipeId"] == UPGRADE_RECIPE
    root_name = PYTHON_ROOT_PACKAGE if plan["recipeId"] == PYTHON_RECIPE else ROOT_PACKAGE
    if baseline_digest(cache, set(approved)) != plan["installedBaselineDigest"]:
        raise ProbeError("package_state_changed", "Packages outside the approved transaction changed; review again")
    if plan["recipeId"] == PYTHON_RECIPE and python_archive_policy(plan) == PYTHON_LOCAL_ARCHIVE_POLICY:
        missing = []
        for name, entry in approved.items():
            package = cache[name] if name in cache else None
            old = package.installed if package else None
            if old is None:
                missing.append(entry)
            elif (old.version, old.architecture) != (entry["version"], entry["architecture"]) or getattr(getattr(package, "_pkg", None), "current_state", None) != 6:
                raise ProbeError("package_state_changed", "A local-archive package is not missing or fully installed at its reviewed version")
        if cache.broken_count:
            raise ProbeError("package_state_broken", "Local archive package dependencies are unresolved")
        return sorted(missing, key=lambda entry: entry["name"])
    missing = []
    for name, entry in approved.items():
        if name not in cache:
            raise ProbeError("package_state_changed", "Approved package metadata disappeared")
        package = cache[name]
        if package.installed:
            # apt_pkg.CURSTATE_INSTALLED=6. Do not promise generic configuration
            # repair for an unpacked/half-configured interrupted package.
            if getattr(getattr(package, "_pkg", None), "current_state", None) != 6:
                raise ProbeError("approved_package_repair_required", "An approved package is not confirmed fully configured; bounded repair requires a separate review")
            if package.installed.architecture != entry["architecture"]:
                raise ProbeError("package_state_changed", "An approved package architecture changed")
            if package.installed.version == entry["version"]:
                continue
            if not dependency_upgrades or entry["installedVersion"] is None or package.installed.version != entry["installedVersion"]:
                raise ProbeError("package_state_changed", "An approved package is installed at an unreviewed version")
            observed = package_entry(package)
            observed["installedVersion"] = package.installed.version
            if observed != entry:
                raise ProbeError("package_state_changed", "Approved upgrade metadata changed")
            missing.append(entry)
        else:
            observed = package_entry(package)
            if dependency_upgrades:
                if entry["installedVersion"] is not None:
                    raise ProbeError("package_state_changed", "A reviewed installed dependency disappeared")
                observed["installedVersion"] = None
            if observed != entry:
                raise ProbeError("package_state_changed", "Approved package version, checksum, bytes or origin changed")
            missing.append(entry)
    if not missing:
        if cache.broken_count:
            raise ProbeError("package_state_broken", "Installed approved packages still have unresolved dependencies")
        return []
    root = cache[root_name]
    if not root.candidate or root.candidate.version != plan["rootPackage"]["version"]:
        raise ProbeError("package_state_changed", "The reviewed libopenmpi-dev candidate changed")
    root.mark_install(auto_fix=True, auto_inst=True, from_user=True)
    if dependency_upgrades:
        for entry in missing:
            cache[entry["name"]].mark_install(auto_fix=True, auto_inst=True, from_user=True)
    if cache.broken_count:
        raise ProbeError("apt_resolution_failed", "Approved missing packages cannot resolve the remaining dependencies")
    changes = cache.get_changes()
    actual = []
    for package in changes:
        try:
            entry = transaction_entry(package, dependency_upgrades)
        except ProbeError as error:
            raise ProbeError("package_state_changed", "Resume contains an unapproved package change") from error
        if approved.get(entry["name"]) != entry:
            raise ProbeError("package_state_changed", "Resume requires an unapproved package")
        actual.append(entry)
    if {entry["name"] for entry in actual} != {entry["name"] for entry in missing}:
        raise ProbeError("package_state_changed", "Remaining dependency closure differs from the approved transaction")
    return sorted(missing, key=lambda entry: entry["name"])


def operation_path(binding, operation_id, create=False):
    home = Path(binding["home"])
    if str(home) != os.path.realpath(home):
        raise ProbeError("state_ownership", "Selected home is redirected")
    info = home.lstat()
    if not stat.S_ISDIR(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022:
        raise ProbeError("state_ownership", "Selected home is not safely owned")
    root = home / ".config" / "Nvidia Corporation" / "Personal AI Router" / "engine-bin" / "diagnostic-tools" / "mpi-packages"
    current = home
    for part in root.relative_to(home).parts:
        current = current / part
        if not create and not path_present(current):
            return root / (operation_id + ".json")
        if create and not os.path.lexists(current):
            current.mkdir(mode=0o700)
        info = current.lstat()
        if not stat.S_ISDIR(info.st_mode) or info.st_uid != binding["uid"] or info.st_mode & 0o022:
            raise ProbeError("state_ownership", "Package operation directory is not private and owned")
    return root / (operation_id + ".json")


@contextmanager
def operation_lock(path, binding):
    import fcntl
    fd = os.open(path.parent / ".lock", os.O_RDWR | os.O_CREAT | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or info.st_uid != binding["uid"] or stat.S_IMODE(info.st_mode) != 0o600:
            raise ProbeError("state_ownership", "MPI transaction lock is not safely owned")
        try:
            fcntl.flock(fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ProbeError("operation_busy", "Another owned MPI package transaction is active") from None
        yield
    finally:
        os.close(fd)


def read_journal(path, native, binding):
    if not path_present(path):
        return None
    info = path.lstat()
    if not stat.S_ISREG(info.st_mode) or info.st_uid != binding["uid"] or stat.S_IMODE(info.st_mode) != 0o600:
        raise ProbeError("state_ownership", "Package operation journal is not safely owned")
    raw = native.read(str(path), 128 << 10)
    saved = json.loads(raw)
    if not isinstance(saved, dict) or saved.get("owner") != JOURNAL_OWNER or saved.get("identity") != binding or saved.get("operationId") != path.stem:
        raise ProbeError("state_ownership", "Package operation journal ownership does not match")
    validate_journal_consent(saved)
    return saved


def path_present(path):
    try:
        os.lstat(path)
        return True
    except FileNotFoundError:
        return False


def write_journal(path, value):
    if value.get("unit"):
        value["cleanupConfirmed"] = cleanup_complete(value)
    data = canonical(value)
    if len(data) > 128 << 10:
        raise ProbeError("state_limit", "Package operation journal exceeds its size bound")
    temporary = path.with_name(path.name + "." + os.urandom(8).hex() + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(data)
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
    finally:
        if os.path.lexists(temporary):
            os.unlink(temporary)


def process_start(pid):
    try:
        with open("/proc/" + str(pid) + "/stat", encoding="ascii") as stream:
            fields = stream.read(4096).rsplit(") ", 1)[1].split()
        return fields[19]
    except FileNotFoundError:
        return None
    except (OSError, IndexError):
        raise ProbeError("cleanup_unconfirmed", "Owned package process identity could not be inspected") from None


def apt_argv(entries, native):
    apt_get = native.resolve("apt-get")
    return [apt_get, "-y", "--no-install-recommends", "--no-remove", "--no-upgrade",
            "-o", "APT::Get::AllowUnauthenticated=false", "-o", "Acquire::AllowInsecureRepositories=false",
            "-o", "Acquire::AllowDowngradeToInsecureRepositories=false", "-o", "Acquire::Retries=0",
            "-o", "Acquire::http::Timeout=15", "-o", "Acquire::https::Timeout=15",
            "-o", "DPkg::Lock::Timeout=5", "-o", "Dpkg::Options::=--force-confold", "install",
            *[entry["name"] + ":" + entry["architecture"] + "=" + entry["version"] for entry in entries]]


def unit_spec(journal, entries, native):
    operation_id = journal["operationId"]
    name = "pair-nccl-mpi-" + operation_id + ".service"
    label = "PAIR Python headers" if journal["approvedPlan"]["recipeId"] == PYTHON_RECIPE else "PAIR MPI"
    return {"name": name,
            "description": label + " uid=" + str(journal["identity"]["uid"]) + " operation=" + operation_id + " plan=" + journal["approvedPlan"]["planDigest"],
            "controlGroup": "/system.slice/" + name, "runtimeDirectory": name[:-8],
            "execArgv": worker_argv(journal, entries, "install", native), "packages": entries,
            "observed": False, "invocationId": None, "collected": False}


def sudo_argv(command_argv, password_present, native):
    return [native.resolve("sudo"), *(["-S", "-p", ""] if password_present else ["-n"]), "--", *command_argv]


def worker_argv(journal, entries, action, native):
    source, _ = archive_worker(journal["approvedPlan"])
    dependency_upgrades = journal["approvedPlan"]["recipeId"] == UPGRADE_RECIPE
    validate_journal_consent(journal)
    manifest = {"action": action, "operationId": journal["operationId"], "planDigest": journal["approvedPlan"]["planDigest"]}
    if action == "install":
        fields = ("name", "version", "architecture", "sha256", "size") + (("installedVersion",) if dependency_upgrades else ())
        manifest["packages"] = [{key: entry[key] for key in fields} for entry in journal["approvedPlan"]["packages"]]
        manifest["installPackages"] = [{key: entry[key] for key in fields} for entry in entries]
        if dependency_upgrades:
            manifest.update(recipeId=UPGRADE_RECIPE, dependencyUpgradesApproved=journal["dependencyUpgradesApproved"])
        elif journal["approvedPlan"]["recipeId"] == PYTHON_RECIPE:
            manifest["recipeId"] = PYTHON_RECIPE
            if python_archive_policy(journal["approvedPlan"]) == PYTHON_LOCAL_ARCHIVE_POLICY:
                manifest.update(identity=journal["approvedPlan"]["identity"], runtimeBinding=journal["approvedPlan"]["runtimeBinding"], installedBaselineDigest=journal["approvedPlan"]["installedBaselineDigest"])
    script = "s=__import__('base64').b64decode('" + base64.b64encode(source.encode()).decode() + "');exec(s,{'__name__':'__main__','_workerSource':s})"
    return [native.resolve("python3"), "-I", "-c", script, base64.b64encode(canonical(manifest)).decode()]


def fixed_install_argv(entries, password_present, native, journal):
    unit = unit_spec(journal, entries, native)
    return sudo_argv([native.resolve("systemd-run"), "--system", "--wait", "--no-ask-password",
                      "--unit=" + unit["name"], "--description=" + unit["description"],
                      "--property=Type=exec", "--property=ExitType=cgroup", "--property=RuntimeMaxSec=180", "--property=TimeoutStartSec=10", "--property=TimeoutStopSec=10",
                      "--property=KillMode=control-group", "--property=Restart=no", "--property=User=root",
                      "--property=StandardInput=null", "--property=RemainAfterExit=yes", "--property=CollectMode=inactive",
                      "--property=RuntimeDirectory=" + unit["runtimeDirectory"], "--property=RuntimeDirectoryMode=0700", "--property=RuntimeDirectoryPreserve=yes",
                      "--property=SendSIGKILL=yes", "--setenv=DEBIAN_FRONTEND=noninteractive", "--", *unit["execArgv"]],
                     password_present, native)


UNIT_PROPERTIES = ("Id", "Description", "LoadState", "ActiveState", "SubState", "Transient", "Type", "ExitType", "User",
                   "RuntimeMaxUSec", "TimeoutStartUSec", "TimeoutStopUSec", "KillMode", "Restart", "StandardInput", "RemainAfterExit",
                   "CollectMode", "SendSIGKILL", "ControlGroup", "InvocationID", "RuntimeDirectory", "RuntimeDirectoryMode", "RuntimeDirectoryPreserve", "Result",
                   "ExecMainCode", "ExecMainStatus", "MainPID", "ControlPID")


def duration_usec(value):
    if value.isdigit():
        return int(value)
    match = re.fullmatch(r"([0-9]+)(us|ms|s|min)", value)
    return int(match[1]) * {"us": 1, "ms": 1000, "s": 1000000, "min": 60000000}[match[2]] if match else None


def inspect_unit(unit, native):
    code, output = native.run([native.resolve("systemctl"), "--system", "--no-pager", "show",
                               "--property=" + ",".join(UNIT_PROPERTIES), "--", unit["name"]])
    values = {}
    for line in output.splitlines():
        key, separator, value = line.partition("=")
        if not separator or key not in UNIT_PROPERTIES or key in values:
            raise ProbeError("unit_metadata_unknown", "Transient-unit metadata is malformed")
        values[key] = value
    if values.get("LoadState") == "not-found" and code in (0, 1, 4):
        return None
    if code or values.get("LoadState") != "loaded":
        raise ProbeError("unit_metadata_unknown", "System manager could not inspect the exact transient unit")
    if set(values) != set(UNIT_PROPERTIES):
        raise ProbeError("unit_metadata_unknown", "System manager omitted required transient-unit properties")
    expected = {"Id": unit["name"], "Description": unit["description"], "Transient": "yes", "Type": "exec", "ExitType": "cgroup", "User": "root",
                "KillMode": "control-group", "Restart": "no", "StandardInput": "null", "RemainAfterExit": "yes",
                "CollectMode": "inactive", "SendSIGKILL": "yes", "RuntimeDirectory": unit["runtimeDirectory"],
                "RuntimeDirectoryMode": "0700", "RuntimeDirectoryPreserve": "yes"}
    if any(values.get(key) != value for key, value in expected.items()) or duration_usec(values.get("RuntimeMaxUSec", "")) != 180000000 or duration_usec(values.get("TimeoutStartUSec", "")) != 10000000 or duration_usec(values.get("TimeoutStopUSec", "")) != 10000000:
        raise ProbeError("unit_ownership_mismatch", "Transient-unit identity or lifecycle policy differs from the reviewed operation")
    invocation = values.get("InvocationID", "")
    if not invocation and values.get("ActiveState") == "activating" and not unit.get("observed"):
        raise ProbeError("unit_pending", "Transient unit is still acquiring its invocation identity")
    if not re.fullmatch(r"[a-f0-9]{32}", invocation) or unit.get("invocationId") not in (None, invocation):
        raise ProbeError("unit_ownership_mismatch", "Transient-unit invocation changed")
    if values.get("ControlGroup") not in ("", unit["controlGroup"]):
        raise ProbeError("unit_ownership_mismatch", "Transient-unit cgroup differs from this operation")
    if unit_exec_argv(unit, native) != unit["execArgv"]:
        raise ProbeError("unit_ownership_mismatch", "Transient-unit executable or arguments differ from the approved transaction")
    for key in ("MainPID", "ControlPID", "ExecMainCode", "ExecMainStatus"):
        if not values.get(key, "").isdigit():
            raise ProbeError("unit_metadata_unknown", "Transient-unit process/result metadata is incomplete")
    return values


def bounded_client(argv, password=None, seconds=15, limit=65536):
    parent_pid = os.getpid()
    child = subprocess.Popen(argv, stdin=subprocess.PIPE if password is not None else subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True,
                             env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
                             preexec_fn=lambda: probe_parent_death(parent_pid))
    data = bytearray(); deadline = time.monotonic() + seconds
    try:
        if password is not None:
            child.stdin.write((password + "\n").encode()); child.stdin.close()
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout, selectors.EVENT_READ)
            while selector.get_map():
                if time.monotonic() >= deadline: raise ProbeError("control_timeout", "Fixed owned-operation control timed out")
                for key, _ in selector.select(0.1):
                    chunk = os.read(key.fd, 4096)
                    if not chunk: selector.unregister(key.fileobj)
                    data.extend(chunk)
                    if len(data) > limit: raise ProbeError("output_limit", "Fixed owned-operation response exceeded its bound")
        child.wait(timeout=max(0.01, deadline - time.monotonic()))
        if child.returncode: raise ProbeError("control_failed", "Fixed owned-operation control was denied or failed")
        return bytes(data)
    finally:
        if child.poll() is None:
            try: os.kill(child.pid, signal.SIGTERM)
            except (PermissionError, ProcessLookupError): pass
        child.stdout.close()


def unit_exec_argv(unit, native):
    escaped = "".join(char if char.isalnum() else "_" + format(ord(char), "02x") for char in unit["name"])
    argv = [native.resolve("busctl"), "--system", "--json=short", "get-property", "org.freedesktop.systemd1",
            "/org/freedesktop/systemd1/unit/" + escaped, "org.freedesktop.systemd1.Service", "ExecStart"]
    if type(native) is Native:
        raw = bounded_client(argv, seconds=5)
    else:
        code, raw = native.run(argv)
        if code: raise ProbeError("unit_metadata_unknown", "Exact unit argv is unavailable")
    body = json.loads(raw)
    data = body.get("data")
    # busctl can wrap an array property in the single-value message body.
    if isinstance(data, list) and len(data) == 1 and isinstance(data[0], list) and len(data[0]) == 1 and isinstance(data[0][0], list):
        data = data[0]
    if not isinstance(data, list) or len(data) != 1 or not isinstance(data[0], list) or len(data[0]) < 2 or not isinstance(data[0][1], list):
        raise ProbeError("unit_metadata_unknown", "Exact unit argv response is malformed")
    if not data[0][1] or data[0][0] != data[0][1][0]:
        raise ProbeError("unit_metadata_unknown", "Exact unit executable and argv disagree")
    return data[0][1]


def cgroup_state(unit):
    # cgroup.events reports population for this group and all descendant groups.
    # Require unified cgroup v2 before interpreting absence as cleanup.
    root = Path("/sys/fs/cgroup")
    if not (root / "cgroup.controllers").is_file():
        raise ProbeError("cgroup_unknown", "Unified cgroup v2 is unavailable")
    group = root / unit["controlGroup"].lstrip("/")
    if not os.path.lexists(group):
        return "absent"
    if group.is_symlink() or not group.is_dir():
        raise ProbeError("cgroup_unknown", "Owned cgroup path is invalid")
    with (group / "cgroup.events").open(encoding="ascii") as stream:
        text = stream.read(4097)
    matches = re.findall(r"^populated ([01])$", text, re.MULTILINE)
    if len(text) > 4096 or len(matches) != 1:
        raise ProbeError("cgroup_unknown", "Owned cgroup population is unavailable")
    return "populated" if matches[0] == "1" else "empty"


def reconcile_unit(journal, native):
    unit = journal.get("unit")
    journal["cleanupConfirmed"] = False
    if not unit:
        raise ProbeError("cleanup_unconfirmed", "No owned transient-unit receipt is available")
    unit["processesGone"] = False
    approved = journal.get("approvedPlan", {}).get("packages", [])
    if not unit.get("packages") or any(entry not in approved for entry in unit["packages"]):
        raise ProbeError("unit_ownership_mismatch", "Transient-unit package list is outside the approved plan")
    expected = unit_spec(journal, unit["packages"], native)
    if any(unit.get(key) != expected[key] for key in ("name", "description", "controlGroup", "execArgv")):
        raise ProbeError("unit_ownership_mismatch", "Recorded transient-unit binding changed")
    values = inspect_unit(unit, native)
    population = cgroup_state(unit)
    unit["cgroupState"] = population
    if values is None:
        if not unit.get("observed") or not unit.get("invocationId") or population not in ("empty", "absent"):
            raise ProbeError("cleanup_unconfirmed", "Unit absence lacks prior ownership and empty-cgroup proof")
        unit["collected"] = True
        unit["processesGone"] = True
        journal["cleanupConfirmed"] = cleanup_complete(journal)
        return None
    unit.update(observed=True, invocationId=values["InvocationID"], result=values["Result"],
                execMainCode=int(values["ExecMainCode"]), execMainStatus=int(values["ExecMainStatus"]),
                activeState=values["ActiveState"], subState=values["SubState"])
    ended = (values["ActiveState"] in ("inactive", "failed") or values["SubState"] == "exited") and values["MainPID"] == "0" and values["ControlPID"] == "0"
    unit["processesGone"] = bool(ended and population in ("empty", "absent"))
    journal["cleanupConfirmed"] = cleanup_complete(journal)
    if ended and "jobResult" not in unit:
        unit["jobResult"] = {"result": values["Result"], "execMainCode": int(values["ExecMainCode"]), "execMainStatus": int(values["ExecMainStatus"])}
    return values


def cleanup_complete(journal):
    unit = journal.get("unit") or {}
    return bool(unit.get("processesGone") and unit.get("collected") and unit.get("cacheRemoved"))


def privileged_control(action, unit, password, native):
    if action not in ("stop", "reset-failed"):
        raise ProbeError("invalid_action", "Only owned transient-unit cleanup is supported")
    if inspect_unit(unit, native) is None:
        return
    argv = sudo_argv([native.resolve("systemctl"), "--system", "--no-ask-password", action, "--", unit["name"]], password is not None, native)
    # This client controls only the verified transient unit. Private stdin is
    # consumed by sudo; systemctl does not forward it to the service.
    parent_pid = os.getpid()
    child = subprocess.Popen(argv, stdin=subprocess.PIPE if password is not None else subprocess.DEVNULL,
                             stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, start_new_session=True,
                             env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C"},
                             preexec_fn=lambda: probe_parent_death(parent_pid))
    try:
        child.communicate((password + "\n").encode() if password is not None else None, timeout=15)
        if child.returncode:
            raise ProbeError("unit_cleanup_denied", "Normal sudo did not complete owned transient-unit cleanup")
    except subprocess.TimeoutExpired:
        raise ProbeError("cleanup_unconfirmed", "Owned transient-unit control did not finish") from None
    finally:
        if child.poll() is None:
            try:
                os.kill(child.pid, signal.SIGTERM)
            except (PermissionError, ProcessLookupError):
                pass


def validate_archive_receipt(receipt, journal):
    required = {"schemaVersion", "operationId", "planDigest", "workerSha256", "state", "phase", "verified", "downloadExitCode", "installExitCode", "archives", "errorCode"}
    _, expected_worker = archive_worker(journal["approvedPlan"])
    if not isinstance(receipt, dict) or set(receipt) != required or receipt["schemaVersion"] != 1 or receipt["operationId"] != journal["operationId"] or receipt["planDigest"] != journal["approvedPlan"]["planDigest"] or receipt["workerSha256"] != expected_worker:
        raise ProbeError("archive_receipt_invalid", "Root archive receipt does not match this operation, plan and worker")
    if receipt["state"] not in ("succeeded", "failed") or receipt["phase"] not in ("download", "verify", "install", "complete") or type(receipt["verified"]) is not bool or not isinstance(receipt["archives"], list):
        raise ProbeError("archive_receipt_invalid", "Root archive result fields are malformed")
    # Native Linux subprocess statuses preserve signals as -1..-64; ordinary
    # exits are 0..255. Unknown remains None, and success still requires zero.
    if any(value is not None and (type(value) is not int or not -64 <= value <= 255) for value in (receipt["downloadExitCode"], receipt["installExitCode"])):
        raise ProbeError("archive_receipt_invalid", "Root archive exit statuses are malformed")
    if receipt["verified"]:
        expected = {e["name"]: e for e in journal["approvedPlan"]["packages"]}
        seen = set()
        for row in receipt["archives"]:
            if not isinstance(row, dict) or set(row) != {"name", "version", "architecture", "fileName", "size", "sha256"} or row.get("name") in seen or row.get("name") not in expected:
                raise ProbeError("archive_receipt_invalid", "Measured archive coverage is not exactly the approved set")
            entry = expected[row["name"]]
            if any(row[key] != entry[key] for key in ("version", "architecture", "size", "sha256")) or not isinstance(row["fileName"], str) or not re.fullmatch(r"[A-Za-z0-9][A-Za-z0-9_.+%:~@-]*\.deb", row["fileName"]):
                raise ProbeError("archive_receipt_invalid", "Measured archive bytes or package identity differ from approval")
            seen.add(row["name"])
        if seen != set(expected):
            raise ProbeError("archive_receipt_invalid", "Measured archive receipt is incomplete")
    if receipt["state"] == "succeeded" and (not receipt["verified"] or receipt["phase"] != "complete" or receipt["downloadExitCode"] != 0 or receipt["installExitCode"] != 0 or receipt["errorCode"] is not None):
        raise ProbeError("archive_receipt_invalid", "Archive success requires verified bytes and both successful APT phases")
    return receipt


def cache_action(action, journal, native, password):
    argv = sudo_argv(worker_argv(journal, [], action, native), password is not None, native)
    result = json.loads(bounded_client(argv, password=password, seconds=15))
    if action == "read-receipt":
        return validate_archive_receipt(result, journal)
    if result != {"operationId": journal["operationId"], "planDigest": journal["approvedPlan"]["planDigest"], "cacheRemoved": True}:
        raise ProbeError("cache_cleanup_unconfirmed", "Owned runtime cache removal was not confirmed")
    return result


def collect_unit(journal, native, password, path=None):
    observed_clean = False
    try:
        values = reconcile_unit(journal, native)
        observed_clean = bool(journal["unit"].get("processesGone"))
        if not observed_clean:
            raise ProbeError("cleanup_unconfirmed", "Owned package unit still has active processes")
        journal["cleanupConfirmed"] = False
        if journal.get("archiveReceipt") is None and not journal["unit"].get("cacheRemoved"):
            journal["archiveReceipt"] = cache_action("read-receipt", journal, native, password)
            if path is not None:
                write_journal(path, journal)  # Measured bytes survive cache collection.
        if values is not None:
            privileged_control("stop", journal["unit"], password, native)
            if inspect_unit(journal["unit"], native) is not None:
                privileged_control("reset-failed", journal["unit"], password, native)
        observed_clean = False
        reconcile_unit(journal, native)
        observed_clean = bool(journal["unit"].get("processesGone"))
        journal["cleanupConfirmed"] = False
        if not journal["unit"].get("cacheRemoved"):
            cache_action("cleanup", journal, native, password)
            journal["unit"]["cacheRemoved"] = True
            if path is not None:
                write_journal(path, journal)
    finally:
        unit = journal.get("unit", {})
        journal["cleanupConfirmed"] = bool(observed_clean and cleanup_complete(journal))


def unit_native(native):
    return Native() if type(native) is Native else native


def install_transaction(entries, password, native, path, journal):
    if journal.get("attemptCount", 0) >= MAX_ATTEMPTS:
        raise ProbeError("attempt_limit", "This operation reached its three-attempt evidence limit; obtain a fresh review")
    unit = unit_spec(journal, entries, native)
    if inspect_unit(unit, native) is not None:
        raise ProbeError("unit_ownership_mismatch", "The new operation unit name already exists")
    if os.path.lexists("/run/" + unit["runtimeDirectory"]):
        raise ProbeError("cache_ownership_mismatch", "The new operation runtime directory already exists; reconcile it first")
    if journal.get("archiveReceipt"):
        journal.setdefault("archiveReceipts", []).append(json.loads(canonical(journal["archiveReceipt"])))
    journal["archiveReceipt"] = None
    journal["attemptCount"] = journal.get("attemptCount", 0) + 1
    journal["unit"] = unit
    argv = fixed_install_argv(entries, password is not None, native, journal)
    previous = {}
    cancelled = False
    child = None
    output_bytes = 0
    deadline = time.monotonic() + MAX_INSTALL_SECONDS + 30

    def stop(_number, _frame):
        nonlocal cancelled
        cancelled = True

    journal.update(state="launching", cleanupConfirmed=False, effectsApplied=False, launchPending=True,
                   controller={"pid": os.getpid(), "startTicks": process_start(os.getpid())})
    write_journal(path, journal)  # A crash in the launch gap remains explicitly unconfirmed.
    try:
        for name in ("SIGTERM", "SIGHUP"):
            number = getattr(signal, name, None)
            if number is not None:
                previous[number] = signal.getsignal(number)
                signal.signal(number, stop)
        parent_pid = os.getpid()
        child = subprocess.Popen(argv, stdin=subprocess.PIPE if password is not None else subprocess.DEVNULL,
                                 stdout=subprocess.PIPE, stderr=subprocess.DEVNULL, start_new_session=True,
                                 env={"PATH": "/usr/bin:/bin", "LANG": "C", "LC_ALL": "C", "HOME": journal["identity"]["home"]},
                                 preexec_fn=lambda: probe_parent_death(parent_pid))
        journal.update(state="installing", effectsApplied=True, launchPending=False,
                       process={"pid": child.pid, "startTicks": process_start(child.pid)})
        write_journal(path, journal)
        if password is not None:
            child.stdin.write((password + "\n").encode())
            child.stdin.close()
        stop_requested = False
        next_inspect = 0
        with selectors.DefaultSelector() as selector:
            selector.register(child.stdout, selectors.EVENT_READ)
            while selector.get_map():
                cancelled = cancelled or os.path.lexists(path.with_suffix(".cancel"))
                if time.monotonic() >= next_inspect:
                    observed_native = unit_native(native)
                    try:
                        values = reconcile_unit(journal, observed_native)
                    except ProbeError as error:
                        # Before sudo has authenticated, absence is not yet
                        # evidence that the submitted unit existed and ended.
                        if error.code not in ("cleanup_unconfirmed", "unit_pending") or unit.get("observed"):
                            raise
                        values = None
                    write_journal(path, journal)
                    if values is not None and cancelled and not stop_requested:
                        privileged_control("stop", unit, password, observed_native)
                        stop_requested = True
                    if unit.get("observed") and unit.get("processesGone") and not cleanup_complete(journal):
                        collect_unit(journal, observed_native, password, path)
                        write_journal(path, journal)
                    next_inspect = time.monotonic() + 0.5
                if time.monotonic() >= deadline:
                    raise ProbeError("cleanup_unconfirmed", "Transient-unit inspection deadline elapsed; use status/cancel to reconcile the owned unit")
                for key, _ in selector.select(0.1):
                    data = os.read(key.fd, 4096)
                    if not data:
                        selector.unregister(key.fileobj)
                    output_bytes += len(data)
                    if output_bytes > 256 << 10:
                        cancelled = True
        child.wait(timeout=max(0.01, deadline - time.monotonic()))
        journal.update(state="verifying", exitCode=child.returncode, process=None, controller=None)
        observed_native = unit_native(native)
        if not unit.get("observed") and child.returncode > 0 and inspect_unit(unit, observed_native) is None and cgroup_state(unit) == "absent" and not path_present("/run/" + unit["runtimeDirectory"]):
            # RemainAfterExit retains success metadata; CollectMode=inactive
            # retains failure metadata. A normal failed client plus no unit or
            # cgroup therefore indicates that no transient job was admitted.
            journal.update(cleanupConfirmed=True, effectsApplied=False)
            unit.update(collected=True, cacheRemoved=True, processesGone=True, cgroupState="absent")
            write_journal(path, journal)
            guidance = ("Existing non-interactive administrator authorization may be unavailable. Enter "
                        if password is None else "Check ")
            raise ProbeError("package_launch_failed", "Normal sudo/systemd did not admit the reviewed package unit. "
                             + guidance + "this device's separate administrator password in PAIR and retry the original MPI package operation, "
                             "or contact its administrator. Other systemd admission failures are also possible.")
        collect_unit(journal, observed_native, password, path)
        write_journal(path, journal)
        if not journal["cleanupConfirmed"]:
            raise ProbeError("cleanup_unconfirmed", "The owned package cgroup is not empty")
        if cancelled:
            raise ProbeError("operation_cancelled", "Owned transient unit stopped and its cgroup is empty")
        job = unit.get("jobResult", {})
        report = validate_archive_receipt(journal.get("archiveReceipt"), journal)
        if child.returncode or job.get("result") != "success" or job.get("execMainCode") != 1 or job.get("execMainStatus") != 0 or report["state"] != "succeeded":
            raise ProbeError("package_install_failed", "The owned transient unit recorded an unsuccessful APT result")
    finally:
        for number, handler in previous.items():
            signal.signal(number, handler)
        if child is not None:
            if child.stdout:
                child.stdout.close()
            if child.stdin and not child.stdin.closed:
                child.stdin.close()


def provision(request, native=None, cache_loader=load_cache, installer=install_transaction):
    validate_request(request)
    native = native or Native()
    binding = identity(request, native)
    if type(request.get("expectedUID")) is not int or request["expectedUID"] != binding["uid"] or request.get("expectedHome") != binding["home"]:
        raise ProbeError("approval_mismatch", "The reviewed account or home changed")
    validate_approval(request.get("approvedPlan"), binding)
    validate_python_preparation_mode(request, request["approvedPlan"])
    if (request["approvedPlan"]["recipeId"] == PYTHON_RECIPE) != request.get("runtimePreparation", False):
        raise ProbeError("approval_mismatch", "The selected package purpose differs from the reviewed recipe")
    validate_dependency_consent(request["approvedPlan"], request.get("dependencyUpgradesApproved", False))
    path = operation_path(binding, request["operationId"], create=True)
    with operation_lock(path, binding):
        # A terminated helper can release its file lock before its privileged
        # child is gone. An unresolved journal keeps this host's lane held.
        journals = list(path.parent.glob("*.json"))
        if len(journals) > 128:
            raise ProbeError("state_limit", "MPI operation history exceeds the bounded review limit")
        for other in journals:
            if other != path:
                saved = read_journal(other, native, binding)
                if saved and not saved.get("cleanupConfirmed", False):
                    raise ProbeError("cleanup_unconfirmed", "Another owned package transaction still requires cleanup confirmation")
        return _provision_locked(request, native, cache_loader, installer)


def _provision_locked(request, native, cache_loader, installer):
    validate_request(request)
    native = native or Native()
    binding = identity(request, native)
    if request.get("expectedUID") != binding["uid"] or request.get("expectedHome") != binding["home"]:
        raise ProbeError("approval_mismatch", "The reviewed account or home changed")
    plan = request.get("approvedPlan")
    validate_approval(plan, binding)
    validate_python_preparation_mode(request, plan)
    python = plan["recipeId"] == PYTHON_RECIPE
    if python != request.get("runtimePreparation", False):
        raise ProbeError("approval_mismatch", "The selected package purpose differs from the reviewed recipe")
    validate_dependency_consent(plan, request.get("dependencyUpgradesApproved", False))
    path = operation_path(binding, request["operationId"], create=True)
    journal = read_journal(path, native, binding)
    if journal:
        if journal.get("closed"):
            raise ProbeError("operation_cancelled", "This operation was durably closed before launch; a fresh review is required")
        if journal.get("approvedPlan") != plan:
            raise ProbeError("approval_mismatch", "This operation identifier belongs to another approved plan")
        if not journal.get("cleanupConfirmed", False):
            reconcile_unit(journal, native)
            write_journal(path, journal)
            if not journal["unit"].get("processesGone"):
                raise ProbeError("cleanup_unconfirmed", "The previous owned package unit still has processes; use status/cancel")
        if journal.get("unit") and not cleanup_complete(journal):
            collect_unit(journal, native, request.get("elevationPassword"), path)
            write_journal(path, journal)
        if python and journal.get("unit") and not cleanup_complete(journal):
            raise ProbeError("cleanup_unconfirmed", "Managed Python cannot be rechecked while the administrator unit remains unresolved")
    else:
        fresh_request = {"action": "review", "nodeId": request["nodeId"], "principal": request["principal"]}
        if plan["recipeId"] == UPGRADE_RECIPE:
            fresh_request["dependencyUpgradeReview"] = True
        elif python:
            fresh_request["runtimePreparation"] = True
            if request.get("firstInstall"):
                fresh_request["firstInstall"] = True
        fresh_review = review(fresh_request, native, cache_loader)
        if fresh_review.get("plan") != plan or fresh_review["state"] != "reviewed":
            raise ProbeError("approval_mismatch", "The initial reviewed package transaction changed before any installation")
        journal = {"owner": JOURNAL_OWNER, "operationId": request["operationId"], "identity": binding,
                   "approvedPlan": plan, "state": "reviewed", "effectsApplied": False, "cleanupConfirmed": True}
        if plan["recipeId"] == UPGRADE_RECIPE:
            journal["dependencyUpgradesApproved"] = request.get("dependencyUpgradesApproved", False)
        write_journal(path, journal)
    if python:
        python_bound_observation(native, binding, plan["runtimeBinding"])
    if os.path.lexists(path.with_suffix(".cancel")):
        raise ProbeError("operation_cancelled", "This package operation was cancelled; obtain a new review")
    cache = cache_loader()
    try:
        pending = pending_transaction(cache, plan)
    finally:
        cache.close()
    try:
        if pending:
            installer(pending, request.get("elevationPassword"), native, path, journal)
        current = cache_loader()
        try:
            if pending_transaction(current, plan):
                raise ProbeError("package_install_incomplete", "Approved packages are still missing after installation")
        finally:
            current.close()
        fresh = Native() if type(native) is Native else native
        family = installed_family(fresh, python)
        if not matching_family(family, python):
            raise ProbeError("package_install_incomplete", "Matching installed MPI family was not confirmed after the transaction")
        archive = validate_archive_receipt(journal.get("archiveReceipt") or journal.get("receipt", {}).get("archiveReceipt"), journal)
        if archive["state"] != "succeeded":
            raise ProbeError("archive_receipt_invalid", "Installation success requires a successful measured archive receipt")
        if journal.get("unit") and not (journal["unit"].get("collected") and journal["unit"].get("cacheRemoved")):
            journal["cleanupConfirmed"] = False
            raise ProbeError("cleanup_unconfirmed", "The exact unit metadata and private archive cache still require reconciliation")
        runtime_receipt = {}
        if python:
            runtime_receipt = python_runtime_postcheck(fresh, binding, plan, journal)
        journal.update(state="succeeded", cleanupConfirmed=True,
                        receipt={"recipeId": plan["recipeId"], "planDigest": plan["planDigest"], "installedFamily": family,
                                "packages": plan["packages"], "runtimeValidated": False, "archiveReceipt": archive, **runtime_receipt})
        journal.pop("error", None)
        write_journal(path, journal)
    except Exception as error:
        if not isinstance(error, ProbeError):
            error = ProbeError("package_operation_failed", "The package operation failed; no command output or access material was persisted")
        if journal.get("unit"):
            journal["cleanupConfirmed"] = cleanup_complete(journal)
        journal["state"] = "cleanup_unconfirmed" if not journal.get("cleanupConfirmed") else "cancelled" if error.code == "operation_cancelled" else "failed"
        journal["error"] = {"code": error.code, "phase": "provision", "message": str(error)}
        write_journal(path, journal)
    return {"schemaVersion": 1, "action": "provision", "operationId": request["operationId"],
            "planDigest": plan["planDigest"],
            "state": journal["state"], "effectsApplied": bool(pending) and journal["effectsApplied"],
            "cleanupConfirmed": journal["cleanupConfirmed"], "receipt": journal.get("receipt"),
            "unit": journal.get("unit"),
            "unitMetadataRetained": bool(journal.get("unit") and not journal["unit"].get("collected")),
            "archiveReceipt": journal.get("archiveReceipt"),
            "errors": [journal["error"]] if journal.get("error") else []}


def validate_pre_unit_journal(journal, request, binding):
    if journal.get("approvedPlan", {}).get("planDigest") != request["expectedPlanDigest"]:
        raise ProbeError("approval_mismatch", "Pre-unit journal belongs to a different original review")
    validate_approval(journal.get("approvedPlan"), binding)
    validate_python_preparation_mode(request, journal["approvedPlan"])
    if (journal["approvedPlan"]["recipeId"] == PYTHON_RECIPE) != request.get("runtimePreparation", False):
        raise ProbeError("approval_mismatch", "Pre-unit operation belongs to another package purpose")
    validate_journal_consent(journal)
    initial_fields = {"owner", "operationId", "identity", "approvedPlan", "state", "effectsApplied", "cleanupConfirmed"}
    if journal["approvedPlan"]["recipeId"] == UPGRADE_RECIPE:
        initial_fields.add("dependencyUpgradesApproved")
    if set(journal) != initial_fields or journal.get("state") != "reviewed" or journal.get("effectsApplied") is not False or journal.get("cleanupConfirmed") is not True:
        raise ProbeError("cleanup_unconfirmed", "A unit-less journal contains inconsistent launch or effect evidence")


def recovery(request, native=None, cache_loader=load_cache):
    validate_request(request)
    native = native or Native()
    binding = identity(request, native)
    path = operation_path(binding, request["operationId"], create=request["action"] in ("cancel", "reconcile"))
    journal = read_journal(path, native, binding)
    if journal and journal.get("approvedPlan") and ((journal["approvedPlan"].get("recipeId") == PYTHON_RECIPE) != request.get("runtimePreparation", False)):
        raise ProbeError("approval_mismatch", "Recovery purpose differs from the retained package operation")
    if journal and journal.get("approvedPlan"):
        validate_python_preparation_mode(request, journal["approvedPlan"])
    if not journal or (not journal.get("closed") and "unit" not in journal):
        if journal:
            validate_pre_unit_journal(journal, request, binding)
        if request["action"] == "status":
            raise ProbeError("cleanup_unconfirmed", "No unit was recorded; reconcile must serialize and close any delayed launch")
        with operation_lock(path, binding):
            journal = read_journal(path, native, binding)
            if journal is None or (not journal.get("closed") and "unit" not in journal):
                if journal:
                    validate_pre_unit_journal(journal, request, binding)
                result = prove_absence(request, native)
                closed = {**(journal or {}), "owner": JOURNAL_OWNER, "operationId": request["operationId"], "identity": binding,
                          "expectedPlanDigest": request["expectedPlanDigest"], "closed": True, "state": "cancelled",
                          "effectsApplied": False, "cleanupConfirmed": True, "absenceProof": result["absenceProof"]}
                if request.get("runtimePreparation"):
                    closed["runtimePreparation"] = True
                if request.get("firstInstall"):
                    closed["firstInstall"] = True
                write_journal(path, closed)
                try:
                    fd = os.open(path.with_suffix(".cancel"), os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
                    os.close(fd)
                except FileExistsError:
                    pass
                result.update(state="cancelled", closed=True, freshReviewRequired=True, cleanupConfirmed=True)
                return result
    if journal.get("closed"):
        if journal.get("runtimePreparation", False) != request.get("runtimePreparation", False) or journal.get("firstInstall", False) != request.get("firstInstall", False):
            raise ProbeError("approval_mismatch", "Closed operation belongs to another package purpose")
        if journal.get("expectedPlanDigest") != request["expectedPlanDigest"]:
            raise ProbeError("approval_mismatch", "Closed operation belongs to a different original review")
        return {"schemaVersion": 1, "action": request["action"], "operationId": request["operationId"],
                "planDigest": request["expectedPlanDigest"], "state": "cancelled", "closed": True,
                "freshReviewRequired": True, "effectsApplied": False, "cleanupConfirmed": True, "errors": []}
    if journal.get("approvedPlan", {}).get("planDigest") != request["expectedPlanDigest"]:
        raise ProbeError("approval_mismatch", "Recovery plan digest differs from the caller's original review")
    validate_approval(journal.get("approvedPlan"), binding)
    validate_python_preparation_mode(request, journal["approvedPlan"])
    python = journal["approvedPlan"]["recipeId"] == PYTHON_RECIPE
    if python != request.get("runtimePreparation", False):
        raise ProbeError("approval_mismatch", "Recovery purpose differs from the retained package recipe")
    validate_journal_consent(journal)
    was_complete = journal.get("cleanupConfirmed") and journal["state"] in ("succeeded", "failed", "cancelled")
    values = reconcile_unit(journal, native)
    if request["action"] == "cancel":
        cancel_path = path.with_suffix(".cancel")
        if not was_complete and not os.path.lexists(cancel_path):
            fd = os.open(cancel_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL | getattr(os, "O_NOFOLLOW", 0), 0o600)
            os.close(fd)
        if values is not None:
            privileged_control("stop", journal["unit"], request.get("elevationPassword"), native)
            reconcile_unit(journal, native)
        if journal["unit"].get("processesGone"):
            collect_unit(journal, native, request.get("elevationPassword"), path)
    elif request["action"] == "reconcile" and journal["unit"].get("processesGone"):
        # Cleanup-only: do not stop a running unit or create a cancel marker.
        collect_unit(journal, native, request.get("elevationPassword"), path)
    elif request["action"] == "status" and journal["unit"].get("processesGone") and journal.get("archiveReceipt") is None:
        # This is a read through normal sudo, never a mutation of the unit/cache.
        journal["archiveReceipt"] = cache_action("read-receipt", journal, native, request.get("elevationPassword"))
    journal["cleanupConfirmed"] = cleanup_complete(journal)
    if journal["cleanupConfirmed"]:
        if (request["action"] == "cancel" and not was_complete) or os.path.lexists(path.with_suffix(".cancel")):
            journal["state"] = "cancelled"
            journal["error"] = {"code": "operation_cancelled", "phase": "recovery", "message": "Owned transient unit stopped; its cgroup is empty"}
        elif journal.get("unit", {}).get("jobResult", {}).get("result") == "success":
            archive = validate_archive_receipt(journal.get("archiveReceipt"), journal)
            cache = cache_loader()
            try:
                missing = pending_transaction(cache, journal["approvedPlan"])
            finally:
                cache.close()
            family = installed_family(native, python)
            runtime_receipt, runtime_error = {}, None
            if python and not missing and matching_family(family, True) and archive["state"] == "succeeded":
                try:
                    postcheck_native = Native() if type(native) is Native else native
                    runtime_receipt = python_runtime_postcheck(postcheck_native, binding, journal["approvedPlan"], journal)
                except ProbeError as error:
                    runtime_error = {"code": error.code, "phase": "runtime-postcheck", "message": str(error)}
            if not missing and matching_family(family, python) and archive["state"] == "succeeded" and runtime_error is None:
                journal["state"] = "succeeded"
                journal["receipt"] = {"recipeId": journal["approvedPlan"]["recipeId"], "planDigest": journal["approvedPlan"]["planDigest"],
                                      "installedFamily": family, "packages": journal["approvedPlan"]["packages"],
                                      "runtimeValidated": False, "archiveReceipt": archive, **runtime_receipt}
                journal.pop("error", None)
            else:
                journal["state"] = "failed"
                journal["error"] = runtime_error or {"code": "package_install_incomplete", "phase": "recovery", "message": "Unit cleanup is confirmed, but approved package state is incomplete"}
        else:
            journal["state"] = "failed"
            journal["error"] = {"code": "package_install_failed", "phase": "recovery", "message": "Owned unit ended without a successful APT result; partial package state is preserved"}
    else:
        journal["state"] = "cleanup_unconfirmed" if journal["unit"].get("processesGone") else "installing"
    if request["action"] == "status" and not (journal["unit"].get("collected") and journal["unit"].get("cacheRemoved")):
        journal["cleanupConfirmed"] = False
        if journal["state"] != "installing":
            journal["state"] = "cleanup_unconfirmed"
    controller = journal.get("controller")
    controller_start = process_start(controller["pid"]) if controller else None
    controller_alive = bool(controller_start and controller_start == controller.get("startTicks")) if controller else False
    if not controller_alive and request["action"] != "status":
        write_journal(path, journal)
    return {"schemaVersion": 1, "action": request["action"], "operationId": request["operationId"],
            "planDigest": request["expectedPlanDigest"],
            "state": journal["state"], "effectsApplied": journal["effectsApplied"],
            "cleanupConfirmed": journal["cleanupConfirmed"], "controllerAlive": controller_alive,
            "unit": journal["unit"], "unitMetadataRetained": not journal["unit"].get("collected", False),
            "archiveReceipt": journal.get("archiveReceipt"),
            "cancelRequested": os.path.lexists(path.with_suffix(".cancel")), "receipt": journal.get("receipt"),
            "errors": [journal["error"]] if journal.get("error") else []}


def prove_absence(request, native):
    name = "pair-nccl-mpi-" + request["operationId"] + ".service"
    ctl = native.resolve("systemctl")
    code, output = native.run([ctl, "--system", "--no-pager", "show", "--property=LoadState", "--", name])
    if code not in (0, 1, 4) or output.strip() != "LoadState=not-found":
        raise ProbeError("cleanup_unconfirmed", "No journal exists, but exact unit absence was not proved")
    code, jobs = native.run([ctl, "--system", "--no-pager", "--no-legend", "--plain", "list-jobs", name])
    if code or jobs.strip():
        raise ProbeError("cleanup_unconfirmed", "An exact unit job is present or its absence is unknown")
    unit = {"name": name, "controlGroup": "/system.slice/" + name}
    if cgroup_state(unit) != "absent" or path_present("/run/" + name[:-8]):
        raise ProbeError("cleanup_unconfirmed", "The exact runtime cache or cgroup is still present")
    return {"schemaVersion": 1, "action": request["action"], "operationId": request["operationId"],
            "planDigest": request["expectedPlanDigest"], "state": "not_started",
            "effectsApplied": False, "cleanupConfirmed": False, "operationAbsent": True,
            "absenceProof": {"journal": True, "unit": True, "jobs": True, "cgroup": True, "runtimeCache": True}, "errors": []}


def dispatch(request):
    validate_request(request)
    if request["action"] == "review":
        return review(request)
    if request["action"] == "provision":
        return provision(request)
    return recovery(request)


def main():
    request = None
    try:
        raw = sys.stdin.buffer.read((128 << 10) + 1)
        if len(raw) > 128 << 10:
            raise ProbeError("invalid_request", "MPI package request exceeds 128 KiB")
        def unique(items):
            value = {}
            for key, item in items:
                if key in value:
                    raise ProbeError("invalid_request", "Duplicate request field")
                value[key] = item
            return value
        request = json.loads(raw, object_pairs_hook=unique)
        result = dispatch(request)
    except ProbeError as error:
        result = {"schemaVersion": 1, "state": "cleanup_unconfirmed" if error.code == "cleanup_unconfirmed" else "blocked", "effectsApplied": False,
                  "errors": [{"code": error.code, "phase": "request", "message": str(error)}]}
    except Exception:
        result = {"schemaVersion": 1, "state": "blocked", "effectsApplied": False,
                  "errors": [{"code": "review_failed", "phase": "review", "message": "MPI package review could not complete"}]}
    if isinstance(request, dict) and request.get("action") in ("provision", "cancel", "status", "reconcile") and "action" not in result:
        # An error before recovery could read the journal cannot establish that
        # a previous attempt made no changes.
        result["effectsUnknown"] = True
        result["action"] = request["action"]
        if isinstance(request.get("operationId"), str) and re.fullmatch(r"[a-f0-9]{32}", request["operationId"]):
            result["operationId"] = request["operationId"]
        expected = request.get("expectedPlanDigest")
        if request["action"] == "provision" and isinstance(request.get("approvedPlan"), dict):
            expected = request["approvedPlan"].get("planDigest")
        if isinstance(expected, str) and re.fullmatch(r"[a-f0-9]{64}", expected):
            result["planDigest"] = expected
    output = canonical(result)
    if len(output) > 96 << 10:
        output = b'{"schemaVersion":1,"state":"blocked","effectsApplied":false,"errors":[{"code":"output_limit","message":"Package review exceeds response limit"}]}'
    sys.stdout.write(output.decode() + "\n")


if __name__ == "__main__":
    main()

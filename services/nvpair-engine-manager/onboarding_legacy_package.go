// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"
)

const onboardingLegacyPackageRecoveryBudget = onboardingUpgradePhaseBudget + 90*time.Second

const (
	onboardingLegacyPackageAbsent        = "absent"
	onboardingLegacyPackageNotApplicable = "not-applicable"
	onboardingLegacyPackageAdmitted      = "admitted"
	onboardingLegacyPackageNoRollback    = "retained-no-exact-rollback"
	onboardingLegacyPackageNoElevation   = "retained-no-administrator-access"
	onboardingLegacyPackageProofOwner    = "nvidia-pair-legacy-package-retirement-v1"
	onboardingLegacyPackageName          = "nvpair"
	onboardingLegacyPackageApp           = "/opt/PAIR/nvpair"
	onboardingLegacyPackageLauncher      = "/usr/bin/nvpair"
	onboardingLegacyPackageDesktopEntry  = "/usr/share/applications/nvpair.desktop"
)

var onboardingDebianToken = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+:~_-]{0,127}$`)

type onboardingLegacyFileIdentity struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
	Device uint64 `json:"device"`
	Inode  uint64 `json:"inode"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Mode   uint32 `json:"mode"`
	Links  uint64 `json:"links"`
}

type onboardingLegacyPackageProcess struct {
	PID        int    `json:"pid"`
	UID        int    `json:"uid"`
	StartTicks string `json:"startTicks"`
	Executable string `json:"executable"`
	Device     uint64 `json:"device"`
	Inode      uint64 `json:"inode"`
}

// This receipt remains backend-only. It binds the exact installed Debian
// owner and rollback archive inspected during review; no package path or
// process list is accepted from the renderer.
type onboardingLegacyPackageReceipt struct {
	Package             string                           `json:"package"`
	Status              string                           `json:"status"`
	Version             string                           `json:"version"`
	Architecture        string                           `json:"architecture"`
	PackageRecordSHA256 string                           `json:"packageRecordSha256"`
	PackageFilesSHA256  string                           `json:"packageFilesSha256"`
	PackageInfoSHA256   string                           `json:"packageInfoSha256"`
	App                 onboardingLegacyFileIdentity     `json:"app"`
	Launcher            onboardingLegacyFileIdentity     `json:"launcher"`
	DesktopEntry        onboardingLegacyFileIdentity     `json:"desktopEntry"`
	Processes           []onboardingLegacyPackageProcess `json:"processes"`
	Rollback            onboardingLegacyFileIdentity     `json:"rollback"`
}

func onboardingLegacyPackageSummaryFields(d *onboardingLegacyPackageReceipt) (string, string, string, string, string, string, string, int64) {
	if d == nil {
		return "", "", "", "", "", "", "", 0
	}
	return d.Package, d.Status, d.Version, d.Architecture, d.App.SHA256, d.Launcher.SHA256, d.Rollback.SHA256, d.Rollback.Bytes
}

func validateOnboardingLegacyFile(f onboardingLegacyFileIdentity, expected string, owners map[int]bool, limit int64, executable bool) error {
	if f.Path != expected || !path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || !onboardingSHA.MatchString(f.SHA256) || f.Bytes <= 0 || f.Bytes > limit || f.Device == 0 || f.Inode == 0 || !owners[f.UID] || f.GID < 0 || f.Links != 1 || f.Mode > 07777 || f.Mode&0022 != 0 {
		return errors.New("legacy package file identity is incomplete, shared or writable")
	}
	if f.UID == 0 && f.GID != 0 {
		return errors.New("legacy package root-owned file has a foreign group")
	}
	if executable && f.Mode&0111 == 0 {
		return errors.New("legacy package executable identity is not executable")
	}
	return nil
}

func validateOnboardingLegacyPackage(status string, d *onboardingLegacyPackageReceipt, info onboardingPlatformInfo) error {
	if status == "" { // Backward-compatible retained journals predate this optional review.
		return nil
	}
	if status != onboardingLegacyPackageAbsent && status != onboardingLegacyPackageNotApplicable && status != onboardingLegacyPackageAdmitted && status != onboardingLegacyPackageNoRollback && status != onboardingLegacyPackageNoElevation {
		return errors.New("legacy package disposition is invalid")
	}
	if status != onboardingLegacyPackageAdmitted {
		if d != nil {
			return errors.New("unadmitted legacy package retained an effect receipt")
		}
		return nil
	}
	_, nativeArchitecture, platformErr := onboardingPlatform(info.OS, info.Arch)
	if d == nil || d.Package != onboardingLegacyPackageName || d.Status != "install ok installed" || !onboardingDebianToken.MatchString(d.Version) || platformErr != nil || d.Architecture != nativeArchitecture || !onboardingSHA.MatchString(d.PackageRecordSHA256) || !onboardingSHA.MatchString(d.PackageFilesSHA256) || !onboardingSHA.MatchString(d.PackageInfoSHA256) || len(d.Processes) != 0 {
		return errors.New("legacy package receipt is incomplete or has a running owner")
	}
	root := map[int]bool{0: true}
	rollbackOwners := map[int]bool{0: true, info.UID: true}
	if err := validateOnboardingLegacyFile(d.App, onboardingLegacyPackageApp, root, 1<<30, true); err != nil {
		return err
	}
	if err := validateOnboardingLegacyFile(d.Launcher, onboardingLegacyPackageLauncher, root, 8192, true); err != nil {
		return err
	}
	if err := validateOnboardingLegacyFile(d.DesktopEntry, onboardingLegacyPackageDesktopEntry, root, 1<<20, false); err != nil {
		return err
	}
	if err := validateOnboardingLegacyFile(d.Rollback, d.Rollback.Path, rollbackOwners, 1<<30, false); err != nil {
		return err
	}
	cache := "/var/cache/apt/archives/"
	downloads := path.Join(info.Home, "Downloads") + "/"
	if !strings.HasPrefix(d.Rollback.Path, cache) && !strings.HasPrefix(d.Rollback.Path, downloads) || !strings.HasSuffix(d.Rollback.Path, ".deb") {
		return errors.New("legacy rollback archive is outside its bounded review locations")
	}
	return nil
}

// dpkg-query uses exit 1 with no stdout for an unknown package. That is the only
// nonzero result admitted as absent. A retained config-files record is also
// absent, but only when its exact package/version/architecture fields parse.
// Inspection and the privileged transaction share this parser so neither can
// turn an unknown database result into successful retirement.
const onboardingLegacyPackageRecordParserScript = `
def legacy_package_record_state(code,record,expected_version=None,expected_architecture=None):
 if code==1 and record==b'': return ('absent','','','','')
 if code!=0: raise RuntimeError('legacy package query failed with an unknown result')
 try: text=record.decode('utf-8')
 except UnicodeDecodeError: raise RuntimeError('legacy package record is not UTF-8')
 if not text.endswith('\n') or text.count('\n')!=1: raise RuntimeError('legacy package record framing is malformed')
 fields=text[:-1].split('\t')
 if len(fields)!=4: raise RuntimeError('legacy package identity is ambiguous')
 package_field,status,version,architecture=fields
 if package_field not in ('nvpair','nvpair:'+architecture): raise RuntimeError('legacy package identity is ambiguous')
 package='nvpair'
 if not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9.+:~_-]{0,127}',version) or architecture not in ('arm64','amd64'): raise RuntimeError('legacy package version or architecture is unsupported')
 if (expected_version is not None and version!=expected_version) or (expected_architecture is not None and architecture!=expected_architecture): raise RuntimeError('legacy package version or architecture changed')
 if status=='install ok installed': state='installed'
 elif status=='deinstall ok config-files': state='absent'
 else: raise RuntimeError('legacy package status is neither installed nor exact config-files absence')
 return (state,package,status,version,architecture)
`

// The reviewed JSON is a nonsecret bounded argv value. stdin contains only the
// sudo password line, so both prompting sudo and cached/NOPASSWD sudo produce
// the same child request instead of conditionally leaking a password prefix
// into Python's JSON decoder.
const onboardingLegacyPackageArgumentScript = `
def legacy_package_request_argument():
 if len(sys.argv)!=2 or len(sys.argv[1])>131072: raise RuntimeError('legacy package argument framing is invalid')
 try: raw=base64.b64decode(sys.argv[1],validate=True)
 except (ValueError,binascii.Error): raise RuntimeError('legacy package argument encoding is invalid')
 if len(raw)>65536: raise RuntimeError('legacy package request exceeds its bound')
 return raw
`

// Read-only Debian inspection is deliberately optional. A complete installed
// package without one unique exact rollback archive is reported but not
// admitted; the existing bound-CLI-only upgrade path remains unchanged.
const onboardingLegacyPackageInspectScript = onboardingLegacyPackageRecordParserScript + `
LEGACY_APP='/opt/PAIR/nvpair'; LEGACY_LAUNCHER='/usr/bin/nvpair'; LEGACY_DESKTOP='/usr/share/applications/nvpair.desktop'
LEGACY_ENV={'PATH':'/usr/sbin:/usr/bin:/sbin:/bin','LC_ALL':'C','LANG':'C'}
def legacy_tool(filename):
 observed=os.lstat(filename)
 if not stat.S_ISREG(observed.st_mode) or observed.st_uid!=0 or observed.st_gid!=0 or observed.st_mode&0o022 or not observed.st_mode&0o111 or os.path.realpath(filename)!=filename: raise RuntimeError('legacy package tool ownership is unsafe')
def legacy_run(argv,limit=1048576,timeout=30):
 p=subprocess.run(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=timeout,env=LEGACY_ENV)
 if len(p.stdout)>limit: raise RuntimeError('legacy package inspection output exceeds its bound')
 return p.returncode,p.stdout
def legacy_file(filename,limit,owners):
 if len(filename)>4096 or any(unicodedata.category(c)=='Cc' for c in filename) or not os.path.isabs(filename) or os.path.normpath(filename)!=filename or os.path.realpath(filename)!=filename: raise RuntimeError('legacy package file is redirected or noncanonical')
 before=os.lstat(filename)
 if not stat.S_ISREG(before.st_mode) or before.st_uid not in owners or before.st_mode&0o022 or before.st_nlink!=1 or before.st_size<=0 or before.st_size>limit: raise RuntimeError('legacy package file is foreign, shared, writable or oversized')
 fd=os.open(filename,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC)
 try:
  opened=os.fstat(fd)
  if (before.st_dev,before.st_ino)!=(opened.st_dev,opened.st_ino): raise RuntimeError('legacy package file changed while opened')
  digest=hashlib.sha256(); body=bytearray()
  while True:
   chunk=os.read(fd,1048576)
   if not chunk: break
   digest.update(chunk)
   if opened.st_size<=8192: body.extend(chunk)
  after=os.fstat(fd)
  if (opened.st_dev,opened.st_ino,opened.st_size,opened.st_nlink)!=(after.st_dev,after.st_ino,after.st_size,after.st_nlink): raise RuntimeError('legacy package file changed while hashed')
 finally: os.close(fd)
 return dict(path=filename,sha256=digest.hexdigest(),bytes=opened.st_size,device=opened.st_dev,inode=opened.st_ino,uid=opened.st_uid,gid=opened.st_gid,mode=stat.S_IMODE(opened.st_mode),links=opened.st_nlink),bytes(body)
def legacy_info_hash():
 digest=hashlib.sha256()
 for suffix in ('list','md5sums','preinst','postinst','prerm','postrm'):
  filename='/var/lib/dpkg/info/nvpair.'+suffix; digest.update(suffix.encode()+b'\0')
  if not os.path.lexists(filename): digest.update(b'absent\0'); continue
  identity,body=legacy_file(filename,1048576,{0}); digest.update(identity['sha256'].encode()+b'\0')
 return digest.hexdigest()
def legacy_processes():
 found=[]
 for name in os.listdir('/proc'):
  if not name.isdigit(): continue
  try:
   target=os.readlink('/proc/'+name+'/exe')
   if target.endswith(' (deleted)') and target[:-10].startswith('/opt/PAIR/'): raise RuntimeError('legacy package has a deleted running executable')
   if target!='/opt/PAIR' and not target.startswith('/opt/PAIR/'): continue
   observed=os.stat('/proc/'+name+'/exe'); owner=os.stat('/proc/'+name).st_uid
   with open('/proc/'+name+'/stat') as stream: fields=stream.read(8193)
   ticks=fields[fields.rfind(')')+2:].split()[19]
   found.append(dict(pid=int(name),uid=owner,startTicks=ticks,executable=target,device=observed.st_dev,inode=observed.st_ino))
  except (FileNotFoundError,ProcessLookupError): continue
 return sorted(found,key=lambda value:value['pid'])
def legacy_rollback(uid,home,version,architecture):
 matches=[]; inspected=0
 for directory,owners in ((os.path.join(home,'Downloads'),{uid}),('/var/cache/apt/archives',{0})):
  if not os.path.exists(directory): continue
  if os.path.realpath(directory)!=directory or not os.path.isdir(directory): raise RuntimeError('legacy rollback directory is redirected')
  with os.scandir(directory) as entries:
   for entry in entries:
    inspected+=1
    if inspected>512: raise RuntimeError('legacy rollback locations exceed their review bound')
    if not entry.name.endswith('.deb'): continue
    try: identity,_=legacy_file(entry.path,1073741824,owners)
    except (OSError,RuntimeError): continue
    values=[]
    for field in ('Package','Version','Architecture'):
     code,value=legacy_run(['/usr/bin/dpkg-deb','--field',entry.path,field],8192)
     if code: break
     values.append(value.decode().strip())
    if code: continue
    if values==['nvpair',version,architecture]: matches.append(identity)
 return matches
def inspect_legacy_package(uid,home):
 if not os.path.exists('/usr/bin/dpkg-query'): return dict(legacyPackageStatus='not-applicable')
 legacy_tool('/usr/bin/dpkg-query')
 code,record=legacy_run(['/usr/bin/dpkg-query','-W','-f=${binary:Package}\t${Status}\t${Version}\t${Architecture}\n','nvpair'],8192)
 native={'aarch64':'arm64','arm64':'arm64','x86_64':'amd64','amd64':'amd64'}.get(platform.machine())
 state,package,package_status,version,architecture=legacy_package_record_state(code,record,None,native)
 if state=='absent': return dict(legacyPackageStatus='absent')
 if not os.path.exists('/usr/bin/dpkg') or not os.path.exists('/usr/bin/dpkg-deb'): return dict(legacyPackageStatus='retained-no-exact-rollback')
 legacy_tool('/usr/bin/dpkg'); legacy_tool('/usr/bin/dpkg-deb')
 app,_=legacy_file(LEGACY_APP,1073741824,{0}); launcher,launcher_body=legacy_file(LEGACY_LAUNCHER,8192,{0}); desktop,_=legacy_file(LEGACY_DESKTOP,1048576,{0})
 if app['gid']!=0 or launcher['gid']!=0 or desktop['gid']!=0: raise RuntimeError('legacy package app or launcher has a foreign group')
 expected=b"#!/bin/sh\n# Personal AI Router terminal UI launcher.\nexec '/opt/PAIR/resources/cli-bin/nvpair-tui' \"$@\"\n"
 if launcher_body!=expected or not app['mode']&0o111 or not launcher['mode']&0o111: raise RuntimeError('legacy package app or launcher ownership is unrecognized')
 code,files=legacy_run(['/usr/bin/dpkg-query','-L','nvpair'],1048576)
 listed=set(files.decode().splitlines())
 if code or LEGACY_APP not in listed or LEGACY_DESKTOP not in listed: raise RuntimeError('legacy package file ownership is incomplete')
 code,verified=legacy_run(['/usr/bin/dpkg','--verify','nvpair'],1048576,60)
 if code or verified.strip(): raise RuntimeError('legacy package files differ from dpkg ownership')
 processes=legacy_processes()
 if processes: raise RuntimeError('close the reviewed legacy PAIR desktop before upgrade; no process was stopped')
 rollback=legacy_rollback(uid,home,version,architecture)
 if len(rollback)!=1: return dict(legacyPackageStatus='retained-no-exact-rollback')
 receipt=dict(package=package,status=package_status,version=version,architecture=architecture,packageRecordSha256=hashlib.sha256(record).hexdigest(),packageFilesSha256=hashlib.sha256(files).hexdigest(),packageInfoSha256=legacy_info_hash(),app=app,launcher=launcher,desktopEntry=desktop,processes=processes,rollback=rollback[0])
 return dict(legacyPackageStatus='admitted',legacyPackage=receipt)
`

const onboardingUpgradeInspectScript = onboardingUpgradeInspectPrelude + onboardingLegacyPackageInspectScript + onboardingRetentionInspectScript + `
answer=inspect_existing(); answer.update(inspect_legacy_package(answer['uid'],answer['home'])); answer['retention']=inspect_retention(answer); print(json.dumps(answer))
`

func onboardingLegacyPackageAdmission(result *onboardingExistingInstallation, elevationAvailable bool) {
	if result.LegacyPackage != nil && !elevationAvailable {
		result.LegacyPackage = nil
		result.LegacyPackageStatus = onboardingLegacyPackageNoElevation
	}
}

type onboardingLegacyPackageResult struct {
	Retired           bool   `json:"retired"`
	RollbackConfirmed bool   `json:"rollbackConfirmed"`
	SuccessorStarted  bool   `json:"successorStarted"`
	State             string `json:"state"`
	FailureCode       string `json:"failureCode,omitempty"`
}

func onboardingLegacyPackageInvocation(action string, receipt onboardingInstallReceipt, old onboardingExistingInstallation, unitSHA256, password string) (string, []byte, error) {
	if action != "remove" && action != "rollback" && action != "reconcile" && action != "commit" || old.LegacyPackage == nil || password == "" || len(password) > 4096 || strings.ContainsAny(password, "\r\n\x00") || !onboardingSHA.MatchString(unitSHA256) {
		return "", nil, errors.New("legacy package administrator action is not bound")
	}
	header := onboardingMarshal(map[string]any{"action": action, "receipt": receipt, "existing": old, "successorUnitSha256": unitSHA256})
	if len(header) > 65536 {
		return "", nil, errors.New("legacy package request exceeds its bound")
	}
	encoded := base64.StdEncoding.EncodeToString(header)
	input := make([]byte, 0, len(password)+1)
	input = append(input, password...)
	input = append(input, '\n')
	command := "/usr/bin/sudo -S -p '' -- /usr/bin/python3 -I -c " + onboardingQuote(onboardingLegacyPackageRootScript) + " " + onboardingQuote(encoded)
	return command, input, nil
}

func validateOnboardingLegacyPackageResult(action string, result onboardingLegacyPackageResult) error {
	if action == "remove" && result.Retired && !result.RollbackConfirmed && result.SuccessorStarted && result.State == "retired" && result.FailureCode == "" {
		return nil
	}
	if action == "rollback" && !result.Retired && result.RollbackConfirmed && result.SuccessorStarted && result.State == "rolled-back" && result.FailureCode != "" {
		return nil
	}
	if action == "reconcile" && result.SuccessorStarted && (result.Retired && result.State == "retired" && !result.RollbackConfirmed || !result.Retired && result.State == "ready" && !result.RollbackConfirmed || !result.Retired && result.State == "rolled-back" && result.RollbackConfirmed && result.FailureCode != "") {
		return nil
	}
	if action == "commit" && result.Retired && !result.RollbackConfirmed && result.SuccessorStarted && result.State == "committed" && result.FailureCode == "" {
		return nil
	}
	if result.Retired || !result.RollbackConfirmed || !result.SuccessorStarted || result.State != "rolled-back" || result.FailureCode == "" {
		return errors.New("legacy desktop package retirement and rollback are unconfirmed")
	}
	return errors.New("legacy desktop package retirement failed; the exact reviewed package was restored")
}

func runOnboardingLegacyPackageAction(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt, old onboardingExistingInstallation, access onboardingAccess, action string) (onboardingLegacyPackageResult, error) {
	var result onboardingLegacyPackageResult
	if old.LegacyPackage == nil {
		return result, nil
	}
	broker := path.Join(path.Dir(receipt.TUIPath), "nvpair-ui-broker")
	unit := onboardingUpgradeUnitText(receipt.TUIPath, broker, old.ConfigHome, 145)
	digest := sha256.Sum256([]byte(unit))
	unitSHA256 := hex.EncodeToString(digest[:])
	command, input, err := onboardingLegacyPackageInvocation(action, receipt, old, unitSHA256, access.elevationPassword)
	if err != nil {
		return result, err
	}
	defer clear(input)
	call, cancel := context.WithTimeout(ctx, onboardingUpgradePhaseBudget)
	defer cancel()
	body, err := client.run(call, command, bytes.NewReader(input))
	if err != nil || onboardingDecode(body, &result) != nil {
		return result, errors.New("legacy desktop package action is unconfirmed; preserve this operation for recovery")
	}
	return result, validateOnboardingLegacyPackageResult(action, result)
}

func (s *onboardingService) retireOnboardingLegacyPackage(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt, old onboardingExistingInstallation, access onboardingAccess, controller string) error {
	if old.LegacyPackage == nil {
		return nil
	}
	result, err := runOnboardingLegacyPackageAction(ctx, client, receipt, old, access, "remove")
	if err == nil {
		_, _, err = s.waitOnboardingMembership(ctx, client, receipt, controller, old.NodeID, old.ClusterID)
		if err == nil {
			err = verifyOnboardingUpgradedIdentity(ctx, client, receipt, old)
		}
		if err == nil {
			commitResult, commitErr := runOnboardingLegacyPackageAction(ctx, client, receipt, old, access, "commit")
			if commitErr == nil {
				return nil
			}
			if commitResult.RollbackConfirmed && commitResult.SuccessorStarted {
				recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), onboardingLegacyPackageRecoveryBudget)
				_, _, confirmErr := s.waitOnboardingMembership(recoveryCtx, client, receipt, controller, old.NodeID, old.ClusterID)
				if confirmErr == nil {
					confirmErr = verifyOnboardingUpgradedIdentity(recoveryCtx, client, receipt, old)
				}
				recoveryCancel()
				if confirmErr != nil {
					return errors.New("legacy package commit rollback or successor membership is unconfirmed; preserve this operation")
				}
			}
			return commitErr
		}
		// A package removal is not accepted when its successor cannot immediately
		// prove the same identity and reciprocal membership. Restore the exact
		// reviewed DEB, then prove the successor again.
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), onboardingLegacyPackageRecoveryBudget)
		defer recoveryCancel()
		result, rollbackErr := runOnboardingLegacyPackageAction(recoveryCtx, client, receipt, old, access, "rollback")
		if rollbackErr == nil {
			_, _, rollbackErr = s.waitOnboardingMembership(recoveryCtx, client, receipt, controller, old.NodeID, old.ClusterID)
		}
		if rollbackErr == nil {
			rollbackErr = verifyOnboardingUpgradedIdentity(recoveryCtx, client, receipt, old)
		}
		if rollbackErr != nil || !result.RollbackConfirmed {
			return errors.New("legacy package rollback or successor membership is unconfirmed; preserve this operation")
		}
		return errors.New("legacy package was restored because successor membership revalidation failed")
	}
	if result.RollbackConfirmed && result.SuccessorStarted {
		recoveryCtx, recoveryCancel := context.WithTimeout(context.WithoutCancel(ctx), onboardingLegacyPackageRecoveryBudget)
		_, _, membershipErr := s.waitOnboardingMembership(recoveryCtx, client, receipt, controller, old.NodeID, old.ClusterID)
		if membershipErr == nil {
			membershipErr = verifyOnboardingUpgradedIdentity(recoveryCtx, client, receipt, old)
		}
		recoveryCancel()
		if membershipErr == nil {
			return err
		}
	}
	return err
}

// Root owns only the exact Debian transaction. It never accepts argv, package
// names, units or paths from the UI: all targets are rederived and rebound to
// the review receipt before each effect. Package hooks own their own removals;
// this worker contains no filesystem delete or process-kill primitive.
const onboardingLegacyPackageRootScript = `import base64,binascii,hashlib,json,os,pwd,re,stat,subprocess,sys,tempfile,time
` + onboardingLegacyPackageArgumentScript + onboardingLegacyPackageRecordParserScript + `
raw=legacy_package_request_argument()
h=json.loads(raw)
if set(h)!={'action','receipt','existing','successorUnitSha256'} or h['action'] not in ('remove','rollback','reconcile','commit'): raise RuntimeError('invalid legacy package action')
if os.geteuid()!=0: raise RuntimeError('legacy package action requires the approved administrator route')
r=h['receipt']; existing=h['existing']; legacy=existing.get('legacyPackage')
if not isinstance(legacy,dict) or existing.get('legacyPackageStatus')!='admitted' or legacy.get('package')!='nvpair': raise RuntimeError('legacy package was not admitted by review')
uid=existing['uid']; account=pwd.getpwuid(uid); home=os.path.normpath(account.pw_dir)
if uid<=0 or home!=existing['home'] or existing['configHome']!=os.path.join(home,'.config'): raise RuntimeError('reviewed legacy package account changed')
ROOT_ENV={'PATH':'/usr/sbin:/usr/bin:/sbin:/bin','LC_ALL':'C','LANG':'C','HOME':'/root','USER':'root','LOGNAME':'root'}
def root_tool(filename):
 observed=os.lstat(filename)
 if not stat.S_ISREG(observed.st_mode) or observed.st_uid!=0 or observed.st_gid!=0 or observed.st_mode&0o022 or not observed.st_mode&0o111 or os.path.realpath(filename)!=filename: raise RuntimeError('fixed package tool ownership is unsafe')
for tool in ('/usr/bin/dpkg','/usr/bin/dpkg-query','/usr/bin/systemctl','/usr/bin/env'): root_tool(tool)
op=os.path.basename(r['stagePath']); root=os.path.join(home,'.local','share','Nvidia Corporation','Personal AI Router'); stage=os.path.join(root,'.onboarding',op); bundle=os.path.join(root,'bundles',r['artifactSha256']); tui=os.path.join(bundle,'bin','nvpair-tui')
if r['stagePath']!=stage or r['bundlePath']!=bundle or r['tuiPath']!=tui or len(op)!=32 or any(c not in '0123456789abcdef' for c in op): raise RuntimeError('successor receipt targets changed')
for directory,owner in ((stage,uid),(bundle,uid)):
 observed=os.lstat(directory)
 if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=owner or observed.st_mode&0o022 or os.path.realpath(directory)!=directory: raise RuntimeError('successor operation storage is redirected, foreign or writable')
def digest_file(filename,limit,owners):
 if not os.path.isabs(filename) or os.path.normpath(filename)!=filename or os.path.realpath(filename)!=filename: raise RuntimeError('legacy package path is redirected')
 before=os.lstat(filename)
 if not stat.S_ISREG(before.st_mode) or before.st_uid not in owners or before.st_mode&0o022 or before.st_nlink!=1 or before.st_size<=0 or before.st_size>limit: raise RuntimeError('legacy package file is foreign, shared, writable or oversized')
 fd=os.open(filename,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC)
 try:
  current=os.fstat(fd); digest=hashlib.sha256()
  while True:
   chunk=os.read(fd,1048576)
   if not chunk: break
   digest.update(chunk)
  after=os.fstat(fd)
  if (before.st_dev,before.st_ino)!=(current.st_dev,current.st_ino) or (current.st_dev,current.st_ino,current.st_size,current.st_nlink)!=(after.st_dev,after.st_ino,after.st_size,after.st_nlink): raise RuntimeError('legacy package file changed while read')
 finally: os.close(fd)
 return dict(path=filename,sha256=digest.hexdigest(),bytes=current.st_size,device=current.st_dev,inode=current.st_ino,uid=current.st_uid,gid=current.st_gid,mode=stat.S_IMODE(current.st_mode),links=current.st_nlink)
def same_file(expected,limit,owners,exact_inode=True):
 observed=digest_file(expected['path'],limit,owners)
 keys=('path','sha256','bytes','uid','gid','mode','links')+(("device","inode") if exact_inode else ())
 if any(observed.get(key)!=expected.get(key) for key in keys): raise RuntimeError('legacy package file changed from its reviewed identity')
 return observed
def run(argv,timeout=300):
 answer=subprocess.run(argv,stdin=subprocess.DEVNULL,stdout=subprocess.DEVNULL,stderr=subprocess.DEVNULL,timeout=timeout,env=ROOT_ENV)
 return answer.returncode
def capture(argv,limit=1048576,timeout=60):
 answer=subprocess.run(argv,stdin=subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,timeout=timeout,env=ROOT_ENV)
 if len(answer.stdout)>limit: raise RuntimeError('legacy package query exceeds its bound')
 return answer.returncode,answer.stdout
def package_record(): return capture(['/usr/bin/dpkg-query','-W','-f=${binary:Package}\t${Status}\t${Version}\t${Architecture}\n','nvpair'],8192)
def package_state():
 code,record=package_record(); state,package,status,version,architecture=legacy_package_record_state(code,record,legacy['version'],legacy['architecture'])
 return state,record
def info_hash():
 digest=hashlib.sha256()
 for suffix in ('list','md5sums','preinst','postinst','prerm','postrm'):
  filename='/var/lib/dpkg/info/nvpair.'+suffix; digest.update(suffix.encode()+b'\0')
  if not os.path.lexists(filename): digest.update(b'absent\0'); continue
  digest.update(digest_file(filename,1048576,{0})['sha256'].encode()+b'\0')
 return digest.hexdigest()
def package_processes():
 found=[]
 for name in os.listdir('/proc'):
  if not name.isdigit(): continue
  try:
   target=os.readlink('/proc/'+name+'/exe')
   if target.endswith(' (deleted)') and target.startswith('/opt/PAIR/'): raise RuntimeError('legacy package has a deleted running owner')
   if target=='/opt/PAIR' or target.startswith('/opt/PAIR/'): found.append(int(name))
  except (FileNotFoundError,ProcessLookupError): continue
 return found
def installed_receipt(expected,exact_inode):
 state,record=package_state()
 if state!='installed' or hashlib.sha256(record).hexdigest()!=legacy['packageRecordSha256']: raise RuntimeError('installed legacy package record changed')
 code,files=capture(['/usr/bin/dpkg-query','-L','nvpair'])
 if code or hashlib.sha256(files).hexdigest()!=legacy['packageFilesSha256'] or info_hash()!=legacy['packageInfoSha256']: raise RuntimeError('installed legacy package ownership changed')
 code,verified=capture(['/usr/bin/dpkg','--verify','nvpair'])
 if code or verified.strip() or package_processes(): raise RuntimeError('legacy package verification or process ownership changed')
 return dict(app=same_file(expected['app'],1073741824,{0},exact_inode),launcher=same_file(expected['launcher'],8192,{0},exact_inode),desktopEntry=same_file(expected['desktopEntry'],1048576,{0},exact_inode))
def package_absent():
 state,record=package_state()
 if state=='installed': return False
 for filename in ('/opt/PAIR/nvpair','/usr/bin/nvpair','/usr/share/applications/nvpair.desktop'):
  if os.path.lexists(filename): raise RuntimeError('legacy package path remains with ambiguous ownership')
 return True
proofpath=os.path.join(stage,'.pair-legacy-package-retirement.json'); binding=hashlib.sha256(json.dumps(legacy,sort_keys=True,separators=(',',':')).encode()).hexdigest(); proof=None
spooldir='/var/tmp'; spoolpath=os.path.join(spooldir,'.nvpair-rollback-'+binding+'-'+op+'.deb')
def spool_directory():
 observed=os.lstat(spooldir); mode=stat.S_IMODE(observed.st_mode)
 if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=0 or observed.st_gid!=0 or not mode&0o1000 or os.path.realpath(spooldir)!=spooldir: raise RuntimeError('root rollback spool ownership is unsafe')
def checked_spool():
 spool_directory(); observed=digest_file(spoolpath,1073741824,{0})
 if observed['sha256']!=legacy['rollback']['sha256'] or observed['bytes']!=legacy['rollback']['bytes'] or observed['gid']!=0 or observed['mode']!=0o600: raise RuntimeError('root rollback spool differs from reviewed archive')
 return spoolpath
def stage_rollback():
 spool_directory()
 if os.path.lexists(spoolpath): return checked_spool()
 expected=legacy['rollback']; source=os.open(expected['path'],os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC)
 destination=None
 try:
  opened=os.fstat(source); keys=('device','inode','uid','gid','mode','links','bytes')
  actual=dict(device=opened.st_dev,inode=opened.st_ino,uid=opened.st_uid,gid=opened.st_gid,mode=stat.S_IMODE(opened.st_mode),links=opened.st_nlink,bytes=opened.st_size)
  if not stat.S_ISREG(opened.st_mode) or any(actual[key]!=expected[key] for key in keys): raise RuntimeError('reviewed rollback archive changed before root custody')
  destination=os.open(spoolpath,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW|os.O_CLOEXEC,0o600); digest=hashlib.sha256(); total=0
  while True:
   chunk=os.read(source,1048576)
   if not chunk: break
   digest.update(chunk); total+=len(chunk); offset=0
   while offset<len(chunk):
    written=os.write(destination,chunk[offset:])
    if written<=0: raise RuntimeError('root rollback spool write made no progress')
    offset+=written
  if total!=expected['bytes'] or digest.hexdigest()!=expected['sha256']: raise RuntimeError('reviewed rollback archive changed while entering root custody')
  os.fchmod(destination,0o600); os.fchown(destination,0,0); os.fsync(destination)

  directory=os.open(spooldir,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC)
  try: os.fsync(directory)
  finally: os.close(directory)
 except Exception:
  if os.path.lexists(spoolpath):
   partial=os.lstat(spoolpath)
   if stat.S_ISREG(partial.st_mode) and partial.st_uid==0: os.unlink(spoolpath)
  raise
 finally:
  os.close(source)
  if destination is not None: os.close(destination)
 try: return checked_spool()
 except Exception:
  if os.path.lexists(spoolpath):
   observed=os.lstat(spoolpath)
   if stat.S_ISREG(observed.st_mode) and observed.st_uid==0: os.unlink(spoolpath)
  raise
def clear_spool():
 checked_spool(); os.unlink(spoolpath)
 directory=os.open(spooldir,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC)
 try: os.fsync(directory)
 finally: os.close(directory)
def read_proof():
 if not os.path.lexists(proofpath): return None
 observed=os.lstat(proofpath)
 if not stat.S_ISREG(observed.st_mode) or observed.st_uid!=uid or stat.S_IMODE(observed.st_mode)!=0o600 or observed.st_nlink!=1: raise RuntimeError('legacy package retirement proof ownership changed')
 fd=os.open(proofpath,os.O_RDONLY|os.O_NOFOLLOW|os.O_CLOEXEC)
 with os.fdopen(fd,'rb') as stream: data=stream.read(65537)
 if len(data)>65536: raise RuntimeError('legacy package retirement proof exceeds its bound')
 value=json.loads(data)
 if value.get('owner')!='nvidia-pair-legacy-package-retirement-v1' or value.get('operation')!=op or value.get('binding')!=binding or value.get('state') not in ('intent','removed','retired','rolled-back','commit-intent','committed') or not isinstance(value.get('attempt'),int) or value['attempt']<1: raise RuntimeError('legacy package retirement proof binding is invalid')
 return value
def seal_proof(state,attempt,rollback_observed=None,failure=None):
 global proof
 value=dict(owner='nvidia-pair-legacy-package-retirement-v1',operation=op,binding=binding,state=state,attempt=attempt)
 if rollback_observed is not None: value['rollbackObserved']=rollback_observed
 if failure is not None: value['failureCode']=failure
 encoded=json.dumps(value,sort_keys=True,separators=(',',':')).encode(); fd,temporary=tempfile.mkstemp(prefix='.pair-legacy-package-',dir=stage)
 try:
  os.fchmod(fd,0o600); os.fchown(fd,uid,account.pw_gid)
  with os.fdopen(fd,'wb') as stream: stream.write(encoded); stream.flush(); os.fsync(stream.fileno())
  os.replace(temporary,proofpath)
  directory=os.open(stage,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC)
  try: os.fsync(directory)
  finally: os.close(directory)
 finally:
  if os.path.lexists(temporary): os.unlink(temporary)
 proof=value
def runuser_path():
 for candidate in ('/usr/sbin/runuser','/sbin/runuser'):
  try:
   observed=os.lstat(candidate)
   if stat.S_ISREG(observed.st_mode) and observed.st_uid==0 and observed.st_gid==0 and observed.st_mode&0o111 and not observed.st_mode&0o022 and os.path.realpath(candidate)==candidate: return candidate
  except FileNotFoundError: pass
 raise RuntimeError('fixed user service transition helper is unavailable')
runtime='/run/user/'+str(uid); userenv=['XDG_RUNTIME_DIR='+runtime,'DBUS_SESSION_BUS_ADDRESS=unix:path='+runtime+'/bus','XDG_CONFIG_HOME='+existing['configHome']]
def user_systemctl(*args): return capture([runuser_path(),'-u',account.pw_name,'--','/usr/bin/env']+userenv+['/usr/bin/systemctl','--user','--no-ask-password','--no-pager']+list(args),16384,60)
def manager():
 fields=('LoadState','ActiveState','SubState','MainPID','FragmentPath','DropInPaths','Job')
 code,body=user_systemctl('show','nvidia-pair-headless.service',*['--property='+field for field in fields])
 values={}
 for line in body.decode().splitlines():
  key,separator,value=line.partition('=')
  if not separator or key in values: raise RuntimeError('successor user service state is ambiguous')
  values[key]=value
 if code or set(values)!=set(fields) or values['LoadState']!='loaded' or values['FragmentPath']!=existing['unitPath'] or values['DropInPaths'] or values['Job'] not in ('','0'): raise RuntimeError('successor user service owner changed')
 unit=digest_file(existing['unitPath'],16384,{uid})
 if unit['sha256']!=h['successorUnitSha256']: raise RuntimeError('successor unit content changed')
 return values
def stop_successor():
 values=manager()
 if values['ActiveState']=='inactive' and values['MainPID']=='0': return
 if values['ActiveState']!='active' or values['SubState']!='running' or not values['MainPID'].isdigit() or values['MainPID']=='0': raise RuntimeError('successor state is not safely stoppable')
 pid=int(values['MainPID']); target=os.readlink('/proc/'+str(pid)+'/exe')
 if target!=tui or os.stat('/proc/'+str(pid)).st_uid!=uid: raise RuntimeError('successor process ownership changed')
 code,_=user_systemctl('stop','nvidia-pair-headless.service')
 if code: raise RuntimeError('successor exact unit stop failed')
 values=manager()
 if values['ActiveState']!='inactive' or values['MainPID']!='0': raise RuntimeError('successor exact unit stop is unconfirmed')
def start_successor():
 values=manager()
 if values['ActiveState']!='active':
  code,_=user_systemctl('start','nvidia-pair-headless.service')
  if code: raise RuntimeError('successor exact unit start failed')
 deadline=time.monotonic()+20
 while True:
  values=manager()
  if values['ActiveState']=='active' and values['SubState']=='running' and values['MainPID'].isdigit() and values['MainPID']!='0':
   pid=int(values['MainPID'])
   if os.readlink('/proc/'+str(pid)+'/exe')==tui and os.stat('/proc/'+str(pid)).st_uid==uid: return
  if time.monotonic()>=deadline: raise RuntimeError('successor exact unit restart is unconfirmed')
  time.sleep(.2)
def rollback_package(attempt,failure):
 archive=stage_rollback()
 if not package_absent():
  try: observed=installed_receipt(legacy,False); seal_proof('rolled-back',attempt,observed,failure); return True
  except Exception: pass
 for filename,key in (('/opt/PAIR/nvpair','app'),('/usr/bin/nvpair','launcher'),('/usr/share/applications/nvpair.desktop','desktopEntry')):
  if os.path.lexists(filename):
   same_file(legacy[key],1073741824 if key=='app' else 1048576,{0},False)
 if run(['/usr/bin/dpkg','--install',archive]): return False
 observed=installed_receipt(legacy,False); seal_proof('rolled-back',attempt,observed,failure); return True
proof=read_proof(); attempt=1 if proof is None else proof['attempt']
try:
 if h['action']=='reconcile':
  if proof is None:
   installed_receipt(legacy,True); start_successor(); print(json.dumps(dict(retired=False,rollbackConfirmed=False,successorStarted=True,state='ready'))); sys.exit(0)
  if proof['state']=='intent':
   stop_successor()
   if not rollback_package(attempt,'interrupted-package-action'): raise RuntimeError('interrupted package action could not be rolled back')
   start_successor(); print(json.dumps(dict(retired=False,rollbackConfirmed=True,successorStarted=True,state='rolled-back',failureCode='interrupted-package-action'))); sys.exit(0)
  if proof['state']=='rolled-back':
   expected=dict(legacy); expected.update(proof.get('rollbackObserved',{})); installed_receipt(expected,False); start_successor(); print(json.dumps(dict(retired=False,rollbackConfirmed=True,successorStarted=True,state='rolled-back',failureCode=proof.get('failureCode','prior-package-action-failed')))); sys.exit(0)
  if proof['state'] in ('commit-intent','committed'):
   if not package_absent(): raise RuntimeError('committed package state changed')
   start_successor()
   if proof['state']=='commit-intent':
    if os.path.lexists(spoolpath): clear_spool()
    seal_proof('committed',attempt)
   elif os.path.lexists(spoolpath): raise RuntimeError('committed rollback spool reappeared')
   print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired'))); sys.exit(0)
  if not package_absent(): raise RuntimeError('removed package state changed')
  checked_spool()
  start_successor()
  if proof['state']=='removed': seal_proof('retired',attempt)
  print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired'))); sys.exit(0)
 if h['action']=='rollback':
  stop_successor()
  if not rollback_package(attempt,'post-removal-revalidation-failed'): raise RuntimeError('exact rollback package could not be restored')
  start_successor(); print(json.dumps(dict(retired=False,rollbackConfirmed=True,successorStarted=True,state='rolled-back',failureCode='post-removal-revalidation-failed'))); sys.exit(0)
 if h['action']=='commit':
  if proof is None or proof['state'] not in ('retired','commit-intent','committed') or not package_absent(): raise RuntimeError('legacy package retirement is not ready to commit')
  start_successor()
  if proof['state']=='committed':
   if os.path.lexists(spoolpath): raise RuntimeError('committed rollback spool reappeared')
  else:
   if proof['state']=='retired': seal_proof('commit-intent',attempt)
   if os.path.lexists(spoolpath): clear_spool()
   seal_proof('committed',attempt)
  print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='committed'))); sys.exit(0)
 if proof and proof['state']=='committed':
  if not package_absent() or os.path.lexists(spoolpath): raise RuntimeError('committed legacy package state changed')
  start_successor(); print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired'))); sys.exit(0)
 if proof and proof['state']=='retired':
  if not package_absent(): raise RuntimeError('retired package reappeared')
  checked_spool()
  start_successor(); print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired'))); sys.exit(0)
 if proof and proof['state']=='removed':
  if not package_absent(): raise RuntimeError('removed package state changed')
  checked_spool()
  start_successor(); seal_proof('retired',attempt); print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired'))); sys.exit(0)
 expected=legacy
 if proof and proof['state']=='rolled-back':
  expected=dict(legacy); expected.update(proof.get('rollbackObserved',{})); attempt+=1
 elif proof and proof['state']=='intent':
  stop_successor()
  if not rollback_package(attempt,'interrupted-package-action'): raise RuntimeError('interrupted package action could not be rolled back')
  start_successor(); print(json.dumps(dict(retired=False,rollbackConfirmed=True,successorStarted=True,state='rolled-back',failureCode='interrupted-package-action'))); sys.exit(0)
 installed_receipt(expected,proof is None); stage_rollback(); stop_successor(); seal_proof('intent',attempt)
 if run(['/usr/bin/dpkg','--remove','nvpair']): raise RuntimeError('dpkg removal failed')
 if not package_absent(): raise RuntimeError('dpkg removal result is ambiguous')
 seal_proof('removed',attempt); start_successor(); seal_proof('retired',attempt)
 print(json.dumps(dict(retired=True,rollbackConfirmed=False,successorStarted=True,state='retired')))
except Exception:
 failure='package-action-failed'; rolled=False; started=False
 try:
  stop_successor(); rolled=rollback_package(attempt,failure)
 except Exception: rolled=False
 try: start_successor(); started=True
 except Exception: started=False
 print(json.dumps(dict(retired=False,rollbackConfirmed=rolled,successorStarted=started,state='rolled-back' if rolled else 'unconfirmed',failureCode=failure)))
`

func onboardingLegacyPackageDisposition(old onboardingExistingInstallation) string {
	disposition := "legacy CLI inventory retired"
	if _, managed := onboardingManagedBundleDigest(old); managed {
		disposition = "prior managed bundle retained as the registered rollback"
	}
	switch old.LegacyPackageStatus {
	case onboardingLegacyPackageAdmitted:
		disposition += "; legacy Debian desktop package retired with an exact reviewed rollback archive"
	case onboardingLegacyPackageNoRollback:
		disposition += "; legacy Debian desktop package retained because no unique exact rollback archive was admitted"
	case onboardingLegacyPackageNoElevation:
		disposition += "; legacy Debian desktop package retained because administrator access was not admitted"
	}
	return disposition
}

func onboardingLegacyPackageCompletion(old onboardingExistingInstallation) string {
	return fmt.Sprintf("Selected headless installation upgraded: original node identity and membership retained, %s, and reviewed user launcher reconciled", onboardingLegacyPackageDisposition(old))
}

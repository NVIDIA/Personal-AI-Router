// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

type onboardingInstallReceipt struct {
	StartupLifetime  string `json:"startupLifetime"`
	ManifestSHA256   string `json:"manifestSha256"`
	NodeID           string `json:"nodeId,omitempty"`
	Recoverable      bool   `json:"recoverable"`
	StagePath        string `json:"stagePath"`
	BundlePath       string `json:"bundlePath"`
	TUIPath          string `json:"tuiPath"`
	ArtifactSHA256   string `json:"artifactSha256"`
	Installed        bool   `json:"installed"`
	ServiceInstalled bool   `json:"serviceInstalled"`
	ServiceStarted   bool   `json:"serviceStarted"`
	CleanupConfirmed bool   `json:"cleanupConfirmed"`
}

type onboardingInstallRunner interface {
	run(context.Context, string, io.Reader) ([]byte, error)
}

const onboardingReceiverUpgradeInspectScript = `
def inspect_reviewed_upgrade():
 observed=inspect_existing(); observed['retention']=inspect_retention(observed)
 return observed
`

// The receiver is product code, not a command supplied by the UI. It consumes
// a bounded JSON header followed by exact archive bytes on authenticated stdin.
// It validates again on the target before activation; no tar links, ownership
// restoration, privileged modes, executable rewriting or installer downloads.
const onboardingReceiveScript = onboardingUpgradeInspectPrelude + onboardingRetentionInspectScript + onboardingReceiverUpgradeInspectScript + `
import ctypes,hashlib,json,os,platform,pwd,shutil,stat,sys,tarfile
os.umask(0o077) # Receiver-local: makedirs secures intermediate directories too.
h=json.loads(sys.stdin.buffer.readline(16385))
required={'operationId','sha256','manifestSha256','startupLifetime','bytes','archiveRoot','arch','uid','home'}
if 'upgrade' in h: required.add('upgrade')
if set(h)!=required: raise RuntimeError('invalid product transfer header')
uid=os.geteuid(); home=os.path.normpath(pwd.getpwuid(uid).pw_dir)
if uid<=0 or uid!=h['uid'] or home!=os.path.normpath(h['home']): raise RuntimeError('approved target account changed')
native={'aarch64':'arm64','arm64':'arm64','x86_64':'amd64','amd64':'amd64'}.get(platform.machine())
if platform.system()!='Linux' or native!=h['arch']: raise RuntimeError('approved native target architecture changed')
op=h['operationId']; digest=h['sha256']
if len(op)!=32 or any(c not in '0123456789abcdef' for c in op) or len(digest)!=64 or any(c not in '0123456789abcdef' for c in digest): raise RuntimeError('invalid owned installation identity')
size=h['bytes']
if not isinstance(size,int) or size<=0 or size>1073741824: raise RuntimeError('invalid package size')
vendor=os.path.join(home,'.local','share','Nvidia Corporation')
root=os.path.join(vendor,'Personal AI Router')
def receiver_directory(path,writable=(),missing=False):
 if not os.path.isabs(path) or os.path.normpath(path)!=path or os.path.commonpath([home,path])!=home: raise RuntimeError('PAIR storage path is outside the approved account')
 flags=os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW
 anchor=os.path.splitdrive(path)[0]+os.sep
 fd=os.open(anchor,flags); current=anchor
 try:
  observed=os.fstat(fd)
  if observed.st_uid not in {0,uid} or observed.st_mode&0o022: raise RuntimeError('PAIR storage anchor is foreign-owned or writable')
  for part in path[len(anchor):].split(os.sep):
   current=os.path.join(current,part)
   try: child=os.open(part,flags,dir_fd=fd)
   except FileNotFoundError:
    if missing: os.close(fd); return None
    raise
   os.close(fd); fd=child; observed=os.fstat(fd)
   owners={uid} if current==home or current.startswith(home+os.sep) else {0,uid}
   if not stat.S_ISDIR(observed.st_mode) or observed.st_uid not in owners or (observed.st_mode&0o022 and current not in writable): raise RuntimeError('PAIR storage ancestry is redirected, foreign-owned or writable outside its approved boundary')
  return fd
 except BaseException:
  os.close(fd); raise
def receiver_bound_bundle(bundle,manifest_sha,expected_marker=None):
 if os.path.dirname(bundle)!=os.path.join(root,'bundles') or len(os.path.basename(bundle))!=64 or any(c not in '0123456789abcdef' for c in os.path.basename(bundle)): raise RuntimeError('PAIR permission repair is outside its owned bundle namespace')
 fd=receiver_directory(bundle,(vendor,bundle))
 try:
  def read(name,limit):
   item=os.open(name,os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=fd)
   with os.fdopen(item,'rb') as stream:
    observed=os.fstat(stream.fileno())
    if not stat.S_ISREG(observed.st_mode) or observed.st_uid!=uid or observed.st_size>limit: raise RuntimeError('PAIR permission repair requires an account-owned marker')
    return stream.read(limit+1)
  marker=json.loads(read('.pair-onboarding.json',8192))
  # A legacy marker may be 0664: authority is the exact original operation and
  # manifest, or the freshly revalidated complete upgrade descriptor, not this marker alone.
  if set(marker)!={'owner','operationId','sha256','manifestSha256','startupLifetime'} or marker.get('owner')!='nvidia-pair-onboarding-v1' or marker.get('sha256')!=os.path.basename(bundle) or marker.get('manifestSha256')!=manifest_sha or not isinstance(marker.get('operationId'),str) or len(marker['operationId'])!=32 or any(c not in '0123456789abcdef' for c in marker['operationId']) or marker.get('startupLifetime') not in ('session','persistent') or (expected_marker is not None and marker!=expected_marker): raise RuntimeError('PAIR permission repair has no matching bundle owner marker')
  binary_fd=os.open('bin',os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW,dir_fd=fd)
  try:
   binary_info=os.fstat(binary_fd)
   if not stat.S_ISDIR(binary_info.st_mode) or binary_info.st_uid!=uid or binary_info.st_mode&0o022: raise RuntimeError('PAIR permission repair requires an unchanged private binary directory')
   item=os.open('manifest.json',os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK,dir_fd=binary_fd)
   with os.fdopen(item,'rb') as stream:
    observed=os.fstat(stream.fileno()); body=stream.read(131073)
    if observed.st_uid!=uid or not stat.S_ISREG(observed.st_mode) or observed.st_mode&0o022 or len(body)>131072 or hashlib.sha256(body).hexdigest()!=manifest_sha: raise RuntimeError('PAIR permission repair manifest binding changed')
  finally: os.close(binary_fd)
 finally: os.close(fd)
def receiver_harden(path,bundle,manifest_sha,expected_marker=None):
 receiver_bound_bundle(bundle,manifest_sha,expected_marker)
 fd=receiver_directory(path,(path,))
 try:
  before=os.fstat(fd)
  if path==vendor and os.listdir(fd)!=['Personal AI Router']: raise RuntimeError('shared vendor storage contains foreign children; permission repair is held')
  named=os.stat(path,follow_symlinks=False)
  if (before.st_dev,before.st_ino)!=(named.st_dev,named.st_ino): raise RuntimeError('PAIR permission repair directory changed')
  if before.st_mode&0o022: os.fchmod(fd,stat.S_IMODE(before.st_mode)&~0o022)
  after=os.fstat(fd); named=os.stat(path,follow_symlinks=False)
  if (before.st_dev,before.st_ino,before.st_uid)!=(after.st_dev,after.st_ino,after.st_uid) or (after.st_dev,after.st_ino)!=(named.st_dev,named.st_ino) or after.st_mode&0o022: raise RuntimeError('PAIR permission repair could not be confirmed')
 finally: os.close(fd)
parent=receiver_directory(vendor,(vendor,),missing=True)
if parent is not None:
 try:
  if os.fstat(parent).st_mode&0o022 and os.listdir(parent)!=['Personal AI Router']: raise RuntimeError('shared vendor storage contains foreign children; permission repair is held')
 finally: os.close(parent)
checked=receiver_directory(root,(vendor,),missing=True)
if checked is not None: os.close(checked)
def contained(p):
 if os.path.commonpath([os.path.realpath(home),os.path.realpath(p)])!=os.path.realpath(home): raise RuntimeError('installation path escapes account storage')
 if p!=root and os.path.commonpath([os.path.realpath(root),os.path.realpath(p)])!=os.path.realpath(root): raise RuntimeError('installation path escapes product storage')
contained(root); os.makedirs(root,mode=0o700,exist_ok=True)
if os.stat(root).st_uid!=uid or os.stat(root).st_mode&0o022: raise RuntimeError('installation storage is not private to the approved account')
stage=os.path.join(root,'.onboarding',op); final=os.path.join(root,'bundles',digest)
for p in [os.path.dirname(stage),os.path.dirname(final)]:
 contained(p); os.makedirs(p,mode=0o700,exist_ok=True)
 if os.stat(p).st_uid!=uid or os.stat(p).st_mode&0o022: raise RuntimeError('installation parent ownership changed')
marker={'owner':'nvidia-pair-onboarding-v1','operationId':op,'sha256':digest,'manifestSha256':h['manifestSha256'],'startupLifetime':h['startupLifetime']}
def marked(p):
 try:
  if os.path.islink(p) or os.path.islink(os.path.join(p,'.pair-onboarding.json')): return False
  if os.stat(p).st_uid!=uid or os.stat(os.path.join(p,'.pair-onboarding.json')).st_uid!=uid: return False
  with open(os.path.join(p,'.pair-onboarding.json')) as f: return json.load(f)==marker
 except (OSError,ValueError): return False
existing=os.path.lexists(final)
if existing and not marked(final): raise RuntimeError('existing bundle is not owned by this approved operation')
identity=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router')
if 'upgrade' in h:
 if inspect_reviewed_upgrade()!=h['upgrade']: raise RuntimeError('reviewed existing installation changed before successor staging')
elif not existing and (os.path.lexists(os.path.join(identity,'node-id.json')) or os.path.lexists(os.path.join(identity,'cluster','identity.json'))): raise RuntimeError('an unrelated PAIR identity appeared before activation')
if os.stat(vendor).st_mode&0o022:
 if 'upgrade' in h:
  if os.path.basename(h['upgrade']['bundle'])!='bin': raise RuntimeError('permission repair requires a verified managed PAIR bundle')
  receiver_harden(vendor,os.path.dirname(h['upgrade']['bundle']),h['upgrade']['manifestSha256'])
 elif existing: receiver_harden(vendor,final,h['manifestSha256'],marker)
 else: raise RuntimeError('existing vendor permissions require a verified PAIR installation before repair')
checked=receiver_directory(root); os.close(checked)
if 'upgrade' in h and os.path.lexists(stage):
 if not marked(stage): raise RuntimeError('partial upgrade staging has different ownership')
 contained(stage); shutil.rmtree(stage)
if os.path.lexists(stage): raise RuntimeError('prior staging requires explicit owned cleanup before retry')
if shutil.disk_usage(root).free<size+67108864: raise RuntimeError('insufficient storage for the verified package transfer')
os.mkdir(stage,0o700)
with open(os.path.join(stage,'.pair-onboarding.json'),'x') as f: json.dump(marker,f)
archive=os.path.join(stage,'package.tar.gz'); hash=hashlib.sha256(); remaining=size
with open(archive,'xb') as f:
 while remaining:
  chunk=sys.stdin.buffer.read(min(1048576,remaining))
  if not chunk: raise RuntimeError('package transfer was incomplete')
  if shutil.disk_usage(stage).free<len(chunk)+67108864: raise RuntimeError('available transfer storage changed')
  remaining-=len(chunk); hash.update(chunk); f.write(chunk)
 f.flush(); os.fsync(f.fileno())
if sys.stdin.buffer.read(1) or hash.hexdigest()!=digest: raise RuntimeError('transferred package identity did not match approval')
names={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','nvpair-proxy'}
machine={'arm64':183,'amd64':62}.get(h['arch'])
if machine is None: raise RuntimeError('unsupported native architecture')
payload=os.path.join(stage,'payload'); os.mkdir(payload,0o700)
seen=set(); binaries=set(); total=0
with tarfile.open(archive,'r:gz') as tf:
 for count,member in enumerate(tf):
  if count>=4096: raise RuntimeError('archive entry limit exceeded')
  name=member.name.rstrip('/'); parts=name.split('/')
  if not name or '\\' in name or name.startswith('/') or any(p in ('','..','.') for p in parts) or parts[0]!=h['archiveRoot'] or name in seen: raise RuntimeError('unsafe or duplicate package path')
  seen.add(name)
  if not (member.isdir() or member.isfile()) or member.mode & 0o6000: raise RuntimeError('package special entries are not admitted')
  if member.size<0 or member.size>268435456: raise RuntimeError('component size limit exceeded')
  total+=member.size
  if total>2147483648: raise RuntimeError('expanded package limit exceeded')
  destination=os.path.join(payload,*parts)
  if member.isdir(): os.makedirs(destination,mode=0o700,exist_ok=True); continue
  if shutil.disk_usage(stage).free < member.size+67108864: raise RuntimeError('insufficient storage for package extraction')
  os.makedirs(os.path.dirname(destination),mode=0o700,exist_ok=True)
  source=tf.extractfile(member)
  with open(destination,'xb') as target:
   shutil.copyfileobj(source,target,1048576)
  if len(parts)>=2 and parts[1]=='bin':
   if len(parts)!=3: raise RuntimeError('unexpected nested binary content')
   if parts[2]!='manifest.json':
    if parts[2] not in names or not(member.mode&0o111): raise RuntimeError('unexpected product binary')
    with open(destination,'rb') as f: prefix=f.read(20)
    if len(prefix)!=20 or prefix[:4]!=b'\x7fELF' or prefix[4:6]!=b'\x02\x01' or int.from_bytes(prefix[18:20],'little')!=machine: raise RuntimeError('native component architecture differs from approval')
    binaries.add(parts[2]); os.chmod(destination,0o755)
   else: os.chmod(destination,0o644)
  else: os.chmod(destination,0o644)
if binaries!=names: raise RuntimeError('the complete application binary set is required')
unpacked=os.path.join(payload,h['archiveRoot'])
manifest=os.path.join(unpacked,'bin','manifest.json')
if not os.path.isfile(manifest): raise RuntimeError('the verified build manifest is required for product-owned replication')
with open(manifest,'rb') as f: manifest_bytes=f.read(131073)
if len(manifest_bytes)>131072 or hashlib.sha256(manifest_bytes).hexdigest()!=h['manifestSha256']: raise RuntimeError('approved build manifest changed during transfer')
with open(os.path.join(unpacked,'.pair-onboarding.json'),'x') as f: json.dump(marker,f)
if not existing:
 if os.path.lexists(final): raise RuntimeError('bundle destination changed before activation')
 contained(final); contained(unpacked)
 rename=ctypes.CDLL(None,use_errno=True).renameat2
 rename.argtypes=[ctypes.c_int,ctypes.c_char_p,ctypes.c_int,ctypes.c_char_p,ctypes.c_uint]; rename.restype=ctypes.c_int
 if rename(-100,os.fsencode(unpacked),-100,os.fsencode(final),1)!=0: raise OSError(ctypes.get_errno(),'atomic no-replace activation was refused')
else:
 for name in names|{'manifest.json'}:
  old=os.path.join(final,'bin',name); new=os.path.join(unpacked,'bin',name)
  if os.path.islink(old) or not os.path.isfile(old): raise RuntimeError('retained component identity changed')
  def filehash(p):
   result=hashlib.sha256()
   with open(p,'rb') as f:
    for chunk in iter(lambda:f.read(1048576),b''): result.update(chunk)
   return result.digest()
  if filehash(old)!=filehash(new): raise RuntimeError('retained bundle differs from the verified package')
if not marked(final): raise RuntimeError('activated bundle ownership could not be verified')
if existing and os.stat(final).st_mode&0o022: receiver_harden(final,final,h['manifestSha256'],marker)
checked=receiver_directory(os.path.join(final,'bin')); os.close(checked)
print(json.dumps(dict(stagePath=stage,bundlePath=final,tuiPath=os.path.join(final,'bin','nvpair-tui'),artifactSha256=digest,manifestSha256=h['manifestSha256'],startupLifetime=h['startupLifetime'],recoverable=True,installed=True,serviceInstalled=False,serviceStarted=False,cleanupConfirmed=False)))
`

func onboardingInstallPaths(info onboardingPlatformInfo, pkg onboardingPackage, operationID string) (onboardingInstallReceipt, error) {
	if !onboardingID.MatchString(operationID) || !onboardingSHA.MatchString(pkg.source.SHA256) || info.UID <= 0 || !path.IsAbs(info.Home) || path.Clean(info.Home) == "/" {
		return onboardingInstallReceipt{}, errors.New("invalid approved installation identity")
	}
	root := path.Join(info.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router")
	digest := strings.ToLower(pkg.source.SHA256)
	bundle := path.Join(root, "bundles", digest)
	return onboardingInstallReceipt{StagePath: path.Join(root, ".onboarding", operationID), BundlePath: bundle, TUIPath: path.Join(bundle, "bin", "nvpair-tui"), ArtifactSHA256: digest}, nil
}

func installOnboardingTarget(ctx context.Context, client *onboardingSSH, info onboardingPlatformInfo, pkg onboardingPackage, operationID, startupLifetime string, progress func(string, string)) (onboardingInstallReceipt, error) {
	return installOnboardingWithRunner(ctx, client, info, pkg, operationID, startupLifetime, progress)
}

func installOnboardingWithRunner(ctx context.Context, client onboardingInstallRunner, info onboardingPlatformInfo, pkg onboardingPackage, operationID, startupLifetime string, progress func(string, string)) (onboardingInstallReceipt, error) {
	receipt, err := stageOnboardingWithRunner(ctx, client, info, pkg, operationID, startupLifetime, nil, progress)
	if err != nil {
		return receipt, err
	}
	if progress != nil {
		progress("starting", "Installing the owned background parent")
	}
	if err := runOnboardingService(ctx, client, receipt, "install", startupLifetime); err != nil {
		return receipt, err
	}
	receipt.ServiceInstalled = true
	if err := runOnboardingService(ctx, client, receipt, "start", startupLifetime); err != nil {
		return receipt, err
	}
	receipt.ServiceStarted = true
	if progress != nil {
		progress("starting", "Verifying the owned unpaired node identity")
	}
	recovery, err := captureOnboardingRecovery(ctx, client, receipt)
	if err != nil {
		return receipt, err
	}
	receipt.NodeID = recovery
	return receipt, nil
}

func stageOnboardingWithRunner(ctx context.Context, client onboardingInstallRunner, info onboardingPlatformInfo, pkg onboardingPackage, operationID, startupLifetime string, existing *onboardingExistingInstallation, progress func(string, string)) (onboardingInstallReceipt, error) {
	if _, _, err := verifyOnboardingArchive(pkg.file, pkg.source.onboardingArtifact); err != nil {
		return onboardingInstallReceipt{}, err
	}
	receipt, err := onboardingInstallPaths(info, pkg, operationID)
	if err != nil {
		return receipt, err
	}
	if startupLifetime != "persistent" && startupLifetime != "session" {
		return receipt, errors.New("an explicit reviewed startup lifetime is required")
	}
	receipt.CleanupConfirmed = true // No remote effects before the verified stream begins.
	receipt.StartupLifetime = startupLifetime
	receipt.ManifestSHA256, err = onboardingPackageManifestHash(pkg)
	if err != nil {
		return receipt, err
	}
	file, err := os.Open(pkg.file)
	if err != nil {
		return receipt, errors.New("verified PAIR package became unavailable")
	}
	defer file.Close()
	fields := map[string]any{"operationId": operationID, "sha256": receipt.ArtifactSHA256, "manifestSha256": receipt.ManifestSHA256, "startupLifetime": startupLifetime, "bytes": pkg.bytes, "archiveRoot": pkg.archiveRoot, "arch": pkg.source.Arch, "uid": info.UID, "home": info.Home}
	if existing != nil {
		if err := validateOnboardingExisting(*existing, info); err != nil {
			return receipt, err
		}
		// The peer can re-inspect every ordinary installation field, but legacy
		// Debian disposition is controller-only evidence added after that same
		// unprivileged inspection. Do not make the fixed receiver compare fields
		// it cannot reproduce; the retained plan keeps the full reviewed value.
		fields["upgrade"] = onboardingUpgradeTransferDescriptor(*existing)
	}
	header := onboardingMarshal(fields)
	header = append(header, '\n')
	if progress != nil {
		progress("installing", "Transferring and verifying the approved PAIR bundle")
	}
	transferCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	receipt.CleanupConfirmed = false
	body, err := client.run(transferCtx, onboardingPython(onboardingReceiveScript), io.MultiReader(bytes.NewReader(header), io.LimitReader(file, pkg.bytes+1)))
	cancel()
	if err != nil {
		return receipt, errors.New("PAIR transfer or verified activation did not complete; owned staging was retained")
	}
	var observed onboardingInstallReceipt
	if onboardingDecode(body, &observed) != nil || observed.StagePath != receipt.StagePath || observed.BundlePath != receipt.BundlePath || observed.TUIPath != receipt.TUIPath || observed.ArtifactSHA256 != receipt.ArtifactSHA256 || observed.ManifestSHA256 != receipt.ManifestSHA256 || observed.StartupLifetime != startupLifetime || !observed.Installed || !observed.Recoverable {
		return receipt, errors.New("activated bundle did not match the approved installation receipt")
	}
	receipt.Installed = true
	receipt.Recoverable = true
	return receipt, nil
}

func onboardingUpgradeTransferDescriptor(existing onboardingExistingInstallation) onboardingExistingInstallation {
	remote := existing
	remote.LegacyPackageStatus, remote.LegacyPackage = "", nil
	return remote
}

func onboardingPackageManifestHash(pkg onboardingPackage) (string, error) {
	file, err := os.Open(pkg.file)
	if err != nil {
		return "", errors.New("approved package is unavailable")
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return "", errors.New("approved package is not a gzip archive")
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var total int64
	for count := 0; count < 4096; count++ {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", errors.New("approved package manifest could not be read")
		}
		if header.Size < 0 || header.Size > 256<<20 {
			return "", errors.New("approved package component exceeds limit")
		}
		total += header.Size
		if total > 2<<30 {
			return "", errors.New("approved package expanded size exceeds limit")
		}
		if header.Name == pkg.archiveRoot+"/bin/manifest.json" && header.Typeflag == tar.TypeReg {
			data, err := io.ReadAll(io.LimitReader(reader, (128<<10)+1))
			if err != nil || len(data) > 128<<10 {
				return "", errors.New("approved manifest exceeds limit")
			}
			sum := sha256.Sum256(data)
			return hex.EncodeToString(sum[:]), nil
		}
	}
	return "", errors.New("approved package lacks its build manifest")
}

func runOnboardingService(ctx context.Context, client onboardingInstallRunner, receipt onboardingInstallReceipt, action, lifetime string) error {
	if (action != "install" && action != "start" && action != "stop" && action != "uninstall") || (lifetime != "persistent" && lifetime != "session") {
		return errors.New("unsupported fixed product service action")
	}
	budget := 35 * time.Second
	if action == "stop" || action == "uninstall" {
		budget = 275 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingServiceScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt, "action": action, "lifetime": lifetime})))
	if err != nil {
		return errors.New("owned PAIR background service action was not confirmed")
	}
	var result struct {
		Unit              string `json:"unit"`
		Operation         string `json:"operation"`
		State             string `json:"state"`
		Persistence       string `json:"persistence"`
		RequestedLifetime string `json:"requestedLifetime"`
		EffectiveLifetime string `json:"effectiveLifetime"`
	}
	if onboardingDecode(body, &result) != nil || result.Unit != "nvidia-pair-headless.service" || result.Operation != action || result.RequestedLifetime != lifetime {
		return errors.New("headless service acknowledgement did not match the request")
	}
	expected := map[string]string{"install": "installed", "start": "started", "stop": "stopped", "uninstall": "not-installed"}[action]
	if result.State != expected || (lifetime == "persistent" && (action == "install" || action == "start") && result.EffectiveLifetime != "persistent") {
		return errors.New("requested background startup lifetime was not confirmed")
	}
	if (action == "install" || action == "start") && result.EffectiveLifetime != "persistent" && result.EffectiveLifetime != "session" {
		return errors.New("effective startup lifetime is unknown")
	}
	return nil
}

// Fixed lifecycle helpers derive every filesystem target from the authenticated
// native account and compare it to the nonsecret operation receipt before acting.
const onboardingOwnedScript = `import hashlib,json,os,pwd,selectors,shutil,signal,ssl,stat,subprocess,sys,tempfile,time,unicodedata
raw=sys.stdin.buffer.read(16385)
if len(raw)>16384: raise RuntimeError('product request limit exceeded')
h=json.loads(raw); r=h['receipt']; home=os.path.normpath(pwd.getpwuid(os.geteuid()).pw_dir)
root=os.path.join(home,'.local','share','Nvidia Corporation','Personal AI Router')
op=os.path.basename(r['stagePath']); digest=r['artifactSha256']
if len(op)!=32 or any(c not in '0123456789abcdef' for c in op) or len(digest)!=64 or any(c not in '0123456789abcdef' for c in digest): raise RuntimeError('invalid owned operation identity')
stage=os.path.join(root,'.onboarding',op); bundle=os.path.join(root,'bundles',digest); tui=os.path.join(bundle,'bin','nvpair-tui')
if r['stagePath']!=stage or r['bundlePath']!=bundle or r['tuiPath']!=tui: raise RuntimeError('receipt targets differ from this native account')
for p in [root,stage,bundle]:
 if os.path.commonpath([os.path.realpath(home),os.path.realpath(p)])!=os.path.realpath(home) or os.path.islink(p): raise RuntimeError('owned path containment changed')
 if p!=root and os.path.commonpath([os.path.realpath(root),os.path.realpath(p)])!=os.path.realpath(root): raise RuntimeError('owned path escaped product storage')
if r.get('startupLifetime') not in ('session','persistent') or len(r.get('manifestSha256',''))!=64: raise RuntimeError('approved lifetime or manifest binding is missing')
marker={'owner':'nvidia-pair-onboarding-v1','operationId':op,'sha256':digest,'manifestSha256':r['manifestSha256'],'startupLifetime':r['startupLifetime']}
def marked(p):
 if os.path.islink(p) or os.path.islink(os.path.join(p,'.pair-onboarding.json')): return False
 try:
  if os.stat(p).st_uid!=os.geteuid() or os.stat(os.path.join(p,'.pair-onboarding.json')).st_uid!=os.geteuid(): return False
  with open(os.path.join(p,'.pair-onboarding.json')) as f: raw=f.read(8193)
  return len(raw)<=8192 and json.loads(raw)==marker
 except (OSError,ValueError): return False
env=os.environ.copy(); runtime='/run/user/'+str(os.geteuid())
env['XDG_RUNTIME_DIR']=runtime; env['DBUS_SESSION_BUS_ADDRESS']='unix:path='+runtime+'/bus'; env['XDG_CONFIG_HOME']=os.path.join(home,'.config')
def verify_tui():
 if not marked(bundle): raise RuntimeError('installed bundle is not owned by this operation')
 if os.path.islink(os.path.join(bundle,'bin')) or os.path.islink(os.path.join(bundle,'bin','manifest.json')): raise RuntimeError('installed manifest path changed')
 with open(os.path.join(bundle,'bin','manifest.json'),'rb') as f: raw=f.read(131073)
 if len(raw)>131072 or hashlib.sha256(raw).hexdigest()!=r['manifestSha256']: raise RuntimeError('approved installed manifest changed')
 manifest=json.loads(raw)
 names={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','nvpair-proxy'}
 entries=manifest.get('files',[])
 if len(entries)!=len(names) or {x.get('fileName') for x in entries}!=names: raise RuntimeError('installed binary inventory changed')
 for entry in entries:
  filename=os.path.join(bundle,'bin',entry['fileName'])
  if os.path.islink(filename) or not os.path.isfile(filename) or os.path.getsize(filename)!=entry['size']: raise RuntimeError('installed component shape changed')
  hash=hashlib.sha256()
  with open(filename,'rb') as f:
   for chunk in iter(lambda:f.read(1048576),b''): hash.update(chunk)
  if hash.hexdigest().lower()!=entry['sha256'].lower(): raise RuntimeError('installed component changed')
 checkpoint=os.path.join(bundle,'.pair-onboarding-state.json')
 if os.path.lexists(checkpoint):
  if os.path.islink(checkpoint): raise RuntimeError('owned identity checkpoint changed')
  with open(checkpoint) as f: saved=json.loads(f.read(8193))
  if saved.get('owner')!=marker or not saved.get('nodeId') or not saved.get('certFingerprint'): raise RuntimeError('owned identity checkpoint is invalid')
  identitypath=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router','cluster','identity.json')
  certpath=os.path.join(os.path.dirname(identitypath),'node.crt')
  if os.path.islink(identitypath) or os.path.islink(certpath): raise RuntimeError('owned identity path changed')
  with open(identitypath) as f: current=json.loads(f.read(8193))
  with open(certpath) as f: certificate=f.read(65537)
  if current.get('node_uuid')!=saved['nodeId'] or len(certificate)>65536 or 'sha256:'+hashlib.sha256(ssl.PEM_cert_to_DER_cert(certificate)).hexdigest()!=saved['certFingerprint']: raise RuntimeError('owned public identity changed; do not mint replacement keys during retry')
def product(argv,input=None,limit=1048576,raw_output=False,timeout=30):
 p=subprocess.Popen(argv,stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,stdout=subprocess.PIPE,stderr=subprocess.DEVNULL,env=env)
 selector=selectors.DefaultSelector(); selector.register(p.stdout,selectors.EVENT_READ); data=bytearray(); deadline=time.monotonic()+timeout
 def interrupted(signum,frame): raise RuntimeError('product action interrupted')
 old=signal.signal(signal.SIGTERM,interrupted)
 try:
  if input is not None: p.stdin.write(input); p.stdin.close()
  eof=False
  while not eof:
   if time.monotonic()>=deadline: raise RuntimeError('product action deadline expired')
   for key,event in selector.select(min(0.2,deadline-time.monotonic())):
    chunk=os.read(key.fd,min(65536,limit+1-len(data)))
    if not chunk: eof=True; break
    data.extend(chunk)
    if len(data)>limit: raise RuntimeError('product response exceeds limit')
  if p.wait(timeout=max(0.01,deadline-time.monotonic()))!=0: raise RuntimeError('fixed product action was not confirmed')
  return data.decode('utf-8') if raw_output else json.loads(data)
 finally:
  signal.signal(signal.SIGTERM,old); selector.close()
  if p.poll() is None: p.kill(); p.wait(timeout=5)
`

const onboardingServiceScript = `
if set(h)!={'receipt','action','lifetime'} or h['action'] not in ('install','start','stop','uninstall') or h['lifetime'] not in ('session','persistent'): raise RuntimeError('unsupported product lifecycle request')
verify_tui()
result=product([tui,'--headless-service',h['action'],'--startup-lifetime',h['lifetime']],limit=16384,timeout=260 if h['action'] in ('stop','uninstall') else 30)
print(json.dumps(result))
`

// This compatibility cleanup is only for the exact never-started legacy unit
// whose WorkingDirectory used argv quotes. Normal lifecycle stays TUI-owned.
const onboardingMalformedUnitRecovery = `
def recover_legacy_inactive_unit():
 unit='nvidia-pair-headless.service'; unitdir=os.path.join(home,'.config','systemd','user'); unitpath=os.path.join(unitdir,unit)
 archive=os.path.join(bundle,'.pair-onboarding-retired-unit.json')
 linkpath=os.path.join(unitdir,'default.target.wants',unit)
 def owned_ancestry(directory):
  directory=os.path.abspath(directory)
  if os.path.commonpath([home,directory])!=home or os.path.realpath(directory)!=directory: raise RuntimeError('legacy unit ancestry is redirected outside its approved location')
  current=home
  for part in ['']+os.path.relpath(directory,home).split(os.sep):
   if part and part!='.': current=os.path.join(current,part)
   if not os.path.lexists(current): continue
   st=os.lstat(current)
   if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.geteuid(): raise RuntimeError('legacy unit ancestry is not an owned directory')
 def private_file(p):
  st=os.lstat(p)
  if not stat.S_ISREG(st.st_mode) or st.st_uid!=os.geteuid() or st.st_mode&0o022 or st.st_size>16384: raise RuntimeError('legacy unit recovery file ownership is unknown')
  with open(p,'rb') as f: data=f.read(16385)
  if len(data)>16384: raise RuntimeError('legacy unit recovery file is oversized')
  return data,st
 def quote(value):
  if any(unicodedata.category(c)=='Cc' for c in value): raise RuntimeError('legacy unit path contains controls')
  return '"'+value.replace('\\','\\\\').replace('"','\\"').replace('%','%%')+'"'
 directory=os.path.dirname(tui); broker=os.path.join(directory,'nvpair-ui-broker')
 legacy='# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart='+quote(tui.replace('$','$$'))+' --headless --broker-path '+quote(broker.replace('$','$$'))+'\nWorkingDirectory='+quote(directory)+'\nEnvironment='+quote('XDG_CONFIG_HOME='+os.path.join(home,'.config'))+'\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=25\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n'
 present=os.path.lexists(unitpath); saved=None; unitstat=None
 if os.path.lexists(archive):
  raw,_=private_file(archive); saved=json.loads(raw)
  if saved!={'owner':marker,'unitPath':unitpath,'unitText':legacy}: raise RuntimeError('retired unit does not match this original operation')
 if present:
  body,unitstat=private_file(unitpath)
  if body!=legacy.encode():
   if saved: raise RuntimeError('legacy unit changed after recovery checkpoint')
   return False
 elif not saved: return False
 owned_ancestry(unitdir)
 # The installer must not erase or stop a node that ever reached identity or
 # admission. Such recovery belongs to the normal product identity owner.
 if r.get('nodeId') or os.path.lexists(os.path.join(bundle,'.pair-onboarding-state.json')): raise RuntimeError('legacy recovery requires a never-started identity')
 identity=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router')
 for relative in ['node-id.json','cluster/identity.json','cluster/node.key','cluster/node.crt','cluster/admission.json','cluster/members.json']:
  if os.path.lexists(os.path.join(identity,relative)): raise RuntimeError('identity or membership requires normal product recovery')
 for p in [unitdir,os.path.dirname(linkpath)]:
  if os.path.lexists(p):
   st=os.lstat(p)
   if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.geteuid() or st.st_mode&0o022: raise RuntimeError('legacy unit directory is not account-owned')
 def manager():
  fields=['LoadState','ActiveState','SubState','MainPID','ExecMainPID','FragmentPath','DropInPaths','Job']
  output=product(['/usr/bin/systemctl','--user','--no-ask-password','--no-pager','show',unit]+['--property='+field for field in fields],limit=16384,raw_output=True)
  values={}
  for line in output.splitlines():
   key,sep,value=line.partition('=')
   if not sep or key in values: raise RuntimeError('legacy unit manager response is ambiguous')
   values[key]=value
  if set(values)!=set(fields) or values['ActiveState']!='inactive' or values['SubState']!='dead' or values['MainPID']!='0' or values['ExecMainPID']!='0' or values['DropInPaths'] or values['Job'] not in ('','0'): raise RuntimeError('legacy unit has active or unconfirmed manager state')
  return values
 def expected_state(values,allow_absent):
  if allow_absent and values['LoadState']=='not-found' and values['FragmentPath']=='': return
  if values['LoadState']!='bad-setting' or values['FragmentPath']!=unitpath: raise RuntimeError('unit is not the exact inactive malformed legacy instance')
 def checked_link():
  if not os.path.lexists(linkpath): return None
  st=os.lstat(linkpath)
  if not stat.S_ISLNK(st.st_mode) or st.st_uid!=os.geteuid() or os.path.realpath(linkpath)!=unitpath: raise RuntimeError('legacy enablement link is foreign or ambiguous')
  return st
 # Inspect targets as well as names: a differently named alias can enable
 # this unit too. Never follow a dependency-directory symlink during recovery.
 def inspect_enablement():
  directories=[unitdir]; count=0
  for directory in directories:
   owned_ancestry(directory)
   if not os.path.isdir(directory): continue
   with os.scandir(directory) as entries:
    for entry in entries:
     count+=1
     if count>256: raise RuntimeError('unit directory exceeds recovery inspection bound')
     if entry.is_symlink() and os.path.realpath(entry.path)==unitpath and entry.path!=linkpath: raise RuntimeError('legacy unit has an additional unreviewed alias')
     if entry.name==unit and entry.path not in (unitpath,linkpath): raise RuntimeError('legacy unit has additional unreviewed enablement')
     if directory==unitdir and entry.name.endswith(('.wants','.requires')):
      if entry.is_symlink() or not entry.is_dir(follow_symlinks=False): raise RuntimeError('dependency directory ownership is unknown')
      directories.append(entry.path)
 inspect_enablement()
 expected_state(manager(),not present)
 if present:
  status=product([tui,'--headless-service','status','--startup-lifetime',r['startupLifetime']],limit=16384)
  if status.get('unit')!=unit or status.get('state')!='inactive': raise RuntimeError('old product owner did not confirm complete unit ownership')
 checked_link()
 if not saved:
  saved={'owner':marker,'unitPath':unitpath,'unitText':legacy}
  fd,temporary=tempfile.mkstemp(prefix='.pair-retired-unit-',dir=bundle)
  try:
   with os.fdopen(fd,'w') as f: json.dump(saved,f); f.flush(); os.fsync(f.fileno())
   os.link(temporary,archive)
  finally: os.unlink(temporary)
 verify_tui(); owned_ancestry(unitdir); inspect_enablement(); expected_state(manager(),not present)
 linkstat=checked_link()
 if present:
  body,current=private_file(unitpath)
  if body!=legacy.encode() or (current.st_dev,current.st_ino)!=(unitstat.st_dev,unitstat.st_ino): raise RuntimeError('unit changed before exact recovery')
 if linkstat:
  current=checked_link()
  if current is None or (current.st_dev,current.st_ino)!=(linkstat.st_dev,linkstat.st_ino): raise RuntimeError('enablement link changed before removal')
  os.unlink(linkpath)
 if present:
  body,current=private_file(unitpath)
  if body!=legacy.encode() or (current.st_dev,current.st_ino)!=(unitstat.st_dev,unitstat.st_ino): raise RuntimeError('unit changed after enablement removal')
  os.unlink(unitpath)
 product(['/usr/bin/systemctl','--user','--no-ask-password','--no-pager','daemon-reload'],limit=16384,raw_output=True)
 values=manager()
 if values['LoadState']!='not-found' or values['FragmentPath'] or os.path.lexists(unitpath) or os.path.lexists(linkpath): raise RuntimeError('legacy unit removal is not confirmed by the user manager')
 return True
`

const onboardingCleanupScript = onboardingMalformedUnitRecovery + `
if set(h)!={'receipt'}: raise RuntimeError('invalid owned cleanup request')
if os.path.lexists(stage):
 if not marked(stage): raise RuntimeError('staging ownership is unknown; nothing was removed')
 shutil.rmtree(stage)
if not os.path.lexists(bundle) and (r.get('installed') or r.get('serviceInstalled') or r.get('serviceStarted')): raise RuntimeError('missing bundle does not prove that its service stopped')
if os.path.lexists(bundle):
 verify_tui()
 admission=os.path.join(home,'.config','Nvidia Corporation','Personal AI Router','cluster','admission.json')
 if os.path.exists(admission):
  with open(admission) as f: state=json.load(f)
  if state.get('clusterId'): raise RuntimeError('paired member retained; use normal membership operations')
 if not recover_legacy_inactive_unit():
  result=product([tui,'--headless-service','uninstall','--startup-lifetime',r['startupLifetime']],limit=16384,timeout=260)
  if result.get('unit')!='nvidia-pair-headless.service' or result.get('state')!='not-installed': raise RuntimeError('owned service cleanup was not confirmed')
print(json.dumps({'cleanupConfirmed':True,'bundleRetained':os.path.exists(bundle)}))
`

const onboardingRecoveryScript = `
if set(h)!={'receipt'}: raise RuntimeError('invalid owned recovery request')
verify_tui(); deadline=time.monotonic()+25; identity=None
while time.monotonic()<deadline:
 answer=product([tui,'--control'],json.dumps({'method':'cluster:get-node-id'}).encode())
 if 'result' in answer:
  identity=answer['result']; break
 if answer.get('error',{}).get('code')!='not-ready': raise RuntimeError('owned node identity could not be confirmed')
 time.sleep(0.25)
if not identity or not identity.get('nodeUuid') or not identity.get('certFingerprint'): raise RuntimeError('owned node identity did not become available')
if identity.get('clusterId'): raise RuntimeError('membership already committed; reconcile through normal cluster operations')
if r.get('nodeId') and r['nodeId']!=identity['nodeUuid']: raise RuntimeError('previously observed node identity changed')
state={'owner':marker,'nodeId':identity['nodeUuid'],'certFingerprint':identity['certFingerprint'],'stage':'installed-not-paired'}
statepath=os.path.join(bundle,'.pair-onboarding-state.json')
if os.path.lexists(statepath):
 if os.path.islink(statepath): raise RuntimeError('owned recovery state is not a regular file')
 with open(statepath) as f: old=json.loads(f.read(8193))
 if old!=state: raise RuntimeError('owned partial-install identity changed')
else:
 fd,temporary=tempfile.mkstemp(prefix='.pair-state-',dir=bundle)
 try:
  with os.fdopen(fd,'w') as f: json.dump(state,f); f.flush(); os.fsync(f.fileno())
  os.link(temporary,statepath)
 finally: os.unlink(temporary)
print(json.dumps({'nodeId':identity['nodeUuid'],'state':'installed-not-paired','recoverable':True}))
`

const onboardingResumeScript = `
if set(h)!={'receipt'}: raise RuntimeError('invalid owned resume request')
if os.path.lexists(bundle): verify_tui()
if os.path.lexists(stage):
 if not marked(stage): raise RuntimeError('partial staging ownership is unknown')
 shutil.rmtree(stage)
print(json.dumps({'resumable':True}))
`

const onboardingFinishScript = `
if set(h)!={'receipt'}: raise RuntimeError('invalid owned completion request')
verify_tui()
if os.path.lexists(stage):
 if not marked(stage): raise RuntimeError('completion staging ownership is unknown')
 shutil.rmtree(stage)
if os.path.lexists(stage): raise RuntimeError('owned staging removal was not confirmed')
print(json.dumps({'stagingCleaned':True}))
`

func finishOnboardingTarget(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt) (bool, error) {
	return finishOnboardingWithRunner(ctx, client, receipt)
}

func finishOnboardingWithRunner(ctx context.Context, client onboardingInstallRunner, receipt onboardingInstallReceipt) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingFinishScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt})))
	if err != nil {
		return false, errors.New("paired installation was retained but owned staging cleanup is unconfirmed")
	}
	var result struct {
		StagingCleaned bool `json:"stagingCleaned"`
	}
	if onboardingDecode(body, &result) != nil || !result.StagingCleaned {
		return false, errors.New("owned staging cleanup was not acknowledged")
	}
	return true, nil
}

const onboardingControlScript = `
if set(h)!={'receipt','method','params'} or h['method'] not in ('headless:status','cluster:get-node-id','nodes:get-initial','cluster:invite-node','cluster:respond-to-invite','cluster:invite-status','cluster:cancel-invite'): raise RuntimeError('unsupported private product control request')
verify_tui()
answer=product([tui,'--control'],json.dumps({'method':h['method'],'params':h['params']}).encode())
print(json.dumps(answer))
`

func runOnboardingControl(ctx context.Context, client onboardingInstallRunner, receipt onboardingInstallReceipt, method string, params any) (json.RawMessage, error) {
	switch method {
	case "headless:status", "cluster:get-node-id", "nodes:get-initial", "cluster:invite-node", "cluster:respond-to-invite", "cluster:invite-status", "cluster:cancel-invite":
	default:
		return nil, errors.New("method is not available on owned onboarding control")
	}
	input := onboardingMarshal(map[string]any{"receipt": receipt, "method": method, "params": params})
	defer clear(input)
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingControlScript), bytes.NewReader(input))
	if err != nil {
		return nil, errors.New("private product control did not return a confirmed result")
	}
	var response struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			RPCCode int    `json:"rpcCode,omitempty"`
		} `json:"error,omitempty"`
	}
	if onboardingDecode(body, &response) != nil {
		return nil, errors.New("invalid private product control response")
	}
	if response.Error != nil {
		if response.Error.Code == "not-ready" {
			return nil, errors.New("owned PAIR broker is still starting")
		}
		return nil, errors.New("private product control rejected the request or its completion is unknown")
	}
	if len(response.Result) == 0 {
		return nil, errors.New("private product control result is missing")
	}
	return response.Result, nil
}

func captureOnboardingRecovery(ctx context.Context, client onboardingInstallRunner, receipt onboardingInstallReceipt) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingRecoveryScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt})))
	if err != nil {
		return "", errors.New("installed PAIR identity is not yet confirmed as this operation's unpaired node; retain the original receipt for recovery")
	}
	var result struct {
		NodeID      string `json:"nodeId"`
		State       string `json:"state"`
		Recoverable bool   `json:"recoverable"`
	}
	if onboardingDecode(body, &result) != nil || result.NodeID == "" || result.State != "installed-not-paired" || !result.Recoverable || (receipt.NodeID != "" && receipt.NodeID != result.NodeID) {
		return "", errors.New("owned partial-install recovery identity is inconsistent")
	}
	return result.NodeID, nil
}

func resumeOnboardingTarget(ctx context.Context, client *onboardingSSH, info onboardingPlatformInfo, pkg onboardingPackage, receipt onboardingInstallReceipt, progress func(string, string)) (onboardingInstallReceipt, error) {
	return resumeOnboardingWithRunner(ctx, client, info, pkg, receipt, progress)
}

func resumeOnboardingWithRunner(ctx context.Context, client onboardingInstallRunner, info onboardingPlatformInfo, pkg onboardingPackage, receipt onboardingInstallReceipt, progress func(string, string)) (resumed onboardingInstallReceipt, resultErr error) {
	defer func() {
		if resultErr != nil {
			prior := receipt
			prior.Installed = prior.Installed || resumed.Installed
			prior.ServiceInstalled = prior.ServiceInstalled || resumed.ServiceInstalled
			prior.ServiceStarted = prior.ServiceStarted || resumed.ServiceStarted
			prior.Recoverable = prior.Recoverable || resumed.Recoverable
			if prior.NodeID == "" {
				prior.NodeID = resumed.NodeID
			}
			prior.CleanupConfirmed = false
			resumed = prior
		}
	}()
	expected, err := onboardingInstallPaths(info, pkg, path.Base(receipt.StagePath))
	if err != nil {
		return receipt, err
	}
	manifest, err := onboardingPackageManifestHash(pkg)
	if err != nil {
		return receipt, err
	}
	if expected.StagePath != receipt.StagePath || expected.BundlePath != receipt.BundlePath || expected.TUIPath != receipt.TUIPath || expected.ArtifactSHA256 != receipt.ArtifactSHA256 || manifest != receipt.ManifestSHA256 {
		return receipt, errors.New("resume no longer matches the original approved artifact and account")
	}
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingResumeScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt})))
	var result struct {
		Resumable bool `json:"resumable"`
	}
	if err != nil || onboardingDecode(body, &result) != nil || !result.Resumable {
		return receipt, errors.New("original installation could not be safely prepared for resume")
	}
	resumed, err = installOnboardingWithRunner(ctx, client, info, pkg, path.Base(receipt.StagePath), receipt.StartupLifetime, progress)
	if err == nil && receipt.NodeID != "" && resumed.NodeID != receipt.NodeID {
		return resumed, errors.New("resumed node identity differs from the original operation")
	}
	return resumed, err
}

func cleanupOnboardingTarget(ctx context.Context, client *onboardingSSH, receipt onboardingInstallReceipt) (bool, error) {
	return cleanupOnboardingWithRunner(ctx, client, receipt)
}

func cleanupOnboardingWithRunner(ctx context.Context, client onboardingInstallRunner, receipt onboardingInstallReceipt) (bool, error) {
	if receipt.CleanupConfirmed {
		return true, nil
	}
	if receipt.StagePath == "" && receipt.BundlePath == "" {
		return true, nil
	}
	if !onboardingID.MatchString(path.Base(receipt.StagePath)) || !onboardingSHA.MatchString(receipt.ArtifactSHA256) {
		return false, errors.New("owned cleanup receipt is invalid")
	}
	ctx, cancel := context.WithTimeout(ctx, 275*time.Second)
	defer cancel()
	body, err := client.run(ctx, onboardingPython(onboardingOwnedScript+onboardingCleanupScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt})))
	if err != nil {
		return false, errors.New("owned staging/service cleanup is unconfirmed; installed bundles and identities were retained")
	}
	var result struct {
		CleanupConfirmed bool `json:"cleanupConfirmed"`
		BundleRetained   bool `json:"bundleRetained"`
	}
	if onboardingDecode(body, &result) != nil || !result.CleanupConfirmed {
		return false, errors.New("owned cleanup was not acknowledged")
	}
	return true, nil
}

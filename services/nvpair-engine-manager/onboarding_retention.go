// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"path"
	"time"
)

const (
	onboardingRetentionOwner       = "nvidia-pair-bundle-retention-v2"
	onboardingLegacyRetentionOwner = "nvidia-pair-bundle-retention-v1"
	onboardingRetentionHeldLimit   = 4
)

// onboardingRetentionReview is captured during the product review and checked
// again after staging, immediately before peer downtime. HeldPruneDigests is the
// normalized current history; NextHeldPruneDigests is the exact, bounded history
// that publication will retain after the reviewed successor is running.
type onboardingRetentionReview struct {
	Owner                string   `json:"owner"`
	ActiveDigest         string   `json:"activeDigest"`
	RollbackDigest       string   `json:"rollbackDigest"`
	HeldPruneDigests     []string `json:"heldPruneDigests"`
	Operation            string   `json:"operation"`
	Generation           uint64   `json:"generation"`
	RegistrySHA256       string   `json:"registrySha256"`
	NextHeldPruneDigests []string `json:"nextHeldPruneDigests"`
	NextGeneration       uint64   `json:"nextGeneration"`
}

type onboardingRetentionRegistry struct {
	Owner            string   `json:"owner"`
	ActiveDigest     string   `json:"activeDigest"`
	RollbackDigest   string   `json:"rollbackDigest,omitempty"`
	HeldPruneDigests []string `json:"heldPruneDigests"`
	Operation        string   `json:"operation"`
	Generation       uint64   `json:"generation"`
}

func validateOnboardingRetentionReview(existing onboardingExistingInstallation) error {
	digest, managed := onboardingManagedBundleDigest(existing)
	if !managed {
		if existing.Retention != nil {
			return errors.New("unmanaged predecessor has a bundle retention claim")
		}
		return nil
	}
	r := existing.Retention
	if r == nil || r.ActiveDigest != digest || r.HeldPruneDigests == nil || r.NextHeldPruneDigests == nil || !validOnboardingHeldDigests(r.ActiveDigest, r.RollbackDigest, r.HeldPruneDigests) || r.NextGeneration != r.Generation+1 || r.NextGeneration == 0 {
		return errors.New("managed predecessor bundle retention is incomplete")
	}
	if r.Owner == "absent" {
		if r.Generation != 0 || r.Operation != "" || r.RegistrySHA256 != "" || r.RollbackDigest != "" || len(r.HeldPruneDigests) != 0 {
			return errors.New("absent bundle retention registry has conflicting history")
		}
	} else if r.Owner != onboardingLegacyRetentionOwner && r.Owner != onboardingRetentionOwner || r.Generation == 0 || !onboardingID.MatchString(r.Operation) || !onboardingSHA.MatchString(r.RegistrySHA256) {
		return errors.New("bundle retention registry identity is invalid")
	}
	want := nextOnboardingHeldDigests(r.ActiveDigest, r.RollbackDigest, r.HeldPruneDigests)
	if len(want) > onboardingRetentionHeldLimit || !equalOnboardingDigests(want, r.NextHeldPruneDigests) || !validOnboardingHeldDigests(r.ActiveDigest, "", r.NextHeldPruneDigests) {
		return errors.New("next bundle retention history exceeds or differs from the reviewed bounded transition")
	}
	return nil
}

func nextOnboardingHeldDigests(active, rollback string, held []string) []string {
	next := append([]string(nil), held...)
	if rollback != "" && rollback != active {
		found := false
		for _, item := range next {
			found = found || item == rollback
		}
		if !found {
			next = append(next, rollback)
		}
	}
	if len(next) > onboardingRetentionHeldLimit {
		next = append([]string(nil), next[len(next)-onboardingRetentionHeldLimit:]...)
	}
	return next
}

// The read-only inspector accepts the one-field v1 held state and normalizes it
// into v2. It closes over every referenced bundle and computes the next list
// before approval, so capacity can never fail for the first time after downtime.
const onboardingRetentionInspectScript = `
RETENTION_V1='nvidia-pair-bundle-retention-v1'; RETENTION_V2='nvidia-pair-bundle-retention-v2'; RETENTION_LIMIT=4
def retention_digest(value): return isinstance(value,str) and re.fullmatch('[0-9a-f]{64}',value) is not None
def retention_private(filename,home,limit=32768):
 raw,info=upgrade_private(filename,home,limit)
 if info.st_mode&0o777!=0o600 or info.st_nlink!=1: raise RuntimeError('bundle retention record ownership is invalid')
 return raw
def retention_owned_bundle(bundles,digest,home,manifest_sha=None):
 folder=os.path.join(bundles,digest)
 if os.path.dirname(folder)!=bundles or os.path.basename(folder)!=digest or not retention_digest(digest): raise RuntimeError('retained bundle path is outside its digest namespace')
 for directory in (folder,os.path.join(folder,'bin')):
  observed=os.lstat(directory)
  if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=os.geteuid() or observed.st_mode&0o022 or os.path.realpath(directory)!=directory: raise RuntimeError('retained bundle directory ownership changed')
 allowed={'.pair-onboarding.json','bin'}; statepath=os.path.join(folder,'.pair-onboarding-state.json')
 if os.path.lexists(statepath): allowed.add('.pair-onboarding-state.json')
 if set(os.listdir(folder))!=allowed: raise RuntimeError('retained bundle contains unclassified files')
 marker_raw=retention_private(os.path.join(folder,'.pair-onboarding.json'),home,8192); owned=json.loads(marker_raw)
 if set(owned)!={'owner','operationId','sha256','manifestSha256','startupLifetime'} or owned['owner']!='nvidia-pair-onboarding-v1' or owned['sha256']!=digest or not retention_digest(owned['manifestSha256']) or owned['startupLifetime'] not in ('session','persistent') or re.fullmatch('[0-9a-f]{32}',owned['operationId']) is None: raise RuntimeError('retained bundle marker is invalid')
 if manifest_sha is not None and owned['manifestSha256']!=manifest_sha: raise RuntimeError('retained bundle manifest differs from its reviewed predecessor')
 manifest_raw,_=upgrade_private(os.path.join(folder,'bin','manifest.json'),home,131072)
 if upgrade_hash(manifest_raw)!=owned['manifestSha256']: raise RuntimeError('retained bundle manifest binding changed')
 manifest=json.loads(manifest_raw); entries=manifest.get('files',[]); names={e.get('fileName') for e in entries if isinstance(e,dict)}
 legacy={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','ollama-proxy','lmstudio-proxy'}
 unified={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','nvpair-proxy'}
 if names not in (unified,legacy,legacy|{'llamacpp-proxy','vllm-proxy'}) or len(entries)!=len(names) or set(os.listdir(os.path.join(folder,'bin')))!=names|{'manifest.json'}: raise RuntimeError('retained bundle inventory has extras or omissions')
 for entry in entries:
  filename=os.path.join(folder,'bin',entry['fileName']); raw,observed=upgrade_private(filename,home,268435456)
  if observed.st_nlink!=1 or type(entry.get('size')) is not int or len(raw)!=entry['size'] or upgrade_hash(raw)!=str(entry.get('sha256','')).lower() or not observed.st_mode&0o111: raise RuntimeError('retained bundle component differs from its manifest')
 if os.path.lexists(statepath):
  state=json.loads(retention_private(statepath,home,8192))
  if set(state)!={'owner','nodeId','certFingerprint','stage'} or state['owner']!=owned or not state['nodeId'] or not state['certFingerprint'] or state['stage']!='installed-not-paired': raise RuntimeError('retained bundle identity checkpoint is invalid')
 return owned
def retention_decode(raw):
 value=json.loads(raw)
 if not isinstance(value,dict): raise RuntimeError('bundle retention registry is invalid')
 owner=value.get('owner'); required={'owner','activeDigest','operation','generation'}
 if owner==RETENTION_V1:
  optional={'rollbackDigest','pendingPruneDigest','pruneState'}
  if not required.issubset(value) or not set(value).issubset(required|optional): raise RuntimeError('legacy bundle retention registry is invalid')
  pending=value.get('pendingPruneDigest',''); state=value.get('pruneState',''); held=[] if not pending else [pending]
  if pending and state!='held' or not pending and state: raise RuntimeError('legacy bundle retention held state is invalid')
 elif owner==RETENTION_V2:
  required=required|{'rollbackDigest','heldPruneDigests'}
  if set(value)!=required: raise RuntimeError('bundle retention registry is invalid')
  held=value['heldPruneDigests']
  if not isinstance(held,list) or len(held)>RETENTION_LIMIT or len(held)!=len(set(held)): raise RuntimeError('bundle retention held history is invalid')
 else: raise RuntimeError('bundle retention owner is invalid')
 active=value.get('activeDigest'); rollback=value.get('rollbackDigest',''); operation=value.get('operation'); generation=value.get('generation')
 if not retention_digest(active) or rollback and (not retention_digest(rollback) or rollback==active) or any(not retention_digest(v) or v in (active,rollback) for v in held) or re.fullmatch('[0-9a-f]{32}',operation or '') is None or type(generation) is not int or generation<1 or generation>9007199254740990: raise RuntimeError('bundle retention registry relationships are invalid')
 return dict(owner=owner,activeDigest=active,rollbackDigest=rollback,heldPruneDigests=held,operation=operation,generation=generation)
def retention_review_value(current,registry_sha):
 observed=dict(current); observed['registrySha256']=registry_sha
 next_held=list(observed['heldPruneDigests']); displaced=observed.get('rollbackDigest','')
 if displaced and displaced not in (observed['activeDigest'],) and displaced not in next_held: next_held.append(displaced)
 if len(next_held)>RETENTION_LIMIT: next_held=next_held[-RETENTION_LIMIT:]
 observed['nextHeldPruneDigests']=next_held; observed['nextGeneration']=observed['generation']+1
 return observed
def retention_residue_action(old_raw,pending_raw,encoded,reviewed):
 if pending_raw is None: return 'none'
 if old_raw==encoded:
  if reviewed is None: raise RuntimeError('unmanaged retention exchange has no reviewed predecessor')
  displaced=retention_decode(pending_raw); observed=retention_review_value(displaced,upgrade_hash(pending_raw)); desired=retention_decode(encoded)
  if observed!=reviewed or desired['generation']<=1 or displaced['generation']+1!=desired['generation'] or displaced['activeDigest']!=desired.get('rollbackDigest'): raise RuntimeError('retention exchange residue differs from the exact reviewed predecessor')
  return 'cleanup-displaced'
 if pending_raw!=encoded: raise RuntimeError('retention publication residue differs from this operation')
 return 'exchange'
def inspect_retention(existing,expected_active=None):
 home=existing['home']; bundles=os.path.join(home,'.local','share','Nvidia Corporation','Personal AI Router','bundles')
 managed=os.path.dirname(existing['bundle']); digest=os.path.basename(managed)
 if os.path.basename(existing['bundle'])!='bin' or os.path.dirname(managed)!=bundles:
  if os.path.commonpath([bundles,os.path.abspath(existing['bundle'])])==bundles: raise RuntimeError('managed predecessor path has no digest binding')
  return None
 observed=os.lstat(bundles)
 if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=os.geteuid() or observed.st_mode&0o022 or os.path.realpath(bundles)!=bundles: raise RuntimeError('bundle registry parent is redirected or not account-owned')
 retention_owned_bundle(bundles,digest,home,existing['manifestSha256'])
 registry_path=os.path.join(bundles,'.pair-retention.json')
 residue=[v for v in os.listdir(bundles) if v.startswith('.pair-retention-') and v.endswith('.next')]
 if residue: raise RuntimeError('bundle retention publication residue requires recovery before another update')
 if not os.path.lexists(registry_path):
  current=dict(owner='absent',activeDigest=digest,rollbackDigest='',heldPruneDigests=[],operation='',generation=0,registrySha256='')
 else:
  raw=retention_private(registry_path,home); current=retention_review_value(retention_decode(raw),upgrade_hash(raw))
 if expected_active is None: expected_active=digest
 if current['activeDigest']!=expected_active: raise RuntimeError('bundle retention active digest differs from the reviewed installation')
 for item in [current.get('activeDigest',''),current.get('rollbackDigest','')]+current.get('heldPruneDigests',[]):
  if item: retention_owned_bundle(bundles,item,home)
 if 'nextHeldPruneDigests' not in current:
  current=retention_review_value(current,current['registrySha256'])
 return current
`

// The registry advances only after the successor is the running enrolled peer
// and predecessor retirement has completed. The bounded history rolls forward,
// but physical pruning remains held: bytes that leave the registry are not deleted.
const onboardingRetentionScript = onboardingRetentionInspectScript + `
if set(h)!={'receipt','existing'}: raise RuntimeError('invalid bundle retention request')
verify_tui(); existing=h['existing']; verify_upgrade_identity(existing,False)
manager=upgrade_manager(home); unit=upgrade_unit(tui,os.path.join(bundle,'bin','nvpair-ui-broker'),existing['configHome'],145).encode()
if manager['LoadState']!='loaded' or manager['ActiveState']!='active' or manager['SubState']!='running' or manager['FragmentPath']!=existing['unitPath'] or manager['DropInPaths'] or manager['Job'] not in ('','0') or upgrade_private(existing['unitPath'],home,16384)[0]!=unit: raise RuntimeError('successor service is not confirmed before retention publication')
if not re.fullmatch('[1-9][0-9]*',manager['MainPID']): raise RuntimeError('successor process is unbound')
running=os.stat('/proc/'+manager['MainPID']+'/exe'); expected=os.stat(tui)
if (running.st_dev,running.st_ino)!=(expected.st_dev,expected.st_ino): raise RuntimeError('successor process does not execute the active bundle')
bundles=os.path.join(root,'bundles'); bundles_info=os.lstat(bundles)
if not stat.S_ISDIR(bundles_info.st_mode) or bundles_info.st_uid!=os.geteuid() or bundles_info.st_mode&0o022 or os.path.realpath(bundles)!=bundles: raise RuntimeError('bundle registry parent is redirected or not account-owned')
def digest_value(value): return isinstance(value,str) and re.fullmatch('[0-9a-f]{64}',value) is not None
def private_record(filename,limit=16384):
 raw,info=upgrade_private(filename,home,limit)
 if info.st_mode&0o777!=0o600 or info.st_nlink!=1: raise RuntimeError('bundle retention record ownership is invalid')
 return raw,info
def owned_bundle(folder,digest,manifest_sha=None):
 if os.path.dirname(folder)!=bundles or os.path.basename(folder)!=digest or not digest_value(digest): raise RuntimeError('retained bundle path is outside its digest namespace')
 for directory in (folder,os.path.join(folder,'bin')):
  observed=os.lstat(directory)
  if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=os.geteuid() or observed.st_mode&0o022 or os.path.realpath(directory)!=directory: raise RuntimeError('retained bundle directory ownership changed')
 allowed={'.pair-onboarding.json','bin'}; statepath=os.path.join(folder,'.pair-onboarding-state.json')
 if os.path.lexists(statepath): allowed.add('.pair-onboarding-state.json')
 if set(os.listdir(folder))!=allowed: raise RuntimeError('retained bundle contains unclassified files')
 marker_raw,marker_info=private_record(os.path.join(folder,'.pair-onboarding.json'),8192); owned=json.loads(marker_raw)
 if set(owned)!={'owner','operationId','sha256','manifestSha256','startupLifetime'} or owned['owner']!='nvidia-pair-onboarding-v1' or owned['sha256']!=digest or not digest_value(owned['manifestSha256']) or owned['startupLifetime'] not in ('session','persistent') or re.fullmatch('[0-9a-f]{32}',owned['operationId']) is None: raise RuntimeError('retained bundle marker is invalid')
 if manifest_sha is not None and owned['manifestSha256']!=manifest_sha: raise RuntimeError('retained bundle manifest differs from its reviewed predecessor')
 manifest_raw,_=upgrade_private(os.path.join(folder,'bin','manifest.json'),home,131072)
 if upgrade_hash(manifest_raw)!=owned['manifestSha256']: raise RuntimeError('retained bundle manifest binding changed')
 manifest=json.loads(manifest_raw); entries=manifest.get('files',[]); names={e.get('fileName') for e in entries if isinstance(e,dict)}
 legacy={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','ollama-proxy','lmstudio-proxy'}
 unified={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','nvpair-proxy'}
 if names not in (unified,legacy,legacy|{'llamacpp-proxy','vllm-proxy'}) or len(entries)!=len(names) or set(os.listdir(os.path.join(folder,'bin')))!=names|{'manifest.json'}: raise RuntimeError('retained bundle inventory has extras or omissions')
 for entry in entries:
  filename=os.path.join(folder,'bin',entry['fileName']); raw,observed=upgrade_private(filename,home,268435456)
  if observed.st_nlink!=1 or type(entry.get('size')) is not int or len(raw)!=entry['size'] or upgrade_hash(raw)!=str(entry.get('sha256','')).lower() or not observed.st_mode&0o111: raise RuntimeError('retained bundle component differs from its manifest')
 if os.path.lexists(statepath):
  state_raw,_=private_record(statepath,8192); state=json.loads(state_raw)
  if set(state)!={'owner','nodeId','certFingerprint','stage'} or state['owner']!=owned or not state['nodeId'] or not state['certFingerprint'] or state['stage']!='installed-not-paired': raise RuntimeError('retained bundle identity checkpoint is invalid')
 return owned
owned_bundle(bundle,digest,r['manifestSha256'])
old_folder=os.path.dirname(existing['bundle']); old_digest=''
if os.path.basename(existing['bundle'])=='bin' and os.path.dirname(old_folder)==bundles:
 old_digest=os.path.basename(old_folder); owned_bundle(old_folder,old_digest,existing['manifestSha256'])
elif os.path.commonpath([bundles,os.path.abspath(existing['bundle'])])==bundles: raise RuntimeError('predecessor resembles a managed bundle but has no valid digest binding')
registry_path=os.path.join(bundles,'.pair-retention.json'); next_path=os.path.join(bundles,'.pair-retention-'+op+'.next')
reviewed=existing.get('retention')
if reviewed is None:
 if old_digest: raise RuntimeError('managed predecessor lacks reviewed bundle retention')
 desired={'owner':RETENTION_V2,'activeDigest':digest,'rollbackDigest':'','heldPruneDigests':[],'operation':op,'generation':1}
else:
 if not isinstance(reviewed,dict) or set(reviewed)!={'owner','activeDigest','rollbackDigest','heldPruneDigests','operation','generation','registrySha256','nextHeldPruneDigests','nextGeneration'}: raise RuntimeError('reviewed bundle retention binding is missing')
 if old_digest!=reviewed['activeDigest'] or reviewed['nextGeneration']!=reviewed['generation']+1 or len(reviewed['nextHeldPruneDigests'])>RETENTION_LIMIT or len(reviewed['nextHeldPruneDigests'])!=len(set(reviewed['nextHeldPruneDigests'])): raise RuntimeError('reviewed bundle retention transition is invalid')
 desired={'owner':RETENTION_V2,'activeDigest':digest,'rollbackDigest':old_digest,'heldPruneDigests':reviewed['nextHeldPruneDigests'],'operation':op,'generation':reviewed['nextGeneration']}
for item in [desired['activeDigest']]+([desired['rollbackDigest']] if desired.get('rollbackDigest') else [])+desired['heldPruneDigests']:
 if not retention_digest(item) or item in desired['heldPruneDigests'] and item in (desired['activeDigest'],desired.get('rollbackDigest','')): raise RuntimeError('desired bundle retention relationship is invalid')
old_raw=None; current=None
if os.path.lexists(registry_path): old_raw=retention_private(registry_path,home); current=retention_decode(old_raw)
if current is not None and current['operation']==op:
 if current!=desired: raise RuntimeError('retention retry differs from its published generation')
 migrated=False
else:
 if reviewed is None:
  if current is not None: raise RuntimeError('unmanaged predecessor has an unrelated bundle retention registry')
  migrated=True
  observed=None
 elif current is None: observed=retention_review_value(dict(owner='absent',activeDigest=old_digest,rollbackDigest='',heldPruneDigests=[],operation='',generation=0),'')
 else:
  observed=retention_review_value(current,upgrade_hash(old_raw))
 if reviewed is not None:
  if observed!=reviewed: raise RuntimeError('bundle retention state changed after review')
  migrated=reviewed['owner']!=RETENTION_V2
encoded=json.dumps(desired,sort_keys=True,separators=(',',':')).encode()
def directory_sync():
 fd=os.open(bundles,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC)
 try: os.fsync(fd)
 finally: os.close(fd)
def create_next():
 fd=os.open(next_path,os.O_WRONLY|os.O_CREAT|os.O_EXCL|os.O_NOFOLLOW|os.O_CLOEXEC,0o600)
 with os.fdopen(fd,'wb') as stream: stream.write(encoded); stream.flush(); os.fsync(stream.fileno())
pending_raw=private_record(next_path)[0] if os.path.lexists(next_path) else None
residue_action=retention_residue_action(old_raw,pending_raw,encoded,reviewed)
if residue_action=='cleanup-displaced': os.unlink(next_path); directory_sync()
if old_raw==encoded:
 if os.path.lexists(next_path): os.unlink(next_path); directory_sync()
else:
 if not os.path.lexists(next_path): create_next()
 import ctypes
 rename=ctypes.CDLL(None,use_errno=True).renameat2
 rename.argtypes=[ctypes.c_int,ctypes.c_char_p,ctypes.c_int,ctypes.c_char_p,ctypes.c_uint]; rename.restype=ctypes.c_int
 flags=1 if old_raw is None else 2
 if rename(-100,os.fsencode(next_path),-100,os.fsencode(registry_path),flags)!=0: raise OSError(ctypes.get_errno(),'atomic retention publication refused')
 if old_raw is not None:
  displaced,_=private_record(next_path)
  if displaced!=old_raw: raise RuntimeError('retention registry changed during atomic publication')
  os.unlink(next_path)
 directory_sync()
published,_=private_record(registry_path)
if published!=encoded or retention_decode(published)!=desired: raise RuntimeError('retention registry readback differs from its publication')
result=dict(desired); result['migrated']=migrated
print(json.dumps(result))
`

func recordOnboardingBundleRetention(ctx context.Context, runner onboardingInstallRunner, receipt onboardingInstallReceipt, old onboardingExistingInstallation) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	input := onboardingMarshal(map[string]any{"receipt": receipt, "existing": old})
	body, err := runner.run(ctx, onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingRetentionScript), bytes.NewReader(input))
	if err != nil {
		return errors.New("successor is retained but its account-owned bundle registry is unconfirmed")
	}
	var result struct {
		onboardingRetentionRegistry
		Migrated bool `json:"migrated"`
	}
	rollback := ""
	if digest, managed := onboardingManagedBundleDigest(old); managed {
		rollback = digest
	}
	expectedGeneration := uint64(1)
	expectedHeld := []string{}
	if old.Retention != nil {
		expectedGeneration = old.Retention.NextGeneration
		expectedHeld = old.Retention.NextHeldPruneDigests
	}
	if onboardingDecode(body, &result) != nil || result.Owner != onboardingRetentionOwner || result.ActiveDigest != receipt.ArtifactSHA256 || result.RollbackDigest != rollback || result.Operation != path.Base(receipt.StagePath) || result.Generation != expectedGeneration || !equalOnboardingDigests(result.HeldPruneDigests, expectedHeld) || !validOnboardingHeldDigests(result.ActiveDigest, result.RollbackDigest, result.HeldPruneDigests) {
		return errors.New("bundle retention acknowledgement differs from the committed upgrade")
	}
	return nil
}

func equalOnboardingDigests(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func validOnboardingHeldDigests(active, rollback string, held []string) bool {
	if !onboardingSHA.MatchString(active) || rollback != "" && (!onboardingSHA.MatchString(rollback) || rollback == active) || len(held) > onboardingRetentionHeldLimit {
		return false
	}
	seen := map[string]bool{active: true}
	if rollback != "" {
		seen[rollback] = true
	}
	for _, digest := range held {
		if !onboardingSHA.MatchString(digest) || seen[digest] {
			return false
		}
		seen[digest] = true
	}
	return true
}

func onboardingRetentionAllowsArtifact(retention *onboardingRetentionReview, digest string) bool {
	if retention == nil || !onboardingSHA.MatchString(digest) {
		return retention == nil && onboardingSHA.MatchString(digest)
	}
	if digest == retention.ActiveDigest || digest == retention.RollbackDigest {
		return false
	}
	for _, held := range retention.HeldPruneDigests {
		if digest == held {
			return false
		}
	}
	return true
}

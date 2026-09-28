// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const onboardingUpgradeUnit = "nvidia-pair-headless.service"
const onboardingUpgradePhaseBudget = 400 * time.Second

type onboardingInstalledComponent struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

// This attested descriptor never crosses the renderer boundary. Old inventory
// compatibility is separate from the strict fifteen-binary successor contract.
type onboardingExistingInstallation struct {
	NodeID              string                          `json:"nodeId"`
	ClusterID           string                          `json:"clusterId"`
	CertFingerprint     string                          `json:"certFingerprint"`
	Version             string                          `json:"version"`
	SourceFingerprint   string                          `json:"sourceFingerprint"`
	UID                 int                             `json:"uid"`
	Home                string                          `json:"home"`
	ConfigHome          string                          `json:"configHome"`
	StartupLifetime     string                          `json:"startupLifetime"`
	Unit                string                          `json:"unit"`
	UnitPath            string                          `json:"unitPath"`
	UnitSHA256          string                          `json:"unitSha256"`
	UnitBytes           string                          `json:"unitBytes"`
	Bundle              string                          `json:"bundle"`
	Executable          string                          `json:"executable"`
	Broker              string                          `json:"broker"`
	ManifestSHA256      string                          `json:"manifestSha256"`
	ProcessID           int                             `json:"processId"`
	ProcessStartTicks   string                          `json:"processStartTicks"`
	ExecutableSHA256    string                          `json:"executableSha256"`
	IdentitySHA256      string                          `json:"identitySha256"`
	NodeIdentitySHA256  string                          `json:"nodeIdentitySha256"`
	LauncherPath        string                          `json:"launcherPath"`
	LauncherPresent     bool                            `json:"launcherPresent"`
	LauncherSHA256      string                          `json:"launcherSha256"`
	LauncherBody        string                          `json:"launcherBody"`
	Components          []onboardingInstalledComponent  `json:"components"`
	Retention           *onboardingRetentionReview      `json:"retention,omitempty"`
	LegacyPackageStatus string                          `json:"legacyPackageStatus,omitempty"`
	LegacyPackage       *onboardingLegacyPackageReceipt `json:"legacyPackage,omitempty"`
}

func (d onboardingExistingInstallation) Summary() onboardingInstallationSummary {
	name, status, version, architecture, app, launcher, rollback, rollbackBytes := onboardingLegacyPackageSummaryFields(d.LegacyPackage)
	var retention *onboardingRetentionReview
	if d.Retention != nil {
		copy := *d.Retention
		copy.HeldPruneDigests = append([]string(nil), d.Retention.HeldPruneDigests...)
		copy.NextHeldPruneDigests = append([]string(nil), d.Retention.NextHeldPruneDigests...)
		retention = &copy
	}
	return onboardingInstallationSummary{NodeID: d.NodeID, ClusterID: d.ClusterID, Version: d.Version, SourceFingerprint: d.SourceFingerprint, Unit: d.Unit, Bundle: d.Bundle, Retention: retention, LegacyPackageDisposition: d.LegacyPackageStatus, LegacyPackageStatus: status, LegacyPackage: name, LegacyPackageVersion: version, LegacyPackageArchitecture: architecture, LegacyAppSHA256: app, LegacyLauncherSHA256: launcher, LegacyRollbackSHA256: rollback, LegacyRollbackBytes: rollbackBytes}
}

func validateOnboardingExisting(d onboardingExistingInstallation, info onboardingPlatformInfo) error {
	if len(onboardingMarshal(d)) > 10<<10 {
		return errors.New("existing installation descriptor exceeds the bounded helper input")
	}
	_, arch, platformErr := onboardingPlatform(info.OS, info.Arch)
	if platformErr != nil || arch == "" || !info.ExistingPAIR || !info.UserRuntime || d.UID != info.UID || d.Home != info.Home || d.UID <= 0 || d.Home == "/" || !path.IsAbs(d.Home) || path.Clean(d.Home) != d.Home {
		return errors.New("existing installation account or native platform is not bound")
	}
	if !cableIdentifier(d.NodeID, 128) || !cableIdentifier(d.ClusterID, 128) || !onboardingToken.MatchString(d.Version) || !onboardingSHA.MatchString(d.SourceFingerprint) || !onboardingSHA.MatchString(d.ManifestSHA256) || !onboardingSHA.MatchString(d.ExecutableSHA256) || !onboardingSHA.MatchString(d.IdentitySHA256) || !onboardingSHA.MatchString(d.NodeIdentitySHA256) || !strings.HasPrefix(d.CertFingerprint, "sha256:") || !onboardingSHA.MatchString(strings.TrimPrefix(d.CertFingerprint, "sha256:")) {
		return errors.New("existing installation identity or manifest is incomplete")
	}
	if d.ConfigHome != path.Join(d.Home, ".config") || d.Unit != onboardingUpgradeUnit || d.UnitPath != path.Join(d.ConfigHome, "systemd", "user", onboardingUpgradeUnit) || len(d.UnitBytes) == 0 || len(d.UnitBytes) > 16384 || d.ProcessID <= 0 || d.ProcessStartTicks == "" {
		return errors.New("existing headless unit or process is unbound")
	}
	unitHash := sha256.Sum256([]byte(d.UnitBytes))
	if hex.EncodeToString(unitHash[:]) != d.UnitSHA256 || path.Clean(d.Bundle) != d.Bundle || !strings.HasPrefix(d.Bundle, d.Home+"/") || d.Executable != path.Join(d.Bundle, "nvpair-tui") || d.Broker != path.Join(d.Bundle, "nvpair-ui-broker") {
		return errors.New("existing unit or bundle paths differ from their binding")
	}
	if d.UnitBytes != onboardingUpgradeUnitText(d.Executable, d.Broker, d.ConfigHome, 25) && d.UnitBytes != onboardingUpgradeUnitText(d.Executable, d.Broker, d.ConfigHome, 145) {
		return errors.New("existing unit is not a supported product-owned definition")
	}
	if ticks, err := strconv.ParseUint(d.ProcessStartTicks, 10, 64); err != nil || ticks == 0 {
		return errors.New("existing process generation is invalid")
	}
	if d.LauncherPath != path.Join(d.Home, ".local", "bin", "nvpair") {
		return errors.New("existing user launcher path is unbound")
	}
	if d.LauncherPresent {
		sum := sha256.Sum256([]byte(d.LauncherBody))
		if hex.EncodeToString(sum[:]) != d.LauncherSHA256 || d.LauncherBody != onboardingUpgradeLauncher(d.Executable, false) && d.LauncherBody != onboardingUpgradeLauncher(d.Executable, true) {
			return errors.New("existing user launcher is not a recognized wrapper for this peer")
		}
	} else if d.LauncherSHA256 != "" || d.LauncherBody != "" {
		return errors.New("absent launcher has conflicting ownership data")
	}
	if d.StartupLifetime != "session" && d.StartupLifetime != "persistent" || (d.StartupLifetime == "persistent") != info.Linger {
		return errors.New("existing startup lifetime is not confirmed")
	}
	want := map[string]bool{}
	for _, name := range onboardingSupportedBinaries(len(d.Components)) {
		want[name] = true
	}
	if len(want) == 0 || len(d.Components) != len(want) {
		return errors.New("old product inventory is not a supported complete unified or legacy bundle")
	}
	for _, component := range d.Components {
		if !want[component.Name] || component.Bytes <= 0 || component.Bytes > 256<<20 || !onboardingSHA.MatchString(component.SHA256) {
			return errors.New("old product inventory is incomplete or inconsistent")
		}
		delete(want, component.Name)
		if component.Name == "nvpair-tui" && component.SHA256 != d.ExecutableSHA256 {
			return errors.New("running executable differs from its installed manifest")
		}
	}
	if err := validateOnboardingRetentionReview(d); err != nil {
		return err
	}
	return validateOnboardingLegacyPackage(d.LegacyPackageStatus, d.LegacyPackage, info)
}

func onboardingUpgradeUnitText(executable, broker, config string, seconds int) string {
	quote := func(value string) string {
		return "\"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(value) + "\""
	}
	for _, value := range []string{executable, broker, config} {
		for _, r := range value {
			if unicode.IsControl(r) {
				return ""
			}
		}
	}
	directory := strings.ReplaceAll(path.Dir(broker), "%", "%%")
	if strings.HasSuffix(directory, "\\") || strings.TrimRightFunc(directory, unicode.IsSpace) != directory {
		directory += "/"
	}
	return "# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart=" + quote(strings.ReplaceAll(executable, "$", "$$")) + " --headless --broker-path " + quote(strings.ReplaceAll(broker, "$", "$$")) + "\nWorkingDirectory=" + directory + "\nEnvironment=" + quote("XDG_CONFIG_HOME="+config) + "\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=" + fmt.Sprint(seconds) + "\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n"
}

func onboardingUpgradeLauncher(executable string, systemStyle bool) string {
	if systemStyle {
		return "#!/bin/sh\n# Personal AI Router terminal UI launcher.\nexec " + onboardingQuote(executable) + " \"$@\"\n"
	}
	quoted := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "$", "\\$", "`", "\\`").Replace(executable)
	return "#!/bin/sh\n# Auto-generated by Personal AI Router. Runs the bundled terminal UI.\nexec \"" + quoted + "\" \"$@\"\n"
}

func onboardingManagedBundleDigest(d onboardingExistingInstallation) (string, bool) {
	bundle := path.Dir(d.Bundle)
	digest := path.Base(bundle)
	expected := path.Join(d.Home, ".local", "share", "Nvidia Corporation", "Personal AI Router", "bundles")
	return digest, path.Base(d.Bundle) == "bin" && path.Dir(bundle) == expected && onboardingSHA.MatchString(digest)
}

func inspectOnboardingUpgrade(ctx context.Context, runner onboardingInstallRunner, info onboardingPlatformInfo, elevationAvailable bool) (onboardingExistingInstallation, error) {
	var result onboardingExistingInstallation
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	body, err := runner.run(ctx, onboardingPython(onboardingUpgradeInspectScript), nil)
	if err != nil || onboardingDecode(body, &result) != nil {
		return result, errors.New("existing PAIR unit, bundle and enrolled identity could not be verified for upgrade")
	}
	onboardingLegacyPackageAdmission(&result, elevationAvailable)
	return result, validateOnboardingExisting(result, info)
}

func (s *onboardingService) verifyUpgradePeer(ctx context.Context, d onboardingExistingInstallation, controller, cluster string) error {
	if cluster == "" || d.ClusterID != cluster || d.NodeID == controller || s.m.mesh == nil {
		return errors.New("upgrade requires an already enrolled peer in this controller's current cluster")
	}
	s.m.mesh.Refresh()
	pin, ok := s.m.mesh.PinSHA256(d.NodeID)
	if !ok || "sha256:"+pin != d.CertFingerprint {
		return errors.New("existing peer certificate differs from current pairing trust")
	}
	roster, err := s.cluster(ctx, "nodes:get-initial", map[string]any{})
	if err != nil || !onboardingRosterMember(roster, d.NodeID) {
		return errors.New("existing installation is not a current enrolled peer")
	}
	return nil
}

func rejectDuplicateOnboardingUpgradePeers(review *onboardingReview, upgrades map[string]onboardingExistingInstallation) {
	nodes, certificates := map[string]int{}, map[string]int{}
	block := func(index int) {
		review.Targets[index].Status = "blocked"
		review.Targets[index].Reason = "Selected SSH aliases resolve to the same enrolled peer; select distinct peer identities"
		review.CanApprove = false
	}
	for i, row := range review.Targets {
		if row.Action != "upgrade" {
			continue
		}
		existing, ok := upgrades[row.CandidateID]
		if !ok {
			continue
		}
		if previous, found := nodes[existing.NodeID]; found {
			block(previous)
			block(i)
		}
		if previous, found := certificates[existing.CertFingerprint]; found {
			block(previous)
			block(i)
		}
		nodes[existing.NodeID], certificates[existing.CertFingerprint] = i, i
	}
}

// Fixed read-only inspection. It executes only the verified old TUI's existing
// identity query; the package, unit, account and process are checked first.
const onboardingUpgradeInspectPrelude = `import hashlib,json,os,platform,pwd,re,ssl,stat,subprocess,sys,unicodedata
def upgrade_hash(data): return hashlib.sha256(data).hexdigest()
def upgrade_wrapper(executable,system=False):
 if system: return "#!/bin/sh\n# Personal AI Router terminal UI launcher.\nexec '"+executable.replace("'","'\\''")+"' \"$@\"\n"
 value=executable.replace('\\','\\\\').replace('"','\\"').replace('$','\\$').replace(chr(96),'\\'+chr(96))
 return '#!/bin/sh\n# Auto-generated by Personal AI Router. Runs the bundled terminal UI.\nexec "'+value+'" "$@"\n'
def upgrade_env(home):
 env=os.environ.copy(); runtime='/run/user/'+str(os.geteuid())
 env['XDG_RUNTIME_DIR']=runtime; env['DBUS_SESSION_BUS_ADDRESS']='unix:path='+runtime+'/bus'; env['XDG_CONFIG_HOME']=os.path.join(home,'.config')
 return env
def upgrade_private_ancestry(path,home):
 if not os.path.isabs(path) or os.path.normpath(path)!=path or os.path.commonpath([home,path])!=home or os.path.realpath(path)!=path: raise RuntimeError('existing product path is redirected or outside its account')
 parent=os.path.dirname(path); ancestry=[]
 while True:
  st=os.lstat(parent)
  if not stat.S_ISDIR(st.st_mode) or st.st_uid!=os.geteuid(): raise RuntimeError('existing product ancestry is not account-owned')
  ancestry.append(st.st_mode)
  if parent==home: break
  parent=os.path.dirname(parent)
 private=False
 for mode in reversed(ancestry):
  if not private and mode&0o022: raise RuntimeError('existing product ancestry is writable outside a private account boundary')
  private=private or not mode&0o077

def upgrade_private(path,home,limit):
 upgrade_private_ancestry(path,home)
 before=os.lstat(path)
 if not stat.S_ISREG(before.st_mode) or before.st_uid!=os.geteuid() or before.st_mode&0o022 or before.st_size>limit: raise RuntimeError('existing product file ownership is unavailable')
 with open(path,'rb') as f:
  current=os.fstat(f.fileno())
  if (before.st_dev,before.st_ino)!=(current.st_dev,current.st_ino): raise RuntimeError('existing product file changed while opened')
  data=f.read(limit+1)
 if len(data)>limit or len(data)!=before.st_size: raise RuntimeError('existing product file exceeded its bound')
 return data,before
def upgrade_unit(executable,broker,config,seconds):
 for value in (executable,broker,config):
  if not value or any(unicodedata.category(c)=='Cc' for c in value): raise RuntimeError('unsupported product path')
 def quote(value): return '"'+value.replace('\\','\\\\').replace('"','\\"').replace('%','%%')+'"'
 directory=os.path.dirname(broker).replace('%','%%')
 if directory.endswith('\\') or directory.rstrip()!=directory: directory+='/'
 return '# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart='+quote(executable.replace('$','$$'))+' --headless --broker-path '+quote(broker.replace('$','$$'))+'\nWorkingDirectory='+directory+'\nEnvironment='+quote('XDG_CONFIG_HOME='+config)+'\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec='+str(seconds)+'\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n'
def upgrade_manager(home):
 fields=('LoadState','ActiveState','SubState','MainPID','FragmentPath','DropInPaths','Job')
 p=subprocess.run(['/usr/bin/systemctl','--user','--no-ask-password','--no-pager','show','nvidia-pair-headless.service']+['--property='+v for v in fields],env=upgrade_env(home),capture_output=True,timeout=15)
 if p.returncode or len(p.stdout)>16384: raise RuntimeError('existing user service state is unavailable')
 result={}
 for line in p.stdout.decode().splitlines():
  key,sep,value=line.partition('=')
  if not sep or key in result: raise RuntimeError('ambiguous user service state')
  result[key]=value
 if set(result)!=set(fields): raise RuntimeError('incomplete user service state')
 return result
def inspect_existing():
 uid=os.geteuid(); home=os.path.normpath(pwd.getpwuid(uid).pw_dir); config=os.path.join(home,'.config')
 if uid<=0 or platform.system()!='Linux': raise RuntimeError('upgrade requires a native account-owned Linux installation')
 unitpath=os.path.join(config,'systemd','user','nvidia-pair-headless.service'); unit,unitstat=upgrade_private(unitpath,home,16384); manager=upgrade_manager(home)
 if manager['LoadState']!='loaded' or manager['ActiveState']!='active' or manager['SubState']!='running' or manager['FragmentPath']!=unitpath or manager['DropInPaths'] or manager['Job'] not in ('','0'): raise RuntimeError('existing service is not a current unmodified running product owner')
 if not re.fullmatch('[1-9][0-9]*',manager['MainPID']): raise RuntimeError('existing running process is unbound')
 pid=int(manager['MainPID']); executable=os.readlink('/proc/'+str(pid)+'/exe'); bindir=os.path.dirname(executable); bundle=bindir; broker=os.path.join(bindir,'nvpair-ui-broker')
 if os.path.basename(executable)!='nvpair-tui' or unit.decode() not in (upgrade_unit(executable,broker,config,25),upgrade_unit(executable,broker,config,145)): raise RuntimeError('existing unit has unsupported or foreign ownership')
 manifestdata,_=upgrade_private(os.path.join(bindir,'manifest.json'),home,131072); manifest=json.loads(manifestdata)
 old={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','ollama-proxy','lmstudio-proxy'}
 unified={'nvpair-ui-broker','nvpair-node-scanner','nvpair-node-info','nvpair-cluster-manager','nvpair-engine-manager','nvpair-node-settings','nvpair-errors','nvpair-manual-nodes','nvpair-workload-manager','nvpair-job-scheduler','nvpair-tui','nvpair-proxy'}
 installed_names={v.get('fileName') for v in manifest.get('files',[]) if isinstance(v,dict)}
 if manifest.get('source')!='services-build' or manifest.get('platform')!='linux' or installed_names not in (unified,old,old|{'llamacpp-proxy','vllm-proxy'}) or len(manifest.get('files',[]))!=len(installed_names) or set(manifest.get('components',{}))!=installed_names: raise RuntimeError('old installed inventory is not a supported complete product bundle')
 native={'aarch64':'arm64','x86_64':'amd64'}.get(platform.machine()); architecture={'x64':'amd64'}.get(manifest.get('arch'),manifest.get('arch'))
 if native is None or architecture!=native: raise RuntimeError('old bundle is not native to this participant')
 components=[]; executable_hash=''; executable_stat=None
 for entry in manifest['files']:
  name=entry['fileName']; data,observed=upgrade_private(os.path.join(bindir,name),home,268435456)
  if type(entry.get('size')) is not int or len(data)!=entry['size'] or upgrade_hash(data)!=str(entry.get('sha256','')).lower() or not observed.st_mode&0o111 or len(data)<20 or data[:6]!=b'\x7fELF\x02\x01' or int.from_bytes(data[18:20],'little')!={'arm64':183,'amd64':62}[native]: raise RuntimeError('old component differs from the installed manifest')
  components.append(dict(name=name,sha256=upgrade_hash(data),bytes=len(data)))
  if name=='nvpair-tui': executable_hash=upgrade_hash(data); executable_stat=observed
 procstat=os.stat('/proc/'+str(pid)+'/exe')
 if (procstat.st_dev,procstat.st_ino)!=(executable_stat.st_dev,executable_stat.st_ino): raise RuntimeError('running unit executable differs from its bound component')
 with open('/proc/'+str(pid)+'/stat') as f: fields=f.read(8193)
 ticks=fields[fields.rfind(')')+2:].split()[19]
 identityroot=os.path.join(config,'Nvidia Corporation','Personal AI Router'); identitydata,_=upgrade_private(os.path.join(identityroot,'cluster','identity.json'),home,8192); certdata,_=upgrade_private(os.path.join(identityroot,'cluster','node.crt'),home,65536); nodeidentity,_=upgrade_private(os.path.join(identityroot,'node-id.json'),home,8192)
 fingerprint='sha256:'+upgrade_hash(ssl.PEM_cert_to_DER_cert(certdata.decode()))
 answer=subprocess.run([executable,'--control'],input=b'{"method":"cluster:get-node-id"}',env=upgrade_env(home),capture_output=True,timeout=25)
 if answer.returncode or len(answer.stdout)>65536: raise RuntimeError('existing product identity did not answer')
 identity=json.loads(answer.stdout).get('result',{})
 if not identity.get('clusterId') or identity.get('nodeUuid')!=json.loads(identitydata).get('node_uuid') or identity.get('certFingerprint')!=fingerprint: raise RuntimeError('existing enrolled identity is inconsistent')
 linger=subprocess.run(['/usr/bin/loginctl','show-user',str(uid),'-p','Linger','--value'],env=upgrade_env(home),capture_output=True,timeout=5)
 if linger.returncode or linger.stdout.strip() not in (b'yes',b'no'): raise RuntimeError('existing startup lifetime is unavailable')
 launcher=os.path.join(home,'.local','bin','nvpair'); launcher_present=os.path.lexists(launcher); launcher_body=''; launcher_hash=''
 if launcher_present:
  launcher_data,_=upgrade_private(launcher,home,8192); launcher_body=launcher_data.decode(); launcher_hash=upgrade_hash(launcher_data)
  if launcher_body not in (upgrade_wrapper(executable),upgrade_wrapper(executable,True)): raise RuntimeError('existing user launcher has unknown or unrelated ownership')
 if upgrade_manager(home)!=manager or upgrade_private(unitpath,home,16384)[0]!=unit or upgrade_private(os.path.join(bindir,'manifest.json'),home,131072)[0]!=manifestdata: raise RuntimeError('existing installation changed during inspection')
 return dict(nodeId=identity['nodeUuid'],clusterId=identity['clusterId'],certFingerprint=fingerprint,version=manifest.get('services',''),sourceFingerprint=manifest.get('sourceFingerprint',''),uid=uid,home=home,configHome=config,startupLifetime='persistent' if linger.stdout.strip()==b'yes' else 'session',unit='nvidia-pair-headless.service',unitPath=unitpath,unitSha256=upgrade_hash(unit),unitBytes=unit.decode(),bundle=bundle,executable=executable,broker=broker,manifestSha256=upgrade_hash(manifestdata),processId=pid,processStartTicks=ticks,executableSha256=executable_hash,identitySha256=upgrade_hash(identitydata),nodeIdentitySha256=upgrade_hash(nodeidentity),launcherPath=launcher,launcherPresent=launcher_present,launcherSha256=launcher_hash,launcherBody=launcher_body,components=sorted(components,key=lambda v:v['name']))
`

func validOnboardingUpgradePlan(plan onboardingPlan) bool {
	if plan.Review.Action == "" || plan.Review.Action == "install" {
		return plan.ExistingInstallation == nil && plan.UpgradePhase == ""
	}
	if plan.Review.Action != "upgrade" || plan.ExistingInstallation == nil || plan.Review.ExistingInstallation == nil || validateOnboardingExisting(*plan.ExistingInstallation, plan.Info) != nil || !onboardingRetentionAllowsArtifact(plan.ExistingInstallation.Retention, plan.Artifact.SHA256) || !reflect.DeepEqual(*plan.Review.ExistingInstallation, plan.ExistingInstallation.Summary()) {
		return false
	}
	switch plan.UpgradePhase {
	case "", "staging", "staged", "stopping", "stopped", "installing-unit", "installed", "starting", "started", "identity-verified", "verified", "retiring", "retired", "rolling-back", "rolled-back", "cancelled-before-stop":
		return plan.InviteID == "" && plan.InviteRequestKey == "" && len(plan.InviteHistory) == 0
	}
	return false
}

// Terminal journals written before managed-bundle retention existed remain
// readable history, but never become a recovery or retry authority.
func validHistoricalOnboardingUpgradePlan(operation onboardingOperation, target onboardingTargetState, plan onboardingPlan) bool {
	completed := operation.State == "completed" && target.Stage == "paired" && !target.CanRetry && !target.CanCancel && target.CleanupConfirmed && plan.UpgradePhase == "retired" && plan.Receipt.Installed && plan.Receipt.ServiceInstalled && plan.Receipt.ServiceStarted && plan.Receipt.CleanupConfirmed
	cancelled := operation.State == "cancelled" && target.Stage == "cancelled" && target.CleanupConfirmed && plan.UpgradePhase == "cancelled-before-stop" && !plan.Receipt.Installed && !plan.Receipt.ServiceInstalled && !plan.Receipt.ServiceStarted && plan.Receipt.CleanupConfirmed
	terminal := operation.FinishedAt > 0 && (completed || cancelled)
	if !terminal || plan.Review.Action != "upgrade" || plan.ExistingInstallation == nil || plan.ExistingInstallation.Retention != nil || plan.Review.ExistingInstallation == nil || !reflect.DeepEqual(*plan.Review.ExistingInstallation, plan.ExistingInstallation.Summary()) {
		return false
	}
	if completed && (target.NodeID != plan.ExistingInstallation.NodeID || plan.Receipt.NodeID != plan.ExistingInstallation.NodeID || plan.Receipt.ArtifactSHA256 != plan.Artifact.SHA256) {
		return false
	}
	legacy := *plan.ExistingInstallation
	digest, managed := onboardingManagedBundleDigest(legacy)
	if !managed {
		return false
	}
	legacy.Retention = &onboardingRetentionReview{Owner: "absent", ActiveDigest: digest, HeldPruneDigests: []string{}, NextHeldPruneDigests: []string{}, NextGeneration: 1}
	plan.ExistingInstallation = &legacy
	summary := legacy.Summary()
	plan.Review.ExistingInstallation = &summary
	return validOnboardingUpgradePlan(plan)
}

const onboardingUpgradeIdentityPrelude = `
def verify_upgrade_identity(existing,old_files=True):
 if existing.get('uid')!=os.geteuid() or existing.get('home')!=home or existing.get('configHome')!=os.path.join(home,'.config'): raise RuntimeError('reviewed upgrade account changed')
 identityroot=os.path.join(existing['configHome'],'Nvidia Corporation','Personal AI Router')
 identity,_=upgrade_private(os.path.join(identityroot,'cluster','identity.json'),home,8192)
 node,_=upgrade_private(os.path.join(identityroot,'node-id.json'),home,8192)
 cert,_=upgrade_private(os.path.join(identityroot,'cluster','node.crt'),home,65536)
 if upgrade_hash(identity)!=existing['identitySha256'] or upgrade_hash(node)!=existing['nodeIdentitySha256'] or 'sha256:'+upgrade_hash(ssl.PEM_cert_to_DER_cert(cert.decode()))!=existing['certFingerprint']: raise RuntimeError('reviewed identity changed; no replacement keys or profiles were created')
 if old_files:
  manifest,_=upgrade_private(os.path.join(existing['bundle'],'manifest.json'),home,131072)
  if upgrade_hash(manifest)!=existing['manifestSha256']: raise RuntimeError('retained old manifest changed')
  for entry in existing['components']:
   data,_=upgrade_private(os.path.join(existing['bundle'],entry['name']),home,268435456)
   if len(data)!=entry['bytes'] or upgrade_hash(data)!=entry['sha256']: raise RuntimeError('retained old component changed')
`

const onboardingUpgradeServiceScript = `
if set(h)!={'receipt','existing','phase'} or h['phase'] not in ('stop','install','rollback'): raise RuntimeError('unsupported product upgrade phase')
verify_tui(); existing=h['existing']; verify_upgrade_identity(existing)
request={'phase':h['phase'],'expectedUnitSha256':existing['unitSha256'],'oldExecutable':existing['executable'],'oldBroker':existing['broker'],'configHome':existing['configHome']}
owner=existing['executable'] if h['phase']=='rollback' else tui
answer=product([owner,'--headless-service','upgrade','--startup-lifetime',r['startupLifetime']],json.dumps(request).encode(),limit=16384,timeout=390)
verify_upgrade_identity(existing)
print(json.dumps(answer))
`

func runOnboardingUpgradePhase(ctx context.Context, runner onboardingInstallRunner, receipt onboardingInstallReceipt, old onboardingExistingInstallation, phase string) error {
	states := map[string]string{"stop": "stopped", "install": "installed", "rollback": "rolled-back"}
	if states[phase] == "" {
		return errors.New("unsupported product upgrade phase")
	}
	ctx, cancel := context.WithTimeout(ctx, onboardingUpgradePhaseBudget)
	defer cancel()
	input := onboardingMarshal(map[string]any{"receipt": receipt, "existing": old, "phase": phase})
	body, err := runner.run(ctx, onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeServiceScript), bytes.NewReader(input))
	if err != nil {
		return errors.New("owned peer upgrade phase is unconfirmed; retain this operation for reconciliation")
	}
	var result struct {
		Unit             string `json:"unit"`
		Operation        string `json:"operation"`
		Phase            string `json:"phase"`
		State            string `json:"state"`
		CleanupConfirmed bool   `json:"cleanupConfirmed"`
	}
	if onboardingDecode(body, &result) != nil || result.Unit != onboardingUpgradeUnit || result.Operation != "upgrade" || result.Phase != phase || result.State != states[phase] || !result.CleanupConfirmed {
		return errors.New("owned peer upgrade acknowledgement differs from its reviewed phase")
	}
	return nil
}

const onboardingUpgradeVerifyScript = `
if set(h)!={'receipt','existing'}: raise RuntimeError('invalid upgraded identity request')
verify_tui(); existing=h['existing']; verify_upgrade_identity(existing,False)
answer=product([tui,'--control'],b'{"method":"cluster:get-node-id"}')
identity=answer.get('result',{})
if identity.get('nodeUuid')!=existing['nodeId'] or identity.get('clusterId')!=existing['clusterId'] or identity.get('certFingerprint')!=existing['certFingerprint']: raise RuntimeError('successor did not retain the reviewed enrolled identity')
print(json.dumps(dict(nodeId=identity['nodeUuid'],clusterId=identity['clusterId'],certFingerprint=identity['certFingerprint'])))
`

func verifyOnboardingUpgradedIdentity(ctx context.Context, runner onboardingInstallRunner, receipt onboardingInstallReceipt, old onboardingExistingInstallation) error {
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	for {
		attempt, stop := context.WithTimeout(ctx, 35*time.Second)
		body, err := runner.run(attempt, onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeVerifyScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt, "existing": old})))
		stop()
		if err == nil {
			var result struct {
				NodeID          string `json:"nodeId"`
				ClusterID       string `json:"clusterId"`
				CertFingerprint string `json:"certFingerprint"`
			}
			if onboardingDecode(body, &result) != nil || result.NodeID != old.NodeID || result.ClusterID != old.ClusterID || result.CertFingerprint != old.CertFingerprint {
				return errors.New("successor identity differs from the reviewed enrolled peer; no replacement identity was authorized")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("successor identity or pairing did not become available within the bounded startup wait")
		case <-time.After(250 * time.Millisecond):
		}
	}
}

const onboardingUpgradeCleanStageScript = `
if set(h)!={'receipt'}: raise RuntimeError('invalid upgrade staging cleanup request')
if os.path.lexists(stage):
 if not marked(stage): raise RuntimeError('upgrade staging owner changed')
 shutil.rmtree(stage)
print(json.dumps({'stagingCleaned':True}))
`

func cleanOnboardingUpgradeStage(ctx context.Context, runner onboardingInstallRunner, receipt onboardingInstallReceipt) error {
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	body, err := runner.run(ctx, onboardingPython(onboardingOwnedScript+onboardingUpgradeCleanStageScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt})))
	var result struct {
		StagingCleaned bool `json:"stagingCleaned"`
	}
	if err != nil || onboardingDecode(body, &result) != nil || !result.StagingCleaned {
		return errors.New("owned upgrade staging cleanup is unconfirmed; installed bundles and profiles were retained")
	}
	return nil
}

// The operation journal records each intent before its remote effect. The TUI
// owns conditional unit transitions; this existing onboarding owner retains the
// old/new package and identity binding, retry position and rollback decision.
func advanceOnboardingUpgrade(ctx context.Context, runner onboardingInstallRunner, plan onboardingPlan, operationID string, checkpoint func(string, onboardingInstallReceipt) error) (onboardingInstallReceipt, string, error) {
	receipt, phase := plan.Receipt, plan.UpgradePhase
	if !validOnboardingUpgradePlan(plan) || plan.ExistingInstallation == nil {
		return receipt, phase, errors.New("reviewed upgrade plan is invalid")
	}
	old := *plan.ExistingInstallation
	record := func(next string) error {
		if err := checkpoint(next, receipt); err != nil {
			return err
		}
		phase = next
		return nil
	}
	if phase == "" || phase == "staging" || phase == "cancelled-before-stop" {
		if err := record("staging"); err != nil {
			return receipt, phase, err
		}
		pkg := onboardingPackage{source: plan.Artifact, file: plan.PackageFile, archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes}
		var err error
		receipt, err = stageOnboardingWithRunner(ctx, runner, plan.Info, pkg, operationID, plan.Review.StartupLifetime, &old, nil)
		receipt.NodeID = old.NodeID
		if err != nil {
			return receipt, phase, err
		}
		if err := record("staged"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "staged" || phase == "rolled-back" {
		observed, err := inspectOnboardingUpgrade(ctx, runner, plan.Info, old.LegacyPackage != nil)
		if phase == "rolled-back" {
			observed.ProcessID, observed.ProcessStartTicks = old.ProcessID, old.ProcessStartTicks
		}
		if err != nil || !reflect.DeepEqual(observed, old) {
			return receipt, phase, errors.New("reviewed old unit, account, process, identity or bundle changed before upgrade downtime")
		}
		if !onboardingRetentionAllowsArtifact(observed.Retention, plan.Artifact.SHA256) {
			return receipt, phase, errors.New("successor artifact now collides with active, rollback or held bundle history; no peer service was stopped")
		}
		if err := record("stopping"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "stopping" {
		if err := runOnboardingUpgradePhase(ctx, runner, receipt, old, "stop"); err != nil {
			return receipt, phase, err
		}
		receipt.ServiceStarted = false
		if err := record("stopped"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "stopped" {
		if err := record("installing-unit"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "installing-unit" {
		if err := runOnboardingUpgradePhase(ctx, runner, receipt, old, "install"); err != nil {
			return receipt, phase, err
		}
		receipt.ServiceInstalled = true
		if err := record("installed"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "installed" {
		if err := record("starting"); err != nil {
			return receipt, phase, err
		}
	}
	if phase == "starting" {
		if err := runOnboardingService(ctx, runner, receipt, "start", plan.Review.StartupLifetime); err != nil {
			return receipt, phase, err
		}
		receipt.ServiceStarted = true
		if err := record("started"); err != nil {
			return receipt, phase, err
		}
	}
	if phase != "started" && phase != "identity-verified" && phase != "verified" {
		return receipt, phase, errors.New("upgrade requires reconciliation of its retained phase")
	}
	if err := verifyOnboardingUpgradedIdentity(ctx, runner, receipt, old); err != nil {
		return receipt, phase, err
	}
	if phase != "verified" {
		if err := record("identity-verified"); err != nil {
			return receipt, phase, err
		}
	}
	return receipt, phase, nil
}

func upgradeNeedsRollback(phase string) bool {
	switch phase {
	case "stopping", "stopped", "installing-unit", "installed", "starting", "started", "identity-verified", "verified", "rolling-back":
		return true
	}
	return false
}

func rollbackOnboardingUpgrade(ctx context.Context, runner onboardingInstallRunner, old onboardingExistingInstallation, receipt onboardingInstallReceipt, checkpoint func(string, onboardingInstallReceipt) error) (onboardingInstallReceipt, error) {
	receipt.CleanupConfirmed = false
	if err := checkpoint("rolling-back", receipt); err != nil {
		return receipt, err
	}
	if err := runOnboardingUpgradePhase(ctx, runner, receipt, old, "rollback"); err != nil {
		return receipt, err
	}
	receipt.ServiceInstalled, receipt.ServiceStarted = false, false
	if err := cleanOnboardingUpgradeStage(ctx, runner, receipt); err != nil {
		return receipt, err
	}
	receipt.CleanupConfirmed = true
	return receipt, checkpoint("rolled-back", receipt)
}

// Retirement commits only the descriptor's old CLI inventory. It never derives
// an enclosing Electron/application root from a binary-directory pathname.
const onboardingUpgradeRetireScript = `
if set(h)!={'receipt','existing','retiredProcessSuffix'}: raise RuntimeError('invalid upgrade retirement request')
import ctypes
rename=ctypes.CDLL(None,use_errno=True).renameat2
rename.argtypes=[ctypes.c_int,ctypes.c_char_p,ctypes.c_int,ctypes.c_char_p,ctypes.c_uint]; rename.restype=ctypes.c_int
def move(first,second,flags):
 if rename(-100,os.fsencode(first),-100,os.fsencode(second),flags)!=0: raise OSError(ctypes.get_errno(),'conditional product-file transition refused')
class RetirementBusyError(Exception): pass
class RetirementProofError(Exception): pass
def retire_guarded():
 verify_tui(); existing=h['existing']; verify_upgrade_identity(existing,False)
 manager=upgrade_manager(home)
 newunit=upgrade_unit(tui,os.path.join(bundle,'bin','nvpair-ui-broker'),existing['configHome'],145).encode()
 if manager['LoadState']!='loaded' or manager['ActiveState']!='active' or manager['SubState']!='running' or manager['FragmentPath']!=existing['unitPath'] or manager['DropInPaths'] or manager['Job'] not in ('','0') or upgrade_private(existing['unitPath'],home,16384)[0]!=newunit: raise RuntimeError('successor service ownership is not confirmed for retirement')
 pid=int(manager['MainPID']); current=os.stat('/proc/'+str(pid)+'/exe'); expected=os.stat(tui)
 if pid<=0 or (current.st_dev,current.st_ino)!=(expected.st_dev,expected.st_ino): raise RuntimeError('successor service executable is unconfirmed')
 if h['retiredProcessSuffix']!='.pair-retired-'+op: raise RuntimeError('retirement alias differs from the bound operation')
 entries=[dict(name=e['name'],sha256=e['sha256'],bytes=e['bytes']) for e in existing['components']]
 entries.append(dict(name='manifest.json',sha256=existing['manifestSha256'],bytes=None))
 binding={'policy':'linux-executable-write-exclusion-v1','owner':marker,'peer':existing['nodeId'],'cluster':existing['clusterId'],'certificate':existing['certFingerprint'],'oldBundle':existing['bundle'],'oldUnit':existing['unitSha256'],'oldManifest':existing['manifestSha256'],'entries':entries}
 def managed_predecessor():
  bundles=os.path.join(root,'bundles'); managed=os.path.dirname(existing['bundle'])
  if os.path.basename(existing['bundle'])!='bin' or os.path.dirname(managed)!=bundles:
   if os.path.commonpath([bundles,os.path.abspath(existing['bundle'])])==bundles: raise RuntimeError('managed predecessor path is not a digest bundle')
   return None
  digest=os.path.basename(managed)
  if len(digest)!=64 or any(c not in '0123456789abcdef' for c in digest): raise RuntimeError('managed predecessor digest is invalid')
  for directory in (managed,existing['bundle']):
   upgrade_private_ancestry(os.path.join(directory,'.retention-check'),home)
   observed=os.lstat(directory)
   if not stat.S_ISDIR(observed.st_mode) or observed.st_uid!=os.geteuid() or observed.st_mode&0o022 or os.path.realpath(directory)!=directory: raise RuntimeError('managed predecessor directory ownership changed')
  allowed={'.pair-onboarding.json','bin'}
  statepath=os.path.join(managed,'.pair-onboarding-state.json')
  if os.path.lexists(statepath): allowed.add('.pair-onboarding-state.json')
  if set(os.listdir(managed))!=allowed or set(os.listdir(existing['bundle']))!={e['name'] for e in entries}: raise RuntimeError('managed predecessor contains unclassified files')
  raw,info=upgrade_private(os.path.join(managed,'.pair-onboarding.json'),home,8192); owned=json.loads(raw)
  if info.st_mode&0o777!=0o600 or info.st_nlink!=1 or set(owned)!={'owner','operationId','sha256','manifestSha256','startupLifetime'} or owned['owner']!='nvidia-pair-onboarding-v1' or owned['sha256']!=digest or owned['manifestSha256']!=existing['manifestSha256'] or owned['startupLifetime'] not in ('session','persistent') or not re.fullmatch('[0-9a-f]{32}',owned['operationId']): raise RuntimeError('managed predecessor owner marker is invalid')
  if os.path.lexists(statepath):
   raw,info=upgrade_private(statepath,home,8192); state=json.loads(raw)
   if info.st_mode&0o777!=0o600 or info.st_nlink!=1 or set(state)!={'owner','nodeId','certFingerprint','stage'} or state['owner']!=owned or state['nodeId']!=existing['nodeId'] or state['certFingerprint']!=existing['certFingerprint'] or state['stage']!='installed-not-paired': raise RuntimeError('managed predecessor identity checkpoint is invalid')
  return digest
 managed_digest=managed_predecessor()
 proofpath=os.path.join(stage,'.pair-retirement-proof.json')
 def verify_retirement_stage():
  try:
   if not marked(stage): raise RuntimeError('retirement staging ownership is unconfirmed')
   upgrade_private_ancestry(proofpath,home)
   if os.lstat(stage).st_mode&0o077: raise RuntimeError('retirement proof stage is not private')
  except (OSError,RuntimeError) as error: raise RetirementProofError() from error
 def proof_private(filename):
  verify_retirement_stage()
  try: return upgrade_private(filename,home,65536)
  except (OSError,RuntimeError) as error: raise RetirementProofError() from error
 verify_retirement_stage()
 proof=None; proof_bytes=None
 if any(name.startswith('.pair-retirement-proof-') for name in os.listdir(stage)): raise RuntimeError('retirement proof publication is unconfirmed; retained evidence requires reconciliation')
 if os.path.lexists(proofpath):
  raw,info=proof_private(proofpath)
  if info.st_mode&0o777!=0o600 or info.st_nlink!=1: raise RuntimeError('retirement proof ownership is invalid')
  proof=json.loads(raw)
  if not isinstance(proof,dict) or set(proof)!={'binding','inodes','retired','pendingUnlink'} or proof['binding']!=binding or not isinstance(proof['inodes'],dict) or set(proof['inodes'])!={e['name'] for e in entries}: raise RuntimeError('retirement proof differs from this operation and inventory')
  for item in proof['inodes'].values():
   if not isinstance(item,dict) or set(item)!={'dev','ino'} or type(item['dev']) is not int or type(item['ino']) is not int or item['dev']<0 or item['ino']<=0: raise RuntimeError('retirement proof inode is invalid')
  names=[e['name'] for e in entries]
  if not isinstance(proof['retired'],list) or proof['retired']!=names[:len(proof['retired'])] or len(proof['retired'])>len(names): raise RuntimeError('retirement completion proof is not an ordered inventory prefix')
  next_name=names[len(proof['retired'])] if len(proof['retired'])<len(names) else None
  if proof['pendingUnlink'] is not None and (proof['pendingUnlink']!=next_name or next_name is None): raise RuntimeError('retirement pending proof differs from the next owned entry')
  proof_bytes=raw
 fds=[]; guards={}; locations={}; observed_inodes={}
 def checked(path,entry):
  raw,info=upgrade_private(path,home,268435456)
  if info.st_nlink!=1 or upgrade_hash(raw)!=entry['sha256'] or entry['bytes'] is not None and len(raw)!=entry['bytes']: raise RuntimeError('old retirement entry identity, link or content changed')
  if entry['name']!='manifest.json' and (len(raw)<20 or raw[:6]!=b'\x7fELF\x02\x01' or not info.st_mode&0o111): raise RuntimeError('old retirement entry is not the reviewed executable')
  return info
 def publish_proof():
  nonlocal proof_bytes
  verify_retirement_stage()
  encoded=json.dumps(proof,sort_keys=True,separators=(',',':')).encode()
  fd,temporary=tempfile.mkstemp(prefix='.pair-retirement-proof-',dir=stage); remove_temporary=True
  try:
   with os.fdopen(fd,'wb') as f: f.write(encoded); f.flush(); os.fsync(f.fileno())
   if proof_bytes is None:
    move(temporary,proofpath,1)
   else:
    before,observed=proof_private(proofpath)
    if before!=proof_bytes or observed.st_mode&0o777!=0o600 or observed.st_nlink!=1: raise RuntimeError('retirement proof changed before progress publication')
    move(temporary,proofpath,2); remove_temporary=False
    displaced,previous=proof_private(temporary)
    if displaced!=proof_bytes or (previous.st_dev,previous.st_ino)!=(observed.st_dev,observed.st_ino): raise RuntimeError('retirement proof changed during progress publication; evidence retained')
    remove_temporary=True
   directory=os.open(stage,os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW|os.O_CLOEXEC)
   try: os.fsync(directory)
   finally: os.close(directory)
   proof_bytes=encoded
  finally:
   if remove_temporary and os.path.lexists(temporary): os.unlink(temporary)
 def refresh_launcher():
  launcher=existing['launcherPath']; launcher_state='absent'
  if not existing['launcherPresent']:
   if os.path.lexists(launcher): raise RuntimeError('an unreviewed user launcher appeared')
  else:
   old=existing['launcherBody'].encode(); new=upgrade_wrapper(tui,existing['launcherBody'].startswith('#!/bin/sh\n# Personal AI Router terminal UI launcher.')).encode()
   raw,observed=upgrade_private(launcher,home,8192); temporary=launcher+'.pair-upgrade-'+op
   if raw not in (old,new): raise RuntimeError('reviewed user launcher changed; foreign content was not overwritten')
   if os.path.lexists(temporary):
    retained,_=upgrade_private(temporary,home,8192)
    if retained not in (old,new): raise RuntimeError('launcher transition retains unknown bytes; reconcile before retry')
    os.unlink(temporary)
   if raw==old:
    fd=os.open(temporary,os.O_WRONLY|os.O_CREAT|os.O_EXCL,0o600)
    with os.fdopen(fd,'wb') as f: f.write(new); f.flush(); os.fsync(f.fileno())
    os.chmod(temporary,observed.st_mode&0o777); replacement=os.stat(temporary)
    move(temporary,launcher,2)
    swapped,oldstat=upgrade_private(temporary,home,8192)
    if swapped!=old or (oldstat.st_dev,oldstat.st_ino)!=(observed.st_dev,observed.st_ino):
     now=os.lstat(launcher)
     if (now.st_dev,now.st_ino)==(replacement.st_dev,replacement.st_ino): move(temporary,launcher,2)
     raise RuntimeError('launcher changed during conditional replacement; prior bytes retained')
    os.unlink(temporary)
   if upgrade_private(launcher,home,8192)[0]!=new: raise RuntimeError('successor user launcher readback failed')
   launcher_state='refreshed'
  return launcher_state
 try:
  # A write-open never creates, truncates or writes an old executable. Linux
  # refuses it for an executing inode and refuses new exec while it stays open.
  # This gives positive evidence about our exact files, independent of unrelated
  # non-dumpable processes. Acquire every remaining guard before any deletion.
  for entry in entries:
   original=os.path.join(existing['bundle'],entry['name']); alias=original+h['retiredProcessSuffix']
   present=[name for name in (original,alias) if os.path.lexists(name)]
   if len(present)>1: raise RuntimeError('both original and retirement alias exist; ownership is ambiguous')
   if proof is None and present!=[original]: raise RuntimeError('legacy retirement is incomplete without a matching proof')
   if not present:
    if entry['name'] not in proof['retired']: raise RuntimeError('missing old entry has no completed retirement proof; outcome remains unconfirmed')
    continue
   if proof is not None and entry['name'] in proof['retired']: raise RuntimeError('an entry reappeared after its proven retirement')
   filename=present[0]; before=checked(filename,entry)
   flags=(os.O_RDONLY if entry['name']=='manifest.json' else os.O_WRONLY)|os.O_NOFOLLOW|os.O_CLOEXEC
   try: fd=os.open(filename,flags)
   except OSError as error:
    if error.errno==26 and entry['name']!='manifest.json': raise RetirementBusyError()
    raise
   fds.append(fd); opened=os.fstat(fd)
   after=checked(filename,entry)
   before_id=(before.st_dev,before.st_ino,before.st_uid,before.st_mode,before.st_size,before.st_nlink)
   if before_id!=(opened.st_dev,opened.st_ino,opened.st_uid,opened.st_mode,opened.st_size,opened.st_nlink) or before_id!=(after.st_dev,after.st_ino,after.st_uid,after.st_mode,after.st_size,after.st_nlink): raise RuntimeError('old executable changed while its kernel guard was acquired')
   identity={'dev':opened.st_dev,'ino':opened.st_ino}
   if proof is not None and proof['inodes'][entry['name']]!=identity: raise RuntimeError('remaining old entry differs from its guarded retirement proof')
   observed_inodes[entry['name']]=identity; guards[entry['name']]=fd; locations[entry['name']]=filename
  verify_upgrade_identity(existing,False); verify_tui()
  if managed_digest:
   if proof is not None or len(observed_inodes)!=len(entries): raise RuntimeError('managed predecessor retention has ambiguous retirement state')
   launcher_state=refresh_launcher(); verify_upgrade_identity(existing,False); verify_tui(); managed_predecessor()
   return dict(retired=True,retiredEntries=0,retainedRollback=managed_digest,launcher=launcher_state,scope='managed-bundle-retained')
  if proof is None:
   if len(observed_inodes)!=len(entries): raise RuntimeError('complete original inventory was not guarded')
   proof={'binding':binding,'inodes':observed_inodes,'retired':[],'pendingUnlink':None}
   publish_proof()
  launcher_state=refresh_launcher()
  for entry in entries:
   original=os.path.join(existing['bundle'],entry['name']); alias=original+h['retiredProcessSuffix']
   filename=locations.get(entry['name'])
   if filename is None:
    if os.path.lexists(original) or os.path.lexists(alias): raise RuntimeError('an entry appeared at a previously retired path')
    continue
   held=os.fstat(guards[entry['name']]); before=checked(filename,entry)
   if (held.st_dev,held.st_ino)!=(before.st_dev,before.st_ino): raise RuntimeError('retirement guard no longer names the reviewed file')
   if filename==original: move(original,alias,1)
   try:
    after=checked(alias,entry)
    if (held.st_dev,held.st_ino)!=(after.st_dev,after.st_ino): raise RuntimeError('old entry inode changed during retirement')
   except Exception:
    if filename==original and not os.path.lexists(original): move(alias,original,1)
    raise
   proof['pendingUnlink']=entry['name']; publish_proof()
   os.unlink(alias)
   if os.fstat(guards[entry['name']]).st_nlink!=0: raise RuntimeError('old executable acquired an unreviewed hardlink during retirement')
   if os.path.lexists(original) or os.path.lexists(alias): raise RuntimeError('another entry appeared at a retired path')
   proof['retired'].append(entry['name']); proof['pendingUnlink']=None; publish_proof()
  verify_upgrade_identity(existing,False); verify_tui()
  return dict(retired=True,retiredEntries=len(entries),launcher=launcher_state,scope='bound-cli-only')
 finally:
  for fd in fds: os.close(fd)
try: print(json.dumps(retire_guarded()))
except RetirementBusyError:
 print(json.dumps(dict(retired=False,retiredEntries=0,launcher='unconfirmed',scope='bound-cli-only',failureCode='old-executable-busy')))
except RetirementProofError:
 print(json.dumps(dict(retired=False,retiredEntries=0,launcher='unconfirmed',scope='bound-cli-only',failureCode='retirement-proof-unconfirmed')))
`

var errOnboardingRetirementBusy = errors.New("an old PAIR executable is still in use; retirement remains held")
var errOnboardingRetirementProof = errors.New("retirement proof ownership or progress is unconfirmed; retirement remains held")

func retireOnboardingUpgrade(ctx context.Context, runner onboardingInstallRunner, receipt onboardingInstallReceipt, old onboardingExistingInstallation) error {
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()
	retiredSuffix := ".pair-retired-" + path.Base(receipt.StagePath)
	body, err := runner.run(ctx, onboardingPython(onboardingUpgradeInspectPrelude+onboardingOwnedScript+onboardingUpgradeIdentityPrelude+onboardingUpgradeRetireScript), bytes.NewReader(onboardingMarshal(map[string]any{"receipt": receipt, "existing": old, "retiredProcessSuffix": retiredSuffix})))
	var result struct {
		Retired          bool   `json:"retired"`
		RetiredEntries   int    `json:"retiredEntries"`
		RetainedRollback string `json:"retainedRollback,omitempty"`
		Launcher         string `json:"launcher"`
		Scope            string `json:"scope"`
		FailureCode      string `json:"failureCode,omitempty"`
	}
	launcher := "absent"
	if old.LauncherPresent {
		launcher = "refreshed"
	}
	if err == nil && onboardingDecode(body, &result) == nil && !result.Retired && result.RetiredEntries == 0 && result.Launcher == "unconfirmed" && result.Scope == "bound-cli-only" {
		switch result.FailureCode {
		case "old-executable-busy":
			return errOnboardingRetirementBusy
		case "retirement-proof-unconfirmed":
			return errOnboardingRetirementProof
		}
	}
	entries, retained, scope := len(old.Components)+1, "", "bound-cli-only"
	if digest, managed := onboardingManagedBundleDigest(old); managed {
		entries, retained, scope = 0, digest, "managed-bundle-retained"
	}
	if err != nil || onboardingDecode(body, &result) != nil || !result.Retired || result.FailureCode != "" || result.RetiredEntries != entries || result.RetainedRollback != retained || result.Launcher != launcher || result.Scope != scope {
		return errors.New("successor is retained but old CLI payload or launcher retirement is unconfirmed; retry this operation")
	}
	return nil
}

func (s *onboardingService) upgradeCheckpoint(run *onboardingRun, id, phase string, receipt onboardingInstallReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if phase == "retiring" && run.cancelTarget[id] {
		return context.Canceled
	}
	plan := run.Plans[id]
	plan.UpgradePhase, plan.Receipt = phase, receipt
	run.Plans[id] = plan
	if phase == "retiring" || phase == "retired" {
		for i := range run.Public.Targets {
			if run.Public.Targets[i].CandidateID == id {
				run.Public.Targets[i].CanCancel = false
			}
		}
	}
	run.Public.Revision++
	if err := s.saveRun(run); err != nil {
		s.recoveryRequired = true
		return errors.New("upgrade checkpoint could not be retained; no next lifecycle phase was started")
	}
	return nil
}

// A cancelled SSH command closes its transport. Recovery obtains a new session
// under its own cleanup bound, keeping the original account and host-key grant.
func (s *onboardingService) dialUpgradeRecovery(ctx context.Context, run *onboardingRun, id string) (*onboardingSSH, error) {
	s.mu.Lock()
	plan := run.Plans[id]
	target := s.targets[id]
	var access onboardingAccess
	available := false
	if target != nil && target.accessGeneration == plan.AccessGeneration {
		access = target.access
		available = target.candidate.AccessAvailable
	}
	s.mu.Unlock()
	if !available || access.user == "" || access.user != plan.Username || plan.ExistingInstallation == nil {
		return nil, errors.New("authorize the same reviewed account before upgrade recovery")
	}
	candidate := plan.Candidate
	candidate.HostKeyTrusted, candidate.HostKeySHA256 = true, plan.Review.HostKeySHA256
	client, err := s.dial(ctx, candidate, access.forPurpose("enrolled-peer-upgrade"))
	if err != nil {
		return nil, errors.New("reviewed peer access is unavailable for upgrade recovery")
	}
	info, err := readOnboardingPlatform(ctx, client)
	osName, architecture, platformErr := onboardingPlatform(info.OS, info.Arch)
	if err != nil || platformErr != nil || info.UID != plan.Info.UID || info.Home != plan.Info.Home || osName != plan.Review.Platform || architecture != plan.Review.Arch {
		client.close()
		return nil, errors.New("reviewed account or platform changed before upgrade recovery")
	}
	return client, nil
}

func (s *onboardingService) executeUpgradeTarget(ctx context.Context, client *onboardingSSH, run *onboardingRun, id string, plan onboardingPlan, access onboardingAccess, cancelled bool) {
	if !validOnboardingUpgradePlan(plan) || plan.ExistingInstallation == nil {
		s.updateTarget(run, id, "blocked", "The retained enrolled-peer upgrade binding is invalid", nil, "")
		return
	}
	old := *plan.ExistingInstallation
	receipt, phase := plan.Receipt, plan.UpgradePhase
	pkg := onboardingPackage{source: plan.Artifact, file: plan.PackageFile, archiveRoot: plan.ArchiveRoot, bytes: plan.ArchiveBytes}
	if receipt.StagePath == "" {
		var err error
		receipt, err = onboardingInstallPaths(plan.Info, pkg, run.Public.OperationID)
		if err == nil {
			receipt.ManifestSHA256, err = onboardingPackageManifestHash(pkg)
		}
		if err != nil {
			s.updateTarget(run, id, "blocked", "The reviewed successor package is unavailable", nil, "")
			return
		}
		receipt.StartupLifetime, receipt.NodeID = plan.Review.StartupLifetime, old.NodeID
		plan.Receipt = receipt
	}
	checkpoint := func(next string, observed onboardingInstallReceipt) error {
		if next == "retiring" && ctx.Err() != nil {
			return ctx.Err()
		}
		if next == "staging" || next == "stopping" || next == "installing-unit" || next == "starting" {
			identity, err := s.localIdentity(ctx)
			if err != nil || identity.NodeUUID != run.ControllerNodeID || identity.ClusterID != run.Public.TargetClusterID || identity.ClusterID != old.ClusterID {
				return errors.New("controller membership changed; no next upgrade phase was admitted")
			}
			if err := s.verifyUpgradePeer(ctx, old, run.ControllerNodeID, old.ClusterID); err != nil {
				return err
			}
		}
		return s.upgradeCheckpoint(run, id, next, observed)
	}
	if err := s.upgradeCheckpoint(run, id, phase, receipt); err != nil {
		return
	}
	committed := phase == "retiring" || phase == "retired"
	if !committed && (cancelled || ctx.Err() != nil || phase == "rolling-back") {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), onboardingUpgradePhaseBudget+40*time.Second)
		defer cancel()
		cleaner, err := s.dialUpgradeRecovery(cleanupCtx, run, id)
		if err == nil {
			defer cleaner.close()
		}
		if err == nil && upgradeNeedsRollback(phase) {
			receipt, err = rollbackOnboardingUpgrade(cleanupCtx, cleaner, old, receipt, checkpoint)
		} else if err == nil {
			err = cleanOnboardingUpgradeStage(cleanupCtx, cleaner, receipt)
			if err == nil {
				receipt.CleanupConfirmed = true
				err = checkpoint("cancelled-before-stop", receipt)
			}
		}
		stage, message := "verification-failed", "The prior upgrade was rolled back; the original peer identity and models were retained. Retry this reviewed operation to try again."
		if cancelled && err == nil {
			stage, message = "cancelled", "Upgrade cancelled; the original peer service, identity and models were retained"
		}
		if err != nil {
			receipt.CleanupConfirmed = false
			message = "Upgrade recovery is unconfirmed; retain the same operation and authorize access again"
		}
		s.updateTarget(run, id, stage, message, &receipt, old.NodeID)
		return
	}
	var err error
	legacyPackageRetired := false
	// A retained retiring phase may have been interrupted after the exact user
	// unit stopped. Reconcile only the remote proof before ordinary membership
	// checks; a fresh remove attempt is admitted later, after reciprocal proof.
	if phase == "retiring" && old.LegacyPackage != nil {
		var result onboardingLegacyPackageResult
		result, err = runOnboardingLegacyPackageAction(ctx, client, receipt, old, access, "reconcile")
		legacyPackageRetired = err == nil && result.Retired
	}
	if !committed {
		if _, _, err := verifyOnboardingArchive(pkg.file, pkg.source.onboardingArtifact); err != nil {
			s.updateTarget(run, id, "blocked", "The strict fifteen-binary successor changed after review", &receipt, old.NodeID)
			return
		}
		if err := s.updateTarget(run, id, "installing", "Staging and verifying the successor before any enrolled-peer downtime", &receipt, old.NodeID); err != nil {
			return
		}
		receipt, phase, err = advanceOnboardingUpgrade(ctx, client, plan, run.Public.OperationID, checkpoint)
	}
	if err == nil {
		if err = s.updateTarget(run, id, "verifying", "Verifying the successor under the original paired node identity", &receipt, old.NodeID); err == nil {
			_, _, err = s.waitOnboardingMembership(ctx, client, receipt, run.ControllerNodeID, old.NodeID, old.ClusterID)
		}
	}
	if err == nil {
		err = verifyOnboardingUpgradedIdentity(ctx, client, receipt, old)
	}
	if err == nil && phase != "retired" {
		if err = checkpoint("retiring", receipt); err == nil {
			phase = "retiring"
			if !legacyPackageRetired {
				err = s.retireOnboardingLegacyPackage(ctx, client, receipt, old, access, run.ControllerNodeID)
			}
			if err == nil {
				err = retireOnboardingUpgrade(ctx, client, receipt, old)
			}
			if err == nil {
				err = checkpoint("retired", receipt)
				phase = "retired"
			}
		}
	}
	if err == nil && phase == "retired" {
		err = recordOnboardingBundleRetention(ctx, client, receipt, old)
	}
	if err == nil {
		receipt.CleanupConfirmed, err = finishOnboardingTarget(ctx, client, receipt)
	}
	if err == nil && receipt.CleanupConfirmed {
		if err = checkpoint("retired", receipt); err == nil {
			s.updateTarget(run, id, "paired", onboardingLegacyPackageCompletion(old), &receipt, old.NodeID)
			return
		}
	}
	s.mu.Lock()
	cancelled = run.cancelTarget[id]
	s.mu.Unlock()
	message := "Peer upgrade did not complete; the staged successor and original ownership record were retained for retry"
	if phase == "retiring" || phase == "retired" {
		message = "Successor is committed; resume verification/retirement of this same operation. No rollback or deletion outside the bound old CLI inventory is allowed."
		if errors.Is(err, errOnboardingRetirementBusy) {
			message = "Successor is committed, but an old PAIR executable is still in use. Retirement remains held; retry this same operation after it stops."
		} else if errors.Is(err, errOnboardingRetirementProof) {
			message = "Successor is committed, but retirement proof ownership or progress is unconfirmed. Preserve this same operation for reconciliation; no old files were assumed retired."
		}
	}
	stage := "verification-failed"
	if upgradeNeedsRollback(phase) {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), onboardingUpgradePhaseBudget+40*time.Second)
		cleaner, rollbackErr := s.dialUpgradeRecovery(cleanupCtx, run, id)
		if rollbackErr == nil {
			receipt, rollbackErr = rollbackOnboardingUpgrade(cleanupCtx, cleaner, old, receipt, checkpoint)
			cleaner.close()
		}
		cancel()
		if rollbackErr == nil {
			message = "Upgrade interrupted; the original peer service was restored with its identity and models unchanged. Retry the same reviewed operation."
			if cancelled {
				stage, message = "cancelled", "Upgrade cancelled and original peer service restored; identity and models retained"
			}
		} else {
			receipt.CleanupConfirmed = false
			message = "Upgrade and rollback completion are unconfirmed; preserve this exact operation for recovery"
		}
	} else if cancelled && !committed && phase != "retiring" && phase != "retired" {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		cleaner, cleanupErr := s.dialUpgradeRecovery(cleanupCtx, run, id)
		if cleanupErr == nil {
			cleanupErr = cleanOnboardingUpgradeStage(cleanupCtx, cleaner, receipt)
			cleaner.close()
		}
		cancel()
		if cleanupErr == nil {
			receipt.CleanupConfirmed = true
			cleanupErr = checkpoint("cancelled-before-stop", receipt)
		}
		if cleanupErr == nil {
			stage, message = "cancelled", "Upgrade cancelled before downtime; old peer service and data retained"
		}
	}
	s.updateTarget(run, id, stage, message, &receipt, old.NodeID)
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func onboardingLegacyUnitFixture(t *testing.T) (onboardingInstallReceipt, string, string, string) {
	t.Helper()
	receipt, home := onboardingPythonVerifierFixture(t)
	receipt.ServiceInstalled = true
	unitPath := filepath.Join(home, ".config", "systemd", "user", "nvidia-pair-headless.service")
	if err := os.MkdirAll(filepath.Dir(unitPath), 0700); err != nil {
		t.Fatal(err)
	}
	q := func(value string) string {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, `%`, `%%`).Replace(value) + `"`
	}
	directory := filepath.Dir(receipt.TUIPath)
	text := "# Owned by NVIDIA Personal AI Router headless service; do not replace a foreign unit.\n[Unit]\nDescription=NVIDIA Personal AI Router headless backend\n\n[Service]\nType=simple\nExecStart=" + q(strings.ReplaceAll(receipt.TUIPath, "$", "$$")) + " --headless --broker-path " + q(strings.ReplaceAll(filepath.Join(directory, "nvpair-ui-broker"), "$", "$$")) + "\nWorkingDirectory=" + q(directory) + "\nEnvironment=" + q("XDG_CONFIG_HOME="+filepath.Join(home, ".config")) + "\nRestart=on-failure\nRestartSec=2\nTimeoutStopSec=25\nKillMode=mixed\nUMask=0077\n\n[Install]\nWantedBy=default.target\n"
	if err := os.WriteFile(unitPath, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
	return receipt, home, unitPath, text
}

// Executes production Python recovery against real private POSIX files/links.
// Only subprocess replies and explicit negative UID/mode facts are substituted;
// no systemctl or installed PAIR executable is launched.
func executeLegacyUnitFixture(t *testing.T, home string, receipt onboardingInstallReceipt, variant, tail string) ([]byte, error) {
	t.Helper()
	variantJSON, _ := json.Marshal(variant)
	setup := `
variant=` + string(variantJSON) + `
unitpath=os.path.join(home,'.config','systemd','user','nvidia-pair-headless.service')
calls=[]; reloaded=False; shows=0
native_lstat=os.lstat
def fixture_lstat(p,*args,**kwargs):
 st=native_lstat(p,*args,**kwargs)
 if os.fspath(p)!=unitpath or variant not in ('foreign-uid','writable-unit'): return st
 mode=st.st_mode
 uid=st.st_uid
 if os.fspath(p)==unitpath and variant=='foreign-uid': uid=os.geteuid()+1
 if os.fspath(p)==unitpath and variant=='writable-unit': mode|=0o020
 return types.SimpleNamespace(st_mode=mode,st_uid=uid,st_size=st.st_size,st_dev=st.st_dev,st_ino=st.st_ino)
os.lstat=fixture_lstat
def fake_product(argv,input=None,limit=1048576,raw_output=False,timeout=30):
 global reloaded,shows
 calls.append(argv)
 if argv[0]==tui:
  if '--headless-service' not in argv or 'status' not in argv: raise RuntimeError('fixture refuses non-status TUI action')
  if variant=='old-owner-unknown': raise RuntimeError('old owner unavailable')
  return {'unit':'nvidia-pair-headless.service','state':'inactive'}
 if argv[0]!='/usr/bin/systemctl' or argv[1:4]!=['--user','--no-ask-password','--no-pager']: raise RuntimeError('unexpected native action')
 if argv[4]=='daemon-reload':
  reloaded=True
  if variant=='lost-ack': raise RuntimeError('fixture lost reload acknowledgement')
  return ''
 if argv[4]!='show': raise RuntimeError('only fixed manager reads/reload are allowed')
 shows+=1
 if variant=='manager-unavailable': raise RuntimeError('manager unavailable')
 absent=reloaded and variant!='absence-unconfirmed'
 values={'LoadState':'not-found' if absent else 'bad-setting','ActiveState':'inactive','SubState':'dead','MainPID':'0','ExecMainPID':'0','FragmentPath':'' if absent else unitpath,'DropInPaths':'','Job':''}
 if variant=='active': values['ActiveState']='active'
 if variant=='pid': values['MainPID']='123'
 if variant=='exec-pid': values['ExecMainPID']='123'
 if variant=='drop-in': values['DropInPaths']='/foreign/override.conf'
 if variant=='job': values['Job']='123'
 if variant=='wrong-fragment': values['FragmentPath']='/foreign/unit.service'
 if variant=='incomplete': del values['ExecMainPID']
 if variant=='changed-unit' and shows==2:
  with open(unitpath,'a') as f: f.write('# changed while checking\n')
 return '\n'.join(key+'='+value for key,value in values.items())
product=fake_product
`
	return executeOnboardingPythonFixture(t, home, receipt, setup+onboardingMalformedUnitRecovery+tail)
}

func TestOnboardingLegacyUnitRecoveryArchivesExactUnitAndRequiresAbsence(t *testing.T) {
	receipt, home, unitPath, original := onboardingLegacyUnitFixture(t)
	output, err := executeLegacyUnitFixture(t, home, receipt, "", strings.TrimPrefix(onboardingCleanupScript, onboardingMalformedUnitRecovery))
	if err != nil || !strings.Contains(string(output), `"cleanupConfirmed": true`) {
		t.Fatalf("recovery=%s %v", output, err)
	}
	if _, err := os.Lstat(unitPath); !os.IsNotExist(err) {
		t.Fatal("legacy unit still exists")
	}
	var saved struct {
		UnitText string         `json:"unitText"`
		UnitPath string         `json:"unitPath"`
		Owner    map[string]any `json:"owner"`
	}
	data, err := os.ReadFile(filepath.Join(receipt.BundlePath, ".pair-onboarding-retired-unit.json"))
	if err != nil || json.Unmarshal(data, &saved) != nil || saved.UnitText != original || saved.UnitPath != unitPath || saved.Owner["operationId"] != filepath.Base(receipt.StagePath) {
		t.Fatal("exact recoverable unit archive missing")
	}
	if output, err := executeLegacyUnitFixture(t, home, receipt, "", "\nverify_tui()\nif not recover_legacy_inactive_unit(): raise RuntimeError('lost acknowledgement not recovered')\nprint('recovered')\n"); err != nil {
		t.Fatalf("repeat recovery=%s %v", output, err)
	}
}

func TestOnboardingLegacyUnitRecoveryRejectsForeignActiveOrUnknown(t *testing.T) {
	expectedErrors := map[string]string{"foreign-uid": "file ownership is unknown", "writable-unit": "file ownership is unknown", "active": "active or unconfirmed manager state", "pid": "active or unconfirmed manager state", "exec-pid": "active or unconfirmed manager state", "drop-in": "active or unconfirmed manager state", "job": "active or unconfirmed manager state", "wrong-fragment": "exact inactive malformed legacy instance", "incomplete": "active or unconfirmed manager state", "manager-unavailable": "manager unavailable", "old-owner-unknown": "old owner unavailable", "changed-unit": "unit changed before exact recovery", "identity": "never-started identity"}
	for _, variant := range []string{"foreign-uid", "writable-unit", "active", "pid", "exec-pid", "drop-in", "job", "wrong-fragment", "incomplete", "manager-unavailable", "old-owner-unknown", "changed-unit", "identity"} {
		t.Run(variant, func(t *testing.T) {
			receipt, home, unitPath, _ := onboardingLegacyUnitFixture(t)
			if variant == "identity" {
				receipt.NodeID = "previously-started-node"
			}
			output, err := executeLegacyUnitFixture(t, home, receipt, variant, "\nverify_tui()\nrecover_legacy_inactive_unit()\nprint('unexpected-success')\n")
			if err == nil || strings.Contains(string(output), "unexpected-success") {
				t.Fatalf("unsafe recovery accepted=%s", output)
			}
			if !strings.Contains(string(output), expectedErrors[variant]) {
				t.Fatalf("negative failed before its intended guard: %s", output)
			}
			if _, err := os.Stat(unitPath); err != nil {
				t.Fatal("unconfirmed unit was removed")
			}
		})
	}
}

func TestOnboardingLegacyUnitRecoveryRetainsUnconfirmedArchiveForRetry(t *testing.T) {
	for _, variant := range []string{"lost-ack", "absence-unconfirmed"} {
		t.Run(variant, func(t *testing.T) {
			receipt, home, unitPath, _ := onboardingLegacyUnitFixture(t)
			if output, err := executeLegacyUnitFixture(t, home, receipt, variant, "\nverify_tui()\nrecover_legacy_inactive_unit()\nprint('unexpected-success')\n"); err == nil {
				t.Fatalf("uncertain manager became clean=%s", output)
			}
			if _, err := os.Stat(unitPath); !os.IsNotExist(err) {
				t.Fatal("fixture did not reach lost-ack removal")
			}
			if output, err := executeLegacyUnitFixture(t, home, receipt, "", "\nverify_tui()\nif not recover_legacy_inactive_unit(): raise RuntimeError('retained retry unavailable')\nprint('recovered')\n"); err != nil {
				t.Fatalf("retained retry=%s %v", output, err)
			}
		})
	}
}

func TestOnboardingLegacyUnitRecoveryExactEnablementAndSymlinkGuards(t *testing.T) {
	for _, variant := range []string{"default-link", "different-alias", "direct-alias", "config-redirect", "systemd-redirect"} {
		t.Run(variant, func(t *testing.T) {
			receipt, home, unitPath, original := onboardingLegacyUnitFixture(t)
			preserved := unitPath
			linkPath := filepath.Join(filepath.Dir(unitPath), "default.target.wants", "nvidia-pair-headless.service")
			switch variant {
			case "default-link", "different-alias", "direct-alias":
				if variant == "different-alias" {
					linkPath = filepath.Join(filepath.Dir(linkPath), "other-name.service")
				}
				if variant == "direct-alias" {
					linkPath = filepath.Join(filepath.Dir(unitPath), "other-name.service")
				}
				if err := os.MkdirAll(filepath.Dir(linkPath), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(unitPath, linkPath); err != nil {
					t.Fatal(err)
				}
			case "config-redirect", "systemd-redirect":
				redirect := filepath.Join(home, ".config")
				if variant == "systemd-redirect" {
					redirect = filepath.Join(redirect, "systemd")
				}
				outside := filepath.Join(t.TempDir(), "redirected")
				rel, err := filepath.Rel(redirect, unitPath)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Rename(redirect, outside); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(outside, redirect); err != nil {
					t.Fatal(err)
				}
				preserved = filepath.Join(outside, rel)
			}
			output, err := executeLegacyUnitFixture(t, home, receipt, "", "\nverify_tui()\nif not recover_legacy_inactive_unit(): raise RuntimeError('legacy missing')\nprint('recovered')\n")
			if variant == "default-link" {
				if err != nil {
					t.Fatalf("default enablement recovery=%s %v", output, err)
				}
				if _, err := os.Lstat(linkPath); !os.IsNotExist(err) {
					t.Fatal("owned default enablement remains")
				}
			} else {
				if err == nil {
					t.Fatalf("unsafe alias/ancestry admitted: %s", output)
				}
				data, e := os.ReadFile(preserved)
				if e != nil || string(data) != original {
					t.Fatal("foreign/redirected unit changed")
				}
			}
		})
	}
}

func TestOnboardingLegacyRecoveryPreservesOtherDefinitionsAndIdentityFiles(t *testing.T) {
	for _, variant := range []string{"different-definition", "identity-file", "wrong-archive"} {
		t.Run(variant, func(t *testing.T) {
			receipt, home, unitPath, original := onboardingLegacyUnitFixture(t)
			tail := "\nverify_tui()\nrecover_legacy_inactive_unit()\n"
			switch variant {
			case "different-definition":
				original += "# a different operator configuration\n"
				if err := os.WriteFile(unitPath, []byte(original), 0600); err != nil {
					t.Fatal(err)
				}
				tail = "\nverify_tui()\nif recover_legacy_inactive_unit(): raise RuntimeError('normal lifecycle intercepted')\nprint('normal-tui-path')\n"
			case "identity-file":
				identity := filepath.Join(home, ".config", "Nvidia Corporation", "Personal AI Router", "cluster", "identity.json")
				if err := os.MkdirAll(filepath.Dir(identity), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(identity, []byte(`{}`), 0600); err != nil {
					t.Fatal(err)
				}
			case "wrong-archive":
				if err := os.WriteFile(filepath.Join(receipt.BundlePath, ".pair-onboarding-retired-unit.json"), []byte(`{"owner":"another-operation"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			output, err := executeLegacyUnitFixture(t, home, receipt, "", tail)
			if variant == "different-definition" {
				if err != nil || !strings.Contains(string(output), "normal-tui-path") {
					t.Fatalf("normal lifecycle stolen: %s %v", output, err)
				}
			} else if err == nil {
				t.Fatal("unsafe recovery accepted")
			}
			data, e := os.ReadFile(unitPath)
			if e != nil || string(data) != original {
				t.Fatal("unrelated unit/identity was changed")
			}
		})
	}
}

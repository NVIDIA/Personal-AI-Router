// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The initial recipe deliberately proves socket-transport collectives, not
// RDMA, topology, line rate, or model serving. It never changes interfaces.
var diagnosticNCCLArgs = []string{"-b", "8", "-e", "8388608", "-f", "2", "-g", "1", "-t", "1", "-n", "10", "-w", "1", "-c", "1", "-N", "1", "-T", "10", "-d", "float", "-o", "sum"}

type diagnosticRankRecord struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	Done       bool   `json:"done"`
	Clean      bool   `json:"clean"`
	Error      string `json:"error,omitempty"`
}

func (d *diagnosticService) nativePreflight(ctx context.Context, p diagnosticProfile, member diagnosticMember, operationID string) error {
	if runtime.GOOS != "linux" {
		return errors.New("NCCL diagnostics execute only on configured Linux participants")
	}
	d.mu.Lock()
	recoveryFailed := d.recoveryFailed
	d.mu.Unlock()
	if recoveryFailed {
		return errors.New("previous diagnostic rank cleanup is unconfirmed; operator recovery is required")
	}
	d.m.mesh.Refresh()
	if !d.m.mesh.Clustered() || d.m.mesh.NodeUUID() != member.Principal {
		return errors.New("participant admission changed")
	}
	for _, m := range p.Members {
		if !d.m.mesh.HasPin(m.Principal) {
			return errors.New("configured participant pin is no longer admitted")
		}
		if p.Bootstrap.OperationID != "" {
			pin, ok := d.m.mesh.PinSHA256(m.Principal)
			if !ok || pin != m.ClusterPinSHA256 {
				return errors.New("reviewed MPI participant certificate changed")
			}
		}
	}
	if err := d.managedIdle(); err != nil {
		return err
	}
	if err := d.checkWorkloads(ctx); err != nil {
		return err
	}
	if operationID == "" {
		if p.Bootstrap.OperationID != "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return err
			}
			if err := verifyDiagnosticManagedMember(member, os.Getuid(), home, verifyDiagnosticManagedTool); err != nil {
				return err
			}
		} else {
			for _, tool := range []diagnosticTool{member.Manager, member.NCCL, member.SMI} {
				if err := tool.verify(); err != nil {
					return err
				}
			}
		}
		if member.NodeID == p.OwnerNodeID {
			tools := []diagnosticTool{p.MPI, p.SSH}
			if p.Bootstrap.OperationID == "" {
				tools = append(tools, p.KnownHosts)
			}
			for _, tool := range tools {
				var err error
				if p.Bootstrap.OperationID != "" {
					err = verifyDiagnosticManagedTool(tool)
				} else {
					err = tool.verify()
				}
				if err != nil {
					return err
				}
			}
			if p.Bootstrap.OperationID == "" {
				st, err := os.Stat(p.IdentityFile)
				if err != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 {
					return errors.New("configured SSH identity is unavailable or not private to the operator")
				}
			}
		}
	}
	iface, err := net.InterfaceByName(member.Interface)
	if err != nil || iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
		return errors.New("configured collective interface is missing or down")
	}
	addresses, err := iface.Addrs()
	haveIPv4 := false
	for _, address := range addresses {
		if ip, _, parseErr := net.ParseCIDR(address.String()); parseErr == nil && ip.To4() != nil && ip.IsGlobalUnicast() {
			haveIPv4 = true
		}
	}
	if err != nil || !haveIPv4 {
		return errors.New("configured collective interface has no address")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	output, err := diagnosticProcess(ctx, member.SMI.Path, []string{"--query-gpu=uuid,utilization.gpu", "--format=csv,noheader,nounits", "-i", member.GPU}, nil, nil)
	if err != nil {
		return fmt.Errorf("supported GPU availability check failed: %w", err)
	}
	fields := strings.Split(strings.TrimSpace(string(output)), ",")
	if len(fields) != 2 || strings.TrimSpace(fields[0]) != member.GPU {
		return errors.New("configured GPU UUID could not be verified")
	}
	util, err := strconv.Atoi(strings.TrimSpace(fields[1]))
	if err != nil || util < 0 || util > 100 {
		return errors.New("GPU utilization is unknown")
	}
	// Aggregate utilization includes graphics work. Compute ownership below,
	// the fresh PAIR workload response, and the reserved test window decide
	// admission; a desktop graphics sample alone is not compute residency.
	output, err = diagnosticProcess(ctx, member.SMI.Path, []string{"--query-compute-apps=pid,gpu_uuid", "--format=csv,noheader,nounits"}, nil, nil)
	if err != nil {
		return fmt.Errorf("GPU process ownership is unknown: %w", err)
	}
	owned := diagnosticRankRecord{}
	if operationID != "" {
		_ = readDiagnosticJSON(filepath.Join(d.runDir(operationID), "rank.json"), &owned)
	}
	for _, line := range strings.Split(strings.TrimSpace(string(output)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) != 2 {
			return errors.New("GPU process check returned an unsupported response")
		}
		pid, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil {
			return errors.New("GPU process check returned an unknown PID")
		}
		if strings.TrimSpace(fields[1]) == member.GPU && (operationID == "" || pid != owned.PID || owned.Done || !diagnosticSameProcess(pid, owned.StartTicks)) {
			return errors.New("a competing GPU process prevents the collective test")
		}
	}
	return nil
}

func (d *diagnosticService) nativeRun(ctx context.Context, p diagnosticProfile, id string) ([]diagnosticSample, error) {
	if p.Bootstrap.OperationID != "" {
		return nil, errors.New("managed MPI requires its operation-scoped volatile coordinator identity")
	}
	// No inherited MPI/NCCL/SSH command overrides. Tool and trust paths came
	// exclusively from the operator profile, never the renderer request.
	owner, _ := p.member(p.OwnerNodeID)
	dir := d.runDir(id)
	var app strings.Builder
	for _, member := range p.Members {
		fmt.Fprintf(&app, "-np 1 -host %s@%s %s --diagnostic-rank %s %s\n", member.User, member.Host, member.Manager.Path, p.GroupID, id)
	}
	appPath := filepath.Join(dir, "mpi.app")
	if err := os.WriteFile(appPath, []byte(app.String()), 0600); err != nil {
		return nil, err
	}
	agent := strings.Join([]string{p.SSH.Path, "-F", "/dev/null", "-o", "BatchMode=yes", "-o", "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + p.KnownHosts.Path, "-o", "GlobalKnownHostsFile=/dev/null", "-o", "IdentitiesOnly=yes", "-o", "PasswordAuthentication=no", "-o", "IdentityFile=" + p.IdentityFile}, " ")
	args := []string{"--mca", "plm_rsh_agent", agent, "--mca", "orte_abort_on_non_zero_status", "1", "--timeout", "90", "--app", appPath}
	output, err := diagnosticProcess(ctx, p.MPI.Path, args, []string{"HOME=" + os.Getenv("HOME"), "PATH=/usr/bin:/bin", "NCCL_IB_DISABLE=1", "NCCL_SOCKET_IFNAME==" + owner.Interface}, nil)
	if err != nil {
		return nil, fmt.Errorf("NCCL launcher failed: %w; %s", err, diagnosticTail(output))
	}
	samples, err := parseDiagnosticOutput(string(output))
	if err != nil {
		return nil, err
	}
	return samples, nil
}

func diagnosticTail(output []byte) string {
	if len(output) > 2048 {
		output = output[len(output)-2048:]
	}
	return strings.TrimSpace(string(output))
}

func diagnosticPublicMessage(message string) string {
	message = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, message)
	message = strings.Join(strings.Fields(message), " ")
	// At most 2048 UTF-8 bytes also fits the desktop's UTF-16 length bound.
	if len(message) > 2048 {
		message = message[:2048]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func parseDiagnosticOutput(output string) ([]diagnosticSample, error) {
	return parseDiagnosticRows(output, 8388608)
}

func parseDiagnosticMPIOutput(stdout, stderr, recipe string) ([]diagnosticSample, error) {
	_, maximum, err := diagnosticMPIRecipeArgs(recipe)
	if err != nil {
		return nil, err
	}
	for _, stream := range []string{stdout, stderr} {
		lower := strings.ToLower(stream)
		for _, marker := range []string{"test timeout", "test failure", "owned nccl rank failed"} {
			if strings.Contains(lower, marker) {
				return nil, errors.New("nccl-tests reported a rank failure")
			}
		}
	}
	if recipe == diagnosticMPILegacyRecipe {
		return parseDiagnosticOutput(stdout)
	}
	// The registered f253 binary/library print this exact non-comment banner.
	// Every other non-comment stdout line must remain a complete numeric row.
	var rows strings.Builder
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "NCCL version 2.30.7+cuda13.0" {
			continue
		}
		rows.WriteString(line)
		rows.WriteByte('\n')
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return parseDiagnosticRows(rows.String(), maximum)
}

func parseDiagnosticRows(output string, maximum uint64) ([]diagnosticSample, error) {
	samples := []diagnosticSample{}
	scanner := bufio.NewScanner(strings.NewReader(output))
	next := uint64(8)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 13 {
			return nil, errors.New("unsupported or incomplete nccl-tests result row")
		}
		bytes, err := strconv.ParseUint(fields[0], 10, 64)
		if err != nil || bytes != next || bytes > maximum || fields[2] != "float" || fields[3] != "sum" {
			return nil, errors.New("nccl-tests output did not match the fixed recipe")
		}
		count, countErr := strconv.ParseUint(fields[1], 10, 64)
		root, rootErr := strconv.Atoi(fields[4])
		if countErr != nil || count != bytes/4 || rootErr != nil || root != -1 {
			return nil, errors.New("nccl-tests count or root did not match float all-reduce")
		}
		values := [6]float64{}
		for i, index := range []int{5, 6, 7, 9, 10, 11} {
			value, err := strconv.ParseFloat(fields[index], 64)
			if err != nil || math.IsInf(value, 0) || math.IsNaN(value) || value < 0 {
				return nil, errors.New("invalid nccl-tests performance value")
			}
			values[i] = value
		}
		for _, index := range []int{8, 12} {
			wrong, err := strconv.ParseUint(fields[index], 10, 64)
			if err != nil || wrong != 0 {
				return nil, errors.New("NCCL correctness check failed or was not performed")
			}
		}
		samples = append(samples, diagnosticSample{Bytes: bytes, LatencyUs: values[0], AlgorithmGBps: values[1], BusGBps: values[2], Wrong: 0})
		next *= 2
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if next != maximum*2 {
		return nil, errors.New("nccl-tests did not return every bounded test size")
	}
	return samples, nil
}

func readDiagnosticJSON(path string, out any) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (128<<10)+1))
	if err != nil {
		return err
	}
	return strictDiagnosticJSON(data, out)
}

// An absent rank is not proof that MPI never prepared SSH or unit resources.
// Even malformed or inaccessible public artifacts retain the recovery hold.
func diagnosticBootstrapArtifactsPresent(dir, nativeRoot string) (bool, error) {
	for _, filename := range []string{filepath.Join(dir, "profile.json"), filepath.Join(dir, "bootstrap-plan.json"), nativeRoot} {
		if _, err := os.Lstat(filename); err == nil {
			return true, nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return true, err
		}
	}
	return false, nil
}

func diagnosticNativeBootstrapRoot() (string, error) {
	account, err := user.Current()
	if err != nil || account.HomeDir == "" {
		return "", errors.New("native MPI account ownership is unavailable")
	}
	return filepath.Join(account.HomeDir, ".local", "share", "pair-nccl-smoke-v1"), nil
}

func (d *diagnosticService) bootstrapArtifactsPresent(id string) (bool, error) {
	root, err := diagnosticNativeBootstrapRoot()
	if err != nil {
		return true, err
	}
	return diagnosticBootstrapArtifactsPresent(d.runDir(id), filepath.Join(root, id))
}

// The Go run directory may itself be lost. Native roots must still have their
// original managed bindings before restart can declare resource ownership idle.
func (d *diagnosticService) checkBootstrapRoots(ctx context.Context, root string) error {
	directory, err := os.Open(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer directory.Close()
	for checked := 0; checked < 4096; {
		entries, readErr := directory.ReadDir(128)
		if readErr != nil && readErr != io.EOF {
			return readErr
		}
		for _, entry := range entries {
			checked++
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !diagnosticDigest.MatchString(entry.Name() + entry.Name()) {
				continue
			}
			var lease diagnosticLeaseRecord
			if !entry.IsDir() || readDiagnosticJSON(filepath.Join(d.runDir(entry.Name()), "lease.json"), &lease) != nil || lease.Request.OperationID != entry.Name() || lease.Request.BootstrapPlanDigest == "" {
				return errors.New("native MPI resources have no original managed lease")
			}
			if _, err := d.participantCancellationBinding(lease.Request, ""); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
	}
	return errors.New("native MPI recovery inventory exceeds its bound")
}

// Validate before creating a cancellation marker. In particular, omitting the
// managed digest cannot turn an existing bootstrap into rank-only cleanup.
// A nonempty caller is the identity verified by the participant control server.
func (d *diagnosticService) participantCancellationBinding(r diagnosticParticipantRequest, caller string) (bool, error) {
	if !diagnosticToken.MatchString(r.GroupID) || !diagnosticDigest.MatchString(r.OperationID+r.OperationID) || !diagnosticDigest.MatchString(r.ProfileDigest) {
		return false, errors.New("invalid diagnostic cancellation binding")
	}
	var lease diagnosticLeaseRecord
	leaseErr := readDiagnosticJSON(filepath.Join(d.runDir(r.OperationID), "lease.json"), &lease)
	if leaseErr != nil {
		present, artifactErr := d.bootstrapArtifactsPresent(r.OperationID)
		if !errors.Is(leaseErr, os.ErrNotExist) || present || artifactErr != nil {
			return false, errors.New("original diagnostic lease is unavailable; retained MPI ownership requires recovery")
		}
		// A coordinator can have retained its approval before local preparation.
		// Its owner remains authoritative even though no participant lease exists.
		if _, statErr := os.Lstat(d.coordinatorBootstrapPath(r.OperationID)); statErr == nil {
			p, _, original, loadErr := d.loadCoordinatorBootstrap(r.OperationID)
			owner, known := p.member(p.OwnerNodeID)
			if loadErr != nil || original != r || !known || (caller != "" && owner.Principal != caller) {
				return false, errors.New("cancellation differs from the retained MPI coordinator approval")
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return false, errors.New("retained MPI coordinator approval is unavailable")
		}
		return false, nil // A pre-lease cancellation closes a future prepare.
	}
	if lease.Request.GroupID != r.GroupID || lease.Request.OperationID != r.OperationID || lease.Request.ProfileDigest != r.ProfileDigest {
		return false, errors.New("diagnostic cancellation differs from the retained lease")
	}
	managed := lease.Request.BootstrapPlanDigest != ""
	var p diagnosticProfile
	var err error
	if managed {
		if lease.Request != r {
			return false, errors.New("managed MPI cancellation requires the original complete lease")
		}
		p, _, err = d.loadBootstrapOperation(lease.Request)
		if err != nil {
			return false, errors.New("original managed MPI profile or plan is unavailable")
		}
		self, selfErr := d.self(p)
		if selfErr != nil || self != lease.Member {
			return false, errors.New("managed MPI participant differs from the retained lease")
		}
	} else {
		present, artifactErr := d.bootstrapArtifactsPresent(r.OperationID)
		if r.BootstrapPlanDigest != "" || present || artifactErr != nil {
			return false, errors.New("managed MPI ownership cannot use legacy rank cleanup")
		}
		if caller != "" {
			p, err = d.profile(r.GroupID)
			if err != nil || profileDigest(p) != lease.Request.ProfileDigest {
				return false, errors.New("original diagnostic profile is unavailable")
			}
		}
	}
	if caller != "" {
		owner, known := p.member(p.OwnerNodeID)
		if !known || owner.Principal != caller {
			return false, errors.New("only the retained diagnostic coordinator may cancel this operation")
		}
	}
	return managed, nil
}

func (d *diagnosticService) cancelParticipant(ctx context.Context, r diagnosticParticipantRequest) (bool, error) {
	if _, err := d.participantCancellationBinding(r, ""); err != nil {
		return false, err
	}
	// The repeated check and marker share prepare's cross-process lock, so a
	// lease cannot appear between an absent-lease check and its cancellation.
	dir := d.runDir(r.OperationID)
	unlock, err := diagnosticLock(ctx, dir)
	if err != nil {
		return false, err
	}
	managed, err := d.participantCancellationBinding(r, "")
	if err == nil {
		err = os.WriteFile(filepath.Join(dir, "cancelled"), []byte("cancelled\n"), 0600)
	}
	if err == nil {
		err = syncManagedFile(filepath.Join(dir, "cancelled"))
	}
	unlock()
	if err != nil {
		return false, err
	}
	clean, err := diagnosticCancelRank(ctx, dir)
	if managed {
		bootstrapClean, bootstrapErr := d.cleanupBootstrapParticipant(ctx, r)
		clean, err = clean && bootstrapClean, errors.Join(err, bootstrapErr)
	}
	if clean {
		d.mu.Lock()
		if d.reservation != nil && d.reservation.Request.OperationID == r.OperationID {
			d.reservation = nil
		}
		d.mu.Unlock()
	}
	return clean, err
}

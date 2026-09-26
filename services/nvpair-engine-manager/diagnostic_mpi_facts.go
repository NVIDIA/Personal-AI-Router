// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"golang.org/x/crypto/ssh"
	"nvpair-shared/cableprobe"
)

// Internal participant read. Artifact and build bindings come from the managed
// registry. Remote SSH public bytes retain their authenticated SSH binding;
// local public observation is revalidated through the pinned PAIR facts handoff.
type diagnosticMPIFactsRequest struct {
	Network       string                          `json:"network"`
	Target        diagnosticManagedTarget         `json:"target"`
	Selection     diagnosticMPIInterfaceSelection `json:"selection"`
	PeerAddress   string                          `json:"peerAddress"`
	PeerAddresses []string                        `json:"peerAddresses,omitempty"`
	SSHAddress    string                          `json:"sshAddress"`
	SSHPort       int                             `json:"sshPort"`
	SSHHostKey    diagnosticBootstrapPublicKey    `json:"sshHostKey"`
}

func (d *diagnosticService) localMPIHostIdentity(ctx context.Context, target diagnosticManagedTarget) (diagnosticBootstrapPublicKey, int, error) {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		return diagnosticBootstrapPublicKey{}, 0, errors.New("local SSH identity requires its native Linux ARM64 owner")
	}
	return d.localMPIHostIdentityWithIO(ctx, target, readDiagnosticMPILocalHostState, observeDiagnosticMPILocalHostKey)
}

type diagnosticMPILocalHostState struct {
	UID                  int
	User                 string
	Home                 string
	PublicIdentitySHA256 string
	Interface            cableprobe.Interface
}

func diagnosticMPILocalInterface(address string, interfaces []net.Interface, addresses func(net.Interface) ([]net.Addr, error)) (cableprobe.Interface, error) {
	var selected cableprobe.Interface
	if !diagnosticConcreteIPv4(address) || len(interfaces) == 0 || len(interfaces) > 128 {
		return selected, errors.New("local SSH address is not a bounded native interface selection")
	}
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		values, err := addresses(iface)
		if err != nil || len(values) > 32 {
			return selected, errors.New("local SSH interface addresses are unavailable")
		}
		for _, value := range values {
			ip, _, err := net.ParseCIDR(value.String())
			if err != nil {
				return selected, errors.New("local SSH interface address is malformed")
			}
			if ip.String() != address {
				continue
			}
			if selected.Name != "" {
				return selected, errors.New("local SSH address belongs to multiple interface entries")
			}
			selected = cableprobe.Interface{Name: iface.Name, Index: iface.Index, MAC: iface.HardwareAddr.String()}
		}
	}
	if !cableInterfaceValid(selected) {
		return selected, errors.New("selected SSH address is not on an up local management interface")
	}
	return selected, nil
}

func readDiagnosticMPILocalHostState(target diagnosticManagedTarget) (diagnosticMPILocalHostState, error) {
	var result diagnosticMPILocalHostState
	account, err := user.LookupId(strconv.Itoa(os.Geteuid()))
	if err != nil || os.Getuid() != os.Geteuid() || os.Geteuid() <= 0 {
		return result, errors.New("local MPI account is unavailable or elevated")
	}
	result.UID, result.User, result.Home = os.Geteuid(), account.Username, account.HomeDir
	identity, err := readOnboardingFile(filepath.Join(account.HomeDir, ".config", "Nvidia Corporation", "Personal AI Router", "cluster", "identity.json"), 8192)
	if err != nil {
		return result, errors.New("local public PAIR account identity is unavailable")
	}
	var public struct {
		NodeID string `json:"node_uuid"`
	}
	if json.Unmarshal(identity, &public) != nil || public.NodeID != target.Principal {
		return result, errors.New("local public PAIR identity changed")
	}
	digest := sha256.Sum256(identity)
	result.PublicIdentitySHA256 = hex.EncodeToString(digest[:])
	interfaces, err := net.Interfaces()
	if err != nil {
		return result, err
	}
	result.Interface, err = diagnosticMPILocalInterface(target.Address, interfaces, func(iface net.Interface) ([]net.Addr, error) { return iface.Addrs() })
	return result, err
}

func (d *diagnosticService) localMPIHostIdentityWithIO(ctx context.Context, target diagnosticManagedTarget, read func(diagnosticManagedTarget) (diagnosticMPILocalHostState, error), exchange func(context.Context, string) (diagnosticBootstrapPublicKey, error)) (diagnosticBootstrapPublicKey, int, error) {
	var empty diagnosticBootstrapPublicKey
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(target.Principal)
	if ctx.Err() != nil || !target.Local || target.NodeID != target.Principal || target.Principal != d.m.mesh.NodeUUID() || !d.m.mesh.Clustered() || !pinned || pin != target.ClusterPinSHA256 || target.CandidateID != "" || target.SSHHostKeySHA256 != "" || !diagnosticConcreteIPv4(target.Address) {
		return empty, 0, errors.New("local MPI server observation changed its adopted participant binding")
	}
	var registration struct {
		Identity struct {
			NodeID               string `json:"nodeId"`
			Principal            string `json:"principal"`
			UID                  int    `json:"uid"`
			User                 string `json:"user"`
			Home                 string `json:"home"`
			PublicIdentitySHA256 string `json:"publicIdentitySha256"`
		} `json:"identity"`
	}
	if json.Unmarshal(target.Registration, &registration) != nil {
		return empty, 0, errors.New("adopted local account identity is unavailable")
	}
	identity := registration.Identity
	before, err := read(target)
	if err != nil || identity.NodeID != target.NodeID || identity.Principal != target.Principal || identity.UID <= 0 || identity.UID != before.UID || !diagnosticToken.MatchString(identity.User) || identity.User != before.User || !diagnosticPath.MatchString(identity.Home) || identity.Home == "/" || identity.Home != before.Home || !diagnosticDigest.MatchString(identity.PublicIdentitySHA256) || identity.PublicIdentitySHA256 != before.PublicIdentitySHA256 || !cableInterfaceValid(before.Interface) {
		return empty, 0, errors.New("local MPI account or interface differs from its registered identity")
	}
	if ctx.Err() != nil {
		return empty, 0, ctx.Err()
	}
	key, err := exchange(ctx, target.Address)
	if err != nil || validateMPIPublicKey(key) != nil {
		return empty, 0, errors.New("active local SSH public identity on port 22 is unconfirmed")
	}
	after, err := read(target)
	d.m.mesh.Refresh()
	current, currentPin := d.m.mesh.PinSHA256(target.Principal)
	if err != nil || before != after || ctx.Err() != nil || !d.m.mesh.Clustered() || d.m.mesh.NodeUUID() != target.Principal || !currentPin || current != pin {
		return empty, 0, errors.New("local MPI account, interface or certificate changed during server observation")
	}
	return key, 22, nil
}

func observeDiagnosticMPILocalHostKey(ctx context.Context, address string) (diagnosticBootstrapPublicKey, error) {
	dialer := net.Dialer{Timeout: 2 * time.Second}
	return exchangeDiagnosticMPILocalHostKey(ctx, address, dialer.DialContext, func(conn net.Conn, endpoint string, config *ssh.ClientConfig) error {
		client, _, _, err := ssh.NewClientConn(conn, endpoint, config)
		if client != nil {
			_ = client.Close()
		}
		return err
	})
}

func exchangeDiagnosticMPILocalHostKey(ctx context.Context, address string, dial func(context.Context, string, string) (net.Conn, error), handshake func(net.Conn, string, *ssh.ClientConfig) error) (diagnosticBootstrapPublicKey, error) {
	var observed diagnosticBootstrapPublicKey
	if !diagnosticConcreteIPv4(address) || ctx.Err() != nil {
		return observed, errors.New("invalid local SSH observation endpoint")
	}
	endpoint := net.JoinHostPort(address, "22")
	conn, err := dial(ctx, "tcp", endpoint)
	if err != nil {
		return observed, errors.New("local SSH port 22 is unavailable")
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	if parent, ok := ctx.Deadline(); ok && parent.Before(deadline) {
		deadline = parent
	}
	if err := conn.SetDeadline(deadline); err != nil {
		return observed, errors.New("local SSH observation deadline is unavailable")
	}
	complete := errors.New("local host-key observation complete")
	config := &ssh.ClientConfig{User: "pair-public-key-observation", HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}, HostKeyCallback: func(host string, remote net.Addr, key ssh.PublicKey) error {
		if host != endpoint || remote == nil || remote.String() != endpoint {
			return errors.New("local SSH server endpoint changed")
		}
		value := diagnosticBootstrapPublicKey{Algorithm: key.Type(), Blob: base64.StdEncoding.EncodeToString(key.Marshal()), Fingerprint: ssh.FingerprintSHA256(key)}
		if err := validateMPIPublicKey(value); err != nil {
			return err
		}
		// x/crypto/ssh verifies the key-exchange signature before this callback.
		// Refusing here prevents authentication, sessions and remote commands.
		// These observed public bytes gain authority only through the pinned
		// PAIR facts handoff and the unchanged registered native account.
		observed = value
		return complete
	}}
	err = handshake(conn, endpoint, config)
	if !errors.Is(err, complete) || observed.Fingerprint == "" || ctx.Err() != nil {
		return diagnosticBootstrapPublicKey{}, errors.New("local SSH signature or public identity was not confirmed")
	}
	return observed, nil
}

type diagnosticMPIFactsReply struct {
	Facts         diagnosticMPIParticipantFacts  `json:"facts"`
	Address       diagnosticMPICollectiveAddress `json:"address"`
	SSHSourceIPv4 string                         `json:"sshSourceIPv4"`
}

type diagnosticMPIInspection struct {
	SchemaVersion  int    `json:"schemaVersion"`
	Action         string `json:"action"`
	EffectsApplied *bool  `json:"effectsApplied"`
	Executable     *bool  `json:"executable"`
	Identity       struct {
		NodeID               string `json:"nodeId"`
		Principal            string `json:"principal"`
		PublicIdentitySHA256 string `json:"publicIdentitySha256"`
	} `json:"identity"`
	Observations struct {
		GPU *struct {
			UUID               string `json:"uuid"`
			GB10Observed       bool   `json:"gb10Observed"`
			UtilizationPercent *int   `json:"utilizationPercent"`
		} `json:"gpu"`
		GPUProcesses *struct {
			Processes []json.RawMessage `json:"processes"`
		} `json:"gpuProcesses"`
		Route *struct {
			PeerAddress   string `json:"peerAddress"`
			SourceAddress string `json:"sourceAddress"`
			Interface     string `json:"interface"`
		} `json:"route"`
	} `json:"observations"`
}

func diagnosticMPIFactsPeers(request diagnosticMPIFactsRequest) ([]string, error) {
	peers := request.PeerAddresses
	if peers == nil {
		peers = []string{request.PeerAddress}
	}
	if len(peers) < 1 || len(peers) > 2 || request.PeerAddress != peers[0] {
		return nil, errors.New("MPI facts require one or two exact selected peer addresses")
	}
	seen := map[string]bool{}
	for _, peer := range peers {
		if !diagnosticConcreteIPv4(peer) || peer == request.SSHAddress || seen[peer] {
			return nil, errors.New("MPI facts peer addresses are invalid or duplicated")
		}
		seen[peer] = true
	}
	return append([]string(nil), peers...), nil
}

func readDiagnosticMPIInspections(ctx context.Context, target diagnosticManagedTarget, publicIdentity string, peers []string, inspect func(context.Context, []byte) ([]byte, error)) (diagnosticMPIInspection, error) {
	var first diagnosticMPIInspection
	if len(peers) < 1 || len(peers) > 2 || !diagnosticDigest.MatchString(publicIdentity) {
		return first, errors.New("MPI inspection roster or original public identity is incomplete")
	}
	for i, peer := range peers {
		if err := ctx.Err(); err != nil {
			return first, err
		}
		input, _ := json.Marshal(map[string]any{"action": "inspect", "nodeId": target.NodeID, "principal": target.Principal, "peerAddress": peer})
		raw, err := inspect(ctx, input)
		if err != nil {
			return first, err
		}
		var observed diagnosticMPIInspection
		if len(raw) > 128<<10 || json.Unmarshal(raw, &observed) != nil {
			return first, errors.New("native inspection returned invalid MPI prerequisite facts")
		}
		facts := observed.Observations
		if observed.SchemaVersion != 1 || observed.Action != "inspect" || observed.EffectsApplied == nil || *observed.EffectsApplied || observed.Executable == nil || *observed.Executable || observed.Identity.NodeID != target.NodeID || observed.Identity.Principal != target.Principal || observed.Identity.PublicIdentitySHA256 != publicIdentity || facts.GPU == nil || !facts.GPU.GB10Observed || facts.GPU.UtilizationPercent == nil || facts.GPUProcesses == nil || facts.GPUProcesses.Processes == nil || len(facts.GPUProcesses.Processes) != 0 || facts.Route == nil || facts.Route.PeerAddress != peer || !diagnosticConcreteIPv4(facts.Route.SourceAddress) || !diagnosticToken.MatchString(facts.Route.Interface) {
			return first, errors.New("fresh GPU ownership, identity, or management route is unconfirmed")
		}
		if i == 0 {
			first = observed
		} else if facts.Route.Interface != first.Observations.Route.Interface || facts.Route.SourceAddress != first.Observations.Route.SourceAddress || facts.GPU.UUID != first.Observations.GPU.UUID {
			return first, errors.New("selected MPI peers do not share one management route and GPU identity")
		}
	}
	if err := ctx.Err(); err != nil {
		return first, err
	}
	return first, nil
}

func readDiagnosticMPIArtifact(filename string) (diagnosticMPIArtifact, error) {
	result := diagnosticMPIArtifact{Path: filename}
	if !diagnosticManagedPath(filename) {
		return result, errors.New("MPI prerequisite path is invalid")
	}
	resolved, trusted, err := diagnosticManagedTrustedInput(filename)
	if err != nil {
		return result, err
	}
	f, err := diagnosticManagedTrustOpen(resolved)
	if err != nil {
		return result, errDiagnosticManagedTrust
	}
	defer f.Close()
	before, err := f.Stat()
	if err != nil || !os.SameFile(trusted, before) || !before.Mode().IsRegular() || before.Mode().Perm()&0022 != 0 || before.Size() <= 0 || before.Size() > 2<<30 {
		return result, errors.New("MPI prerequisite is not a bounded regular file")
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, (2<<30)+1))
	after, statErr := f.Stat()
	if err != nil || statErr != nil || n != before.Size() || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		return result, errors.New("MPI prerequisite changed while hashing")
	}
	finalPath, finalInfo, err := diagnosticManagedTrustedInput(filename)
	if err != nil {
		return result, err
	}
	if finalPath != resolved || !os.SameFile(before, after) || !os.SameFile(after, finalInfo) {
		return result, errors.New("MPI prerequisite path changed while hashing")
	}
	result.SHA256, result.Size = hex.EncodeToString(h.Sum(nil)), n
	return result, nil
}

func (d *diagnosticService) localMPIFacts(ctx context.Context, request diagnosticMPIFactsRequest) (diagnosticMPIFactsReply, error) {
	var reply diagnosticMPIFactsReply
	target := request.Target
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" {
		return reply, errors.New("managed MPI facts require their native Linux ARM64 owner")
	}
	d.m.mesh.Refresh()
	pin, pinned := d.m.mesh.PinSHA256(target.Principal)
	if (request.Network != "management" && request.Network != "fabric") || (request.Network == "fabric" && (target.NodeID != request.Selection.NodeID || target.Principal != request.Selection.Principal || request.Selection.Network != "fabric")) || target.Principal != d.m.mesh.NodeUUID() || !d.m.mesh.Clustered() || !pinned || target.ClusterPinSHA256 != pin {
		return reply, errors.New("managed MPI facts no longer match the adopted participant identity")
	}
	peers, err := diagnosticMPIFactsPeers(request)
	if err != nil || !diagnosticConcreteIPv4(request.SSHAddress) || request.SSHAddress != target.Address || request.SSHPort < 1 || request.SSHPort > 65535 || validateMPIPublicKey(request.SSHHostKey) != nil || !target.Local && request.SSHHostKey.Fingerprint != target.SSHHostKeySHA256 {
		return reply, errors.New("managed MPI SSH route or reviewed public identity changed")
	}
	if target.Local {
		key, port, err := d.localMPIHostIdentity(ctx, target)
		if err != nil || port != request.SSHPort || key != request.SSHHostKey {
			return reply, errors.New("local SSH server differs from the observed review identity")
		}
	}
	var registration diagnosticMPIRegistration
	if json.Unmarshal(target.Registration, &registration) != nil || !onboardingID.MatchString(registration.OperationID) || registration.PlanDigest != target.PlanDigest {
		return reply, errors.New("adopted runtime registration is incomplete")
	}
	local := diagnosticInspectionTarget{NodeID: target.NodeID, Principal: target.Principal, Address: request.SSHAddress, Local: true}
	statusRaw, err := d.runtimeNative(ctx, local, onboardingPrivateTarget{}, map[string]any{"action": "status", "nodeId": target.NodeID, "principal": target.Principal, "operationId": registration.OperationID, "expectedPlanDigest": target.PlanDigest})
	if err != nil {
		return reply, err
	}
	status, err := runtimeResult(statusRaw, "status", registration.OperationID, target.PlanDigest)
	if err != nil || status.State != "built" || !status.CleanupConfirmed || !status.ArtifactsValidated || status.Attempt != target.Attempt || !sameManagedJSON(status.Registration, target.Registration) {
		return reply, errors.New("adopted runtime no longer matches a fresh successful static artifact check")
	}
	// The worker just rehashed these pinned artifacts. Apply the same native
	// ancestry policy now, before publishing a managed MPI review as ready.
	for _, artifact := range []diagnosticMPIArtifact{registration.Binary, registration.NCCLLibrary, registration.Dependencies["libcudart.so.13"], registration.Dependencies["libmpi.so.40"]} {
		if !validMPIArtifact(artifact) {
			return reply, errors.New("managed MPI registration has an invalid pinned input")
		}
		_, info, err := diagnosticManagedTrustedInput(artifact.Path)
		if err != nil {
			return reply, err
		}
		if info.Size() != artifact.Size {
			return reply, errors.New("managed MPI registered input size changed")
		}
	}
	inspection, err := readDiagnosticMPIInspections(ctx, target, registration.Identity.PublicIdentitySHA256, peers, func(ctx context.Context, input []byte) ([]byte, error) {
		return d.participantProgram(ctx, local, onboardingPrivateTarget{}, diagnosticInspectPython, input)
	})
	if err != nil {
		return reply, err
	}
	facts := inspection.Observations
	manager, err := os.Executable()
	if err != nil {
		return reply, err
	}
	managerArtifact, err := readDiagnosticMPIArtifact(manager)
	if err != nil {
		return reply, err
	}
	tools := map[string]diagnosticMPIArtifact{}
	for _, name := range []string{"mpirun", "orted", "ssh", "ssh-agent", "ssh-add", "python3", "systemd-run", "systemctl", "busctl", "nvidia-smi"} {
		tool, err := readDiagnosticMPIArtifact("/usr/bin/" + name)
		if err != nil {
			return reply, err
		}
		tools[name] = tool
	}
	// Read the existing normal API after the slower artifact checks so address
	// freshness is measured at this response, not borrowed from an old inspection.
	f := d.m.cableLocal
	if f == nil || f.nodeID != target.NodeID || f.port < 1 || f.port > 65535 {
		return reply, errors.New("parent-owned PAIR node-info service is unavailable")
	}
	var connections diagnosticMPIConnections
	if err = readCableJSON(ctx, f.http, "http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(f.port))+"/v1/node-info", &connections); err != nil {
		return reply, err
	}
	selected := request.Selection
	if request.Network == "management" {
		selected = diagnosticMPIInterfaceSelection{Network: "management", NodeID: target.NodeID, Principal: target.Principal}
		if connections.Connections == nil {
			return reply, errors.New("current management interface facts are unavailable")
		}
		for _, row := range connections.Connections.Interfaces {
			if row.Name == facts.Route.Interface {
				selected.Interface = cableprobe.Interface{Name: row.Name, Index: row.Index, MAC: row.MAC}
			}
		}
	}
	address, err := projectDiagnosticMPIAddress(connections, selected, time.Now())
	if err != nil {
		return reply, err
	}
	if request.Network == "management" && address.Address != facts.Route.SourceAddress {
		return reply, errors.New("management route and current interface address disagree")
	}
	interfaces := []diagnosticMPIInterfaceObservation{}
	for _, row := range connections.Connections.Interfaces {
		if row.AddressesUnavailable {
			return reply, errors.New("complete interface observations are required for MPI subnet binding")
		}
		ipv4 := []string{}
		for _, value := range row.Addresses {
			ip, _, err := net.ParseCIDR(value)
			if err != nil {
				return reply, errors.New("native interface address is invalid")
			}
			if ip.To4() != nil {
				ipv4 = append(ipv4, value)
			}
		}
		interfaces = append(interfaces, diagnosticMPIInterfaceObservation{Name: row.Name, Up: row.Up, Addresses: ipv4})
	}
	d.m.mesh.Refresh()
	pin, pinned = d.m.mesh.PinSHA256(target.Principal)
	if !d.m.mesh.Clustered() || d.m.mesh.NodeUUID() != target.Principal || !pinned || pin != target.ClusterPinSHA256 {
		return reply, errors.New("participant certificate changed while reading MPI facts")
	}
	if target.Local {
		key, port, keyErr := d.localMPIHostIdentity(ctx, target)
		state, stateErr := readDiagnosticMPILocalHostState(target)
		if keyErr != nil || stateErr != nil || key != request.SSHHostKey || port != request.SSHPort || request.SSHAddress != facts.Route.SourceAddress || state.Interface.Name != facts.Route.Interface || request.Network == "management" && state.Interface != selected.Interface {
			return reply, errors.New("local SSH identity or selected management interface changed during MPI facts")
		}
	}
	reply.Facts = diagnosticMPIParticipantFacts{NodeID: target.NodeID, Principal: target.Principal, ClusterPinSHA256: target.ClusterPinSHA256, ObservedAt: time.Now().UnixMilli(), UID: registration.Identity.UID, User: registration.Identity.User, Home: registration.Identity.Home, PublicIdentitySHA256: registration.Identity.PublicIdentitySHA256, SSHAddress: request.SSHAddress, SSHPort: request.SSHPort, SSHHostKey: request.SSHHostKey, Manager: managerArtifact, Tools: tools, Interface: selected.Interface.Name, CollectiveAddress: address.Address, GPUUUID: facts.GPU.UUID, Interfaces: interfaces, Registration: append([]byte(nil), status.Registration...)}
	reply.Address, reply.SSHSourceIPv4 = address, facts.Route.SourceAddress
	return reply, nil
}

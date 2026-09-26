// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	"nvpair-shared/cableprobe"
)

func nativeCableProbeIO() cableProbeIO {
	type binding struct {
		alias cableprobe.Interface
		port  cableprobe.PortRef
	}
	var mu sync.Mutex
	bindings := map[int]binding{}
	current := func(fd int) (int, error) {
		bound, ok := bindings[fd]
		if !ok {
			return 0, errors.New("cable descriptor has no reviewed native binding")
		}
		if err := nativeCableProbeAliasCurrent(bound.alias, bound.port, net.InterfaceByIndex, nativeCableProbePhysicalPort); err != nil {
			return 0, err
		}
		return bound.alias.Index, nil
	}
	return cableProbeIO{
		validate: func(alias cableprobe.Interface, port cableprobe.PortRef) error {
			return nativeCableProbeAliasCurrent(alias, port, net.InterfaceByIndex, nativeCableProbePhysicalPort)
		},
		lock: nativeCableProbeLock,
		// Protocol zero keeps reception disabled until the reviewed bind.
		open: func() (int, error) {
			return unix.Socket(unix.AF_PACKET, unix.SOCK_RAW|unix.SOCK_CLOEXEC|unix.SOCK_NONBLOCK, 0)
		},
		configure: func(fd int, alias cableprobe.Interface, port cableprobe.PortRef) error {
			mu.Lock()
			defer mu.Unlock()
			if _, exists := bindings[fd]; exists || len(bindings) >= 8 {
				return errors.New("cable descriptor binding is duplicate or exceeds the native bound")
			}
			if err := nativeCableProbeAliasCurrent(alias, port, net.InterfaceByIndex, nativeCableProbePhysicalPort); err != nil {
				return err
			}
			if err := nativeCableProbeConfigure(fd, alias.Index); err != nil {
				return err
			}
			if err := nativeCableProbeAliasCurrent(alias, port, net.InterfaceByIndex, nativeCableProbePhysicalPort); err != nil {
				return err
			}
			bindings[fd] = binding{alias: alias, port: port}
			return nil
		},
		read: func(fds []int, wait time.Duration) (cableProbePacket, error) {
			mu.Lock()
			defer mu.Unlock()
			return nativeCableProbeRead(fds, wait, current)
		},
		write: func(fd int, data []byte) error {
			mu.Lock()
			defer mu.Unlock()
			index, err := current(fd)
			if err != nil {
				return err
			}
			frame, err := cableprobe.DecodeFrame(data)
			if err != nil {
				return err
			}
			if !strings.EqualFold(frame.SourceMAC, bindings[fd].alias.MAC) {
				return errors.New("cable frame source no longer matches its reviewed native alias")
			}
			return nativeCableProbeWrite(fd, data, index)
		},
		close: func(fd int) error {
			mu.Lock()
			defer mu.Unlock()
			delete(bindings, fd)
			return unix.Close(fd)
		},
	}
}

// The injected readers make scope validation testable without opening sockets.
// Production supplies the OS index lookup and the fixed sysfs reader below.
func nativeCableProbeAliasCurrent(alias cableprobe.Interface, port cableprobe.PortRef, lookup func(int) (*net.Interface, error), physicalPort func(string) (string, string, error)) error {
	invalid := errors.New("reviewed cable alias or native physical port changed")
	if !cableInterfaceValid(alias) || !cableIdentifier(port.NodeID, 128) || !cableIdentifier(port.SwitchID, 128) || !cableIdentifier(port.PortName, 128) {
		return invalid
	}
	actual, err := lookup(alias.Index)
	if err != nil || actual == nil || actual.Index != alias.Index || actual.Name != alias.Name || !strings.EqualFold(actual.HardwareAddr.String(), alias.MAC) {
		return invalid
	}
	switchID, portName, err := physicalPort(actual.Name)
	if err != nil || !strings.EqualFold(switchID, port.SwitchID) || portName != port.PortName {
		return invalid
	}
	// The index may have been removed/reused while sysfs was read.
	after, err := lookup(alias.Index)
	if err != nil || after == nil || after.Index != alias.Index || after.Name != alias.Name || !strings.EqualFold(after.HardwareAddr.String(), alias.MAC) {
		return invalid
	}
	return nil
}

func nativeCableProbePhysicalPort(name string) (string, string, error) {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "", "", errors.New("invalid native interface name")
	}
	dir := filepath.Join("/sys/class/net", name)
	device, err := os.Stat(filepath.Join(dir, "device"))
	if err != nil || !device.IsDir() {
		return "", "", errors.New("native physical device is unavailable")
	}
	read := func(attribute string) (string, error) {
		file, err := os.Open(filepath.Join(dir, attribute))
		if err != nil {
			return "", err
		}
		defer file.Close()
		data, err := io.ReadAll(io.LimitReader(file, 257))
		if err != nil || len(data) > 256 {
			return "", errors.New("native physical-port attribute is unavailable or oversized")
		}
		return strings.TrimSpace(string(data)), nil
	}
	switchID, err := read("phys_switch_id")
	if err != nil {
		return "", "", err
	}
	portName, err := read("phys_port_name")
	return switchID, portName, err
}

func nativeCableProbeLock() (int, error) {
	// This first worker supports an explicitly approved root-owned one-shot
	// launch only. Reject ordinary users before creating even the lock inode.
	if unix.Geteuid() != 0 {
		return -1, errCableProbePermission
	}
	fd, err := unix.Open("/run/lock/nvpair-cable-probe.lock", unix.O_RDWR|unix.O_CREAT|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, 0600)
	if err != nil {
		return -1, err
	}
	var stat unix.Stat_t
	err = unix.Fstat(fd, &stat)
	if err == nil && (stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Mode&0777 != 0600 || stat.Uid != uint32(unix.Geteuid()) || stat.Nlink != 1 || stat.Size != 0) {
		err = errors.New("cable reservation requires an empty owner-only regular lock file")
	}
	if err == nil {
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
	}
	if err != nil {
		return -1, errors.Join(err, unix.Close(fd))
	}
	// Keep this inode: unlinking lets another opener lock a different file while
	// a prior holder still owns the old inode. Closing the descriptor releases it.
	return fd, nil
}

func nativeCableProbeProtocol() uint16 {
	return binary.NativeEndian.Uint16([]byte{0x88, 0xcc})
}

func nativeCableProbeConfigure(fd, index int) error {
	if index <= 0 || index > 0x7fffffff {
		return errors.New("invalid reviewed cable interface index")
	}
	// The NEW timestamp ABI carries two signed 64-bit fields on every Linux
	// architecture. A kernel lacking it fails preparation without a time fallback.
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS_NEW, 1); err != nil {
		return err
	}
	member := unix.PacketMreq{Ifindex: int32(index), Type: unix.PACKET_MR_MULTICAST, Alen: 6, Address: [8]byte{1, 128, 194, 0, 0, 14}}
	if err := unix.SetsockoptPacketMreq(fd, unix.SOL_PACKET, unix.PACKET_ADD_MEMBERSHIP, &member); err != nil {
		return err
	}
	return unix.Bind(fd, &unix.SockaddrLinklayer{Protocol: nativeCableProbeProtocol(), Ifindex: index})
}

func nativeCableProbeRead(fds []int, wait time.Duration, current func(int) (int, error)) (cableProbePacket, error) {
	if len(fds) == 0 || len(fds) > 8 {
		return cableProbePacket{}, errors.New("invalid reviewed cable descriptor count")
	}
	poll := make([]unix.PollFd, len(fds))
	for i, fd := range fds {
		if fd < 0 || int64(fd) > 0x7fffffff {
			return cableProbePacket{}, errors.New("invalid cable descriptor")
		}
		if _, err := current(fd); err != nil {
			return cableProbePacket{}, err
		}
		poll[i] = unix.PollFd{Fd: int32(fd), Events: unix.POLLIN}
	}
	ready, err := unix.Poll(poll, int(min(max(wait, 0), cableProbePollInterval)/time.Millisecond))
	if errors.Is(err, unix.EINTR) {
		return cableProbePacket{}, errCableProbeIdle
	}
	if err != nil {
		return cableProbePacket{}, err
	}
	if ready == 0 {
		return cableProbePacket{}, errCableProbeIdle
	}
	for _, polled := range poll {
		if polled.Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return cableProbePacket{}, errors.New("cable descriptor is no longer usable")
		}
		if polled.Revents&unix.POLLIN == 0 {
			continue
		}
		index, err := current(int(polled.Fd))
		if err != nil {
			return cableProbePacket{}, err
		}
		data, control := make([]byte, 1518), make([]byte, unix.CmsgSpace(16))
		n, controlN, flags, from, err := unix.Recvmsg(int(polled.Fd), data, control, unix.MSG_DONTWAIT|unix.MSG_TRUNC)
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return cableProbePacket{}, err
		}
		link, ok := from.(*unix.SockaddrLinklayer)
		if !ok || link.Ifindex != index || link.Protocol != nativeCableProbeProtocol() {
			return cableProbePacket{}, errors.New("kernel cable ingress metadata is unavailable")
		}
		packet := cableProbePacket{Data: data[:min(n, len(data))], Index: link.Ifindex, Kind: link.Pkttype,
			Truncated: flags&(unix.MSG_TRUNC|unix.MSG_CTRUNC) != 0 || n > len(data)}
		if packet.Truncated {
			return packet, nil
		}
		messages, err := unix.ParseSocketControlMessage(control[:controlN])
		if err != nil || len(messages) != 1 || messages[0].Header.Level != unix.SOL_SOCKET || messages[0].Header.Type != unix.SO_TIMESTAMPNS_NEW || len(messages[0].Data) != 16 {
			return cableProbePacket{}, errors.New("kernel cable receive timestamp is unavailable")
		}
		var stamp unix.KernelTimespec
		if _, err := binary.Decode(messages[0].Data, binary.NativeEndian, &stamp); err != nil || stamp.Sec <= 0 || stamp.Nsec < 0 || stamp.Nsec >= int64(time.Second) {
			return cableProbePacket{}, errors.New("kernel cable receive timestamp is invalid")
		}
		packet.ReceivedAt = time.Unix(stamp.Sec, stamp.Nsec)
		if packet.ReceivedAt.After(time.Now()) {
			return cableProbePacket{}, errors.New("kernel cable receive timestamp is in the future")
		}
		return packet, nil
	}
	return cableProbePacket{}, errCableProbeIdle
}

func nativeCableProbeWrite(fd int, data []byte, index int) error {
	frame, err := cableprobe.DecodeFrame(data)
	if err != nil {
		return err
	}
	expected, err := cableprobe.EncodeFrame(frame.SourceMAC, frame.RunMarker, frame.Sequence)
	if err != nil || !bytes.Equal(data, expected) {
		return errors.New("cable write requires an internally encoded fixed-profile frame")
	}
	address, err := unix.Getsockname(fd)
	if err != nil {
		return err
	}
	bound, ok := address.(*unix.SockaddrLinklayer)
	if !ok || bound.Ifindex != index || index <= 0 || bound.Protocol != nativeCableProbeProtocol() {
		return errors.New("cable descriptor is not bound to a reviewed LLDP interface")
	}
	// AF_PACKET SOCK_RAW uses the complete validated Ethernet header and this
	// socket's bound interface; there is no caller-supplied destination sockaddr.
	n, err := unix.Write(fd, data)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package main

import (
	"github.com/go-ole/go-ole"
	"github.com/go-ole/go-ole/oleutil"
)

type nativeWindowsFirewallAPI struct{}

func newNativeWindowsFirewallAPI() windowsFirewallAPI {
	return nativeWindowsFirewallAPI{}
}

func (nativeWindowsFirewallAPI) Rules() ([]windowsFirewallRule, error) {
	var result []windowsFirewallRule
	err := withWindowsFirewallRules(func(rules *ole.IDispatch) error {
		return oleutil.ForEach(rules, func(value *ole.VARIANT) error {
			dispatch := value.ToIDispatch()
			if dispatch == nil {
				return ErrFirewallUnavailable
			}
			defer dispatch.Release()
			name, err := firewallStringProperty(dispatch, "Name")
			if err != nil || name != windowsFirewallRuleName {
				return err
			}
			rule := windowsFirewallRule{Name: name}
			if rule.Enabled, err = firewallBoolProperty(dispatch, "Enabled"); err != nil {
				return err
			}
			if rule.Direction, err = firewallIntProperty(dispatch, "Direction"); err != nil {
				return err
			}
			if rule.Profiles, err = firewallIntProperty(dispatch, "Profiles"); err != nil {
				return err
			}
			if rule.Protocol, err = firewallIntProperty(dispatch, "Protocol"); err != nil {
				return err
			}
			if rule.LocalPorts, err = firewallStringProperty(dispatch, "LocalPorts"); err != nil {
				return err
			}
			if rule.RemotePorts, err = firewallStringProperty(dispatch, "RemotePorts"); err != nil {
				return err
			}
			if rule.LocalAddresses, err = firewallStringProperty(dispatch, "LocalAddresses"); err != nil {
				return err
			}
			if rule.RemoteAddresses, err = firewallStringProperty(dispatch, "RemoteAddresses"); err != nil {
				return err
			}
			if rule.ApplicationName, err = firewallStringProperty(dispatch, "ApplicationName"); err != nil {
				return err
			}
			if rule.ServiceName, err = firewallStringProperty(dispatch, "ServiceName"); err != nil {
				return err
			}
			if rule.InterfaceTypes, err = firewallStringProperty(dispatch, "InterfaceTypes"); err != nil {
				return err
			}
			if rule.Interfaces, err = firewallStringProperty(dispatch, "Interfaces"); err != nil {
				return err
			}
			if rule.IcmpTypesAndCodes, err = firewallStringProperty(dispatch, "IcmpTypesAndCodes"); err != nil {
				return err
			}
			if rule.EdgeTraversal, err = firewallBoolProperty(dispatch, "EdgeTraversal"); err != nil {
				return err
			}
			if rule.EdgeTraversalOptions, err = firewallIntProperty(dispatch, "EdgeTraversalOptions"); err != nil {
				return err
			}
			if rule.Grouping, err = firewallStringProperty(dispatch, "Grouping"); err != nil {
				return err
			}
			if rule.LocalAppPackageID, err = firewallStringProperty(dispatch, "LocalAppPackageId"); err != nil {
				return err
			}
			if rule.LocalUserOwner, err = firewallStringProperty(dispatch, "LocalUserOwner"); err != nil {
				return err
			}
			if rule.LocalUserAuthorizedList, err = firewallStringProperty(dispatch, "LocalUserAuthorizedList"); err != nil {
				return err
			}
			if rule.RemoteUserAuthorizedList, err = firewallStringProperty(dispatch, "RemoteUserAuthorizedList"); err != nil {
				return err
			}
			if rule.RemoteMachineAuthorizedList, err = firewallStringProperty(dispatch, "RemoteMachineAuthorizedList"); err != nil {
				return err
			}
			if rule.SecureFlags, err = firewallIntProperty(dispatch, "SecureFlags"); err != nil {
				return err
			}
			if rule.Action, err = firewallIntProperty(dispatch, "Action"); err != nil {
				return err
			}
			result = append(result, rule)
			return nil
		})
	})
	return result, err
}

func (nativeWindowsFirewallAPI) Add(rule windowsFirewallRule) error {
	return withWindowsFirewallRules(func(rules *ole.IDispatch) error {
		unknown, err := oleutil.CreateObject("HNetCfg.FWRule")
		if err != nil {
			return err
		}
		defer unknown.Release()
		dispatch, err := unknown.QueryInterface(ole.IID_IDispatch)
		if err != nil {
			return err
		}
		defer dispatch.Release()
		for property, value := range map[string]any{
			"Name":                        rule.Name,
			"Enabled":                     rule.Enabled,
			"Direction":                   rule.Direction,
			"Profiles":                    rule.Profiles,
			"Protocol":                    rule.Protocol,
			"LocalPorts":                  rule.LocalPorts,
			"RemotePorts":                 rule.RemotePorts,
			"LocalAddresses":              rule.LocalAddresses,
			"RemoteAddresses":             rule.RemoteAddresses,
			"ApplicationName":             rule.ApplicationName,
			"ServiceName":                 rule.ServiceName,
			"InterfaceTypes":              rule.InterfaceTypes,
			"IcmpTypesAndCodes":           rule.IcmpTypesAndCodes,
			"EdgeTraversal":               rule.EdgeTraversal,
			"EdgeTraversalOptions":        rule.EdgeTraversalOptions,
			"Grouping":                    rule.Grouping,
			"LocalAppPackageId":           rule.LocalAppPackageID,
			"LocalUserOwner":              rule.LocalUserOwner,
			"LocalUserAuthorizedList":     rule.LocalUserAuthorizedList,
			"RemoteUserAuthorizedList":    rule.RemoteUserAuthorizedList,
			"RemoteMachineAuthorizedList": rule.RemoteMachineAuthorizedList,
			"SecureFlags":                 rule.SecureFlags,
			"Action":                      rule.Action,
		} {
			if _, err := oleutil.PutProperty(dispatch, property, value); err != nil {
				return err
			}
		}
		_, err = oleutil.CallMethod(rules, "Add", dispatch)
		return err
	})
}

func (nativeWindowsFirewallAPI) Remove(name string) error {
	return withWindowsFirewallRules(func(rules *ole.IDispatch) error {
		_, err := oleutil.CallMethod(rules, "Remove", name)
		return err
	})
}

func withWindowsFirewallRules(operation func(*ole.IDispatch) error) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil {
		return err
	}
	defer ole.CoUninitialize()
	unknown, err := oleutil.CreateObject("HNetCfg.FwPolicy2")
	if err != nil {
		return err
	}
	defer unknown.Release()
	policy, err := unknown.QueryInterface(ole.IID_IDispatch)
	if err != nil {
		return err
	}
	defer policy.Release()
	value, err := oleutil.GetProperty(policy, "Rules")
	if err != nil {
		return err
	}
	defer value.Clear()
	rules := value.ToIDispatch()
	if rules == nil {
		return ErrFirewallUnavailable
	}
	defer rules.Release()
	return operation(rules)
}

func firewallStringProperty(dispatch *ole.IDispatch, name string) (string, error) {
	value, err := oleutil.GetProperty(dispatch, name)
	if err != nil {
		return "", err
	}
	defer value.Clear()
	return value.ToString(), nil
}

func firewallBoolProperty(dispatch *ole.IDispatch, name string) (bool, error) {
	value, err := oleutil.GetProperty(dispatch, name)
	if err != nil {
		return false, err
	}
	defer value.Clear()
	return value.Val != 0, nil
}

func firewallIntProperty(dispatch *ole.IDispatch, name string) (int32, error) {
	value, err := oleutil.GetProperty(dispatch, name)
	if err != nil {
		return 0, err
	}
	defer value.Clear()
	return int32(value.Val), nil
}

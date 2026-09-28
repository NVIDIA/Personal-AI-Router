// SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package main

import (
	"context"
	"os"
	"strings"
	"syscall"

	"nvpair-shared/hostbootstrap"
)

func probeLinuxFirewallNative(
	ctx context.Context,
	runner commandRunner,
	request hostbootstrap.Request,
) (nativeProbe, error) {
	config, err := readRootFirewallFile("/etc/ufw/ufw.conf")
	manager, managerErr := classifyLinuxFirewallManager(config, err)
	if managerErr != nil {
		return nativeProbe{Unavailable: true}, nil
	}
	if manager == linuxFirewallUFW {
		profiles, err := ufwFamilyProfiles()
		if err != nil {
			return nativeProbe{Unavailable: true}, nil
		}
		present := false
		exact := true
		identityExact := true
		for _, profile := range profiles {
			rules, err := readRootFirewallFile(profile.rulesPath)
			if err != nil {
				return nativeProbe{Unavailable: true}, nil
			}
			configPresent, configExact, parseErr :=
				parseUFWOwnedConfigRule(rules, profile.family)
			if parseErr != nil {
				return nativeProbe{Unavailable: true}, nil
			}
			runtime, err := runner.Run(ctx, profile.saveCommand)
			if err != nil {
				return nativeProbe{Unavailable: true}, nil
			}
			runtimePresent, runtimeExact, parseErr :=
				parseUFWRuntimeRule(runtime, profile.family)
			if parseErr != nil {
				return nativeProbe{Unavailable: true}, nil
			}
			present = present || configPresent || runtimePresent
			exact = exact && configExact && runtimeExact
			identityExact = identityExact &&
				(!configPresent || configExact) &&
				(!runtimePresent || runtimeExact)
		}
		return linuxFirewallProbe(
			request,
			present,
			exact,
			identityExact,
		), nil
	}
	rule, err := firewalldOwnedRuleForEndpoint(request.Binding.Endpoint.Address)
	if err != nil {
		return nativeProbe{Unavailable: true}, nil
	}
	status, statusErr := runner.Run(ctx, commandSpec{
		Path: "/usr/bin/systemctl",
		Args: []string{"show", "firewalld", "--property=LoadState", "--property=ActiveState"},
	})
	if statusErr != nil {
		return nativeProbe{Unavailable: true}, nil
	}
	if strings.Contains(status, "LoadState=not-found") || strings.Contains(status, "ActiveState=inactive") {
		return nativeProbe{Available: true, Exact: true, NotApplicable: true}, nil
	}
	if !strings.Contains(status, "LoadState=loaded") || !strings.Contains(status, "ActiveState=active") {
		return nativeProbe{Unavailable: true}, nil
	}
	var states [2][2]bool
	for index, arguments := range [][]string{
		{"--list-rich-rules"},
		{"--permanent", "--list-rich-rules"},
	} {
		output, err := runner.Run(ctx, commandSpec{Path: "/usr/bin/firewall-cmd", Args: arguments})
		if err != nil {
			return nativeProbe{Unavailable: true}, nil
		}
		present, exact, err := parseFirewalldOwnedRule(output, rule)
		if err != nil {
			return nativeProbe{Unavailable: true}, nil
		}
		states[index] = [2]bool{present, exact}
	}
	present := states[0][0] || states[1][0]
	identityExact := (!states[0][0] || states[0][1]) &&
		(!states[1][0] || states[1][1])
	return linuxFirewallProbe(
		request,
		present,
		states[0][1] && states[1][1],
		identityExact,
	), nil
}

func readRootFirewallFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok ||
		!info.Mode().IsRegular() ||
		info.Mode()&os.ModeSymlink != 0 ||
		info.Mode().Perm()&0022 != 0 ||
		stat.Uid != 0 ||
		stat.Nlink != 1 {
		return nil, ErrUnsafeState
	}
	return readBoundedRegularFile(path, 1<<20)
}

func linuxFirewallProbe(
	request hostbootstrap.Request,
	present, exact, identityExact bool,
) nativeProbe {
	identity := ""
	if exact {
		identity = resourceIdentitySHA256(hostbootstrap.ResourceFirewall, request.Binding)
	} else if present {
		identity = strings.Repeat("0", 64)
	}
	return nativeProbe{
		Available:      true,
		Present:        present,
		Exact:          exact,
		IdentitySHA256: identity,
		IdentityExact:  identityExact,
	}
}

func linuxFirewallNativeEffects(
	ctx context.Context,
	runner commandRunner,
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	config, readErr := readRootFirewallFile("/etc/ufw/ufw.conf")
	manager, err := classifyLinuxFirewallManager(config, readErr)
	if err != nil {
		return nil, err
	}
	if manager == linuxFirewallUFW {
		profiles, err := ufwFamilyProfiles()
		if err != nil {
			return nil, err
		}
		inspectProfiles := func() (bool, bool, error) {
			present := false
			for _, profile := range profiles {
				rules, err := readRootFirewallFile(profile.rulesPath)
				if err != nil {
					return false, false, ErrFirewallUnavailable
				}
				configPresent, configExact, err :=
					parseUFWOwnedConfigRule(rules, profile.family)
				if err != nil {
					return false, false, ErrFirewallUnavailable
				}
				output, err := runner.Run(ctx, profile.saveCommand)
				if err != nil {
					return false, false, ErrFirewallUnavailable
				}
				runtimePresent, runtimeExact, err :=
					parseUFWRuntimeRule(output, profile.family)
				if err != nil {
					return false, false, ErrFirewallUnavailable
				}
				if (configPresent && !configExact) ||
					(runtimePresent && !runtimeExact) {
					return true, false, ErrForeignCollision
				}
				present = present || configPresent || runtimePresent
			}
			return present, true, nil
		}
		return []nativeEffect{
			{
				ID:         "firewall:ufw-repair-cleanup",
				ReplaySafe: true,
				Satisfied: func() (bool, error) {
					present, _, err := inspectProfiles()
					return !present, err
				},
				Apply: func() error {
					_, err := runner.Run(ctx, commandSpec{
						Path: "/usr/sbin/ufw",
						Args: []string{
							"delete",
							"allow",
							"in",
							"proto",
							"tcp",
							"to",
							"any",
							"port",
							"22",
							"comment",
							windowsFirewallRuleName,
						},
					})
					if err != nil {
						return err
					}
					present, _, err := inspectProfiles()
					if err != nil {
						return err
					}
					if present {
						return ErrVerification
					}
					return nil
				},
			},
			{
				ID: "firewall:ufw-config-add",
				Satisfied: func() (bool, error) {
					for _, profile := range profiles {
						rules, err := readRootFirewallFile(
							profile.rulesPath,
						)
						if err != nil {
							return false, ErrFirewallUnavailable
						}
						present, exact, err :=
							parseUFWOwnedConfigRule(
								rules,
								profile.family,
							)
						if err != nil {
							return false, ErrFirewallUnavailable
						}
						if present && !exact {
							return false, ErrForeignCollision
						}
						if !exact {
							return false, nil
						}
					}
					return true, nil
				},
				Apply: func() error {
					_, err := runner.Run(ctx, commandSpec{
						Path: "/usr/sbin/ufw",
						Args: []string{
							"allow",
							"in",
							"proto",
							"tcp",
							"to",
							"any",
							"port",
							"22",
							"comment",
							windowsFirewallRuleName,
						},
					})
					return err
				},
			},
			{
				ID:         "firewall:ufw-runtime-restore",
				ReplaySafe: true,
				Satisfied: func() (bool, error) {
					for _, profile := range profiles {
						output, err := runner.Run(
							ctx,
							profile.saveCommand,
						)
						if err != nil {
							return false, ErrFirewallUnavailable
						}
						present, exact, err := parseUFWRuntimeRule(
							output,
							profile.family,
						)
						if err != nil {
							return false, ErrFirewallUnavailable
						}
						if present && !exact {
							return false, ErrForeignCollision
						}
						if !exact {
							return false, nil
						}
					}
					return true, nil
				},
				Apply: func() error {
					_, err := runner.Run(ctx, commandSpec{
						Path: "/usr/sbin/ufw",
						Args: []string{"reload"},
					})
					return err
				},
			},
		}, nil
	}
	rule, err := firewalldOwnedRuleForEndpoint(
		request.Binding.Endpoint.Address,
	)
	if err != nil {
		return nil, err
	}
	return firewalldNativeEffects(ctx, runner, rule), nil
}

func removeLinuxFirewallNative(
	ctx context.Context,
	runner commandRunner,
	request hostbootstrap.Request,
) error {
	config, err := readRootFirewallFile("/etc/ufw/ufw.conf")
	manager, err := classifyLinuxFirewallManager(config, err)
	if err != nil {
		return err
	}
	if manager == linuxFirewallUFW {
		_, err = runner.Run(ctx, commandSpec{
			Path: "/usr/sbin/ufw",
			Args: []string{"delete", "allow", "in", "proto", "tcp", "to", "any", "port", "22", "comment", windowsFirewallRuleName},
		})
		if err != nil {
			return err
		}
		profiles, err := ufwFamilyProfiles()
		if err != nil {
			return err
		}
		for _, profile := range profiles {
			rules, err := readRootFirewallFile(profile.rulesPath)
			if err != nil {
				return err
			}
			configPresent, _, err := parseUFWOwnedConfigRule(
				rules,
				profile.family,
			)
			if err != nil {
				return err
			}
			output, err := runner.Run(ctx, profile.saveCommand)
			if err != nil {
				return err
			}
			runtimePresent, _, err := parseUFWRuntimeRule(
				output,
				profile.family,
			)
			if err != nil {
				return err
			}
			if configPresent || runtimePresent {
				return ErrVerification
			}
		}
		return nil
	}
	rule, err := firewalldOwnedRuleForEndpoint(request.Binding.Endpoint.Address)
	if err != nil {
		return err
	}
	if _, err = runner.Run(ctx, commandSpec{
		Path: "/usr/bin/firewall-cmd",
		Args: []string{"--remove-rich-rule", rule},
	}); err != nil {
		return err
	}
	if _, err = runner.Run(ctx, commandSpec{
		Path: "/usr/bin/firewall-cmd",
		Args: []string{"--permanent", "--remove-rich-rule", rule},
	}); err != nil {
		_, _ = runner.Run(ctx, commandSpec{
			Path: "/usr/bin/firewall-cmd",
			Args: []string{"--add-rich-rule", rule},
		})
		return err
	}
	for _, arguments := range [][]string{
		{"--list-rich-rules"},
		{"--permanent", "--list-rich-rules"},
	} {
		output, err := runner.Run(ctx, commandSpec{
			Path: "/usr/bin/firewall-cmd",
			Args: arguments,
		})
		if err != nil {
			return err
		}
		present, _, err := parseFirewalldOwnedRule(output, rule)
		if err != nil {
			return err
		}
		if present {
			return ErrVerification
		}
	}
	return nil
}

func linuxFirewallRemovalEffects(
	ctx context.Context,
	runner commandRunner,
	request hostbootstrap.Request,
) ([]nativeEffect, error) {
	config, err := readRootFirewallFile("/etc/ufw/ufw.conf")
	manager, err := classifyLinuxFirewallManager(config, err)
	if err != nil {
		return nil, err
	}
	if manager == linuxFirewallUFW {
		return []nativeEffect{{
			ID: "uninstall:firewall:ufw-delete",
			Satisfied: func() (bool, error) {
				probe, err := probeLinuxFirewallNative(ctx, runner, request)
				return !probe.Present, err
			},
			Apply: func() error {
				_, err := runner.Run(ctx, commandSpec{
					Path: "/usr/sbin/ufw",
					Args: []string{
						"delete", "allow", "in", "proto", "tcp",
						"to", "any", "port", "22", "comment",
						windowsFirewallRuleName,
					},
				})
				return err
			},
		}}, nil
	}
	rule, err := firewalldOwnedRuleForEndpoint(
		request.Binding.Endpoint.Address,
	)
	if err != nil {
		return nil, err
	}
	effect := func(
		id string,
		permanent bool,
	) nativeEffect {
		arguments := []string{"--list-rich-rules"}
		remove := []string{"--remove-rich-rule", rule}
		if permanent {
			arguments = append([]string{"--permanent"}, arguments...)
			remove = append([]string{"--permanent"}, remove...)
		}
		return nativeEffect{
			ID: id,
			Satisfied: func() (bool, error) {
				output, err := runner.Run(ctx, commandSpec{
					Path: "/usr/bin/firewall-cmd",
					Args: arguments,
				})
				if err != nil {
					return false, err
				}
				present, _, err := parseFirewalldOwnedRule(output, rule)
				return !present, err
			},
			Apply: func() error {
				_, err := runner.Run(ctx, commandSpec{
					Path: "/usr/bin/firewall-cmd",
					Args: remove,
				})
				return err
			},
		}
	}
	return []nativeEffect{
		effect("uninstall:firewall:runtime", false),
		effect("uninstall:firewall:permanent", true),
	}, nil
}

type ufwFamilyState struct {
	family      string
	rulesPath   string
	saveCommand commandSpec
}

func ufwFamilyProfiles() ([]ufwFamilyState, error) {
	defaults, err := readRootFirewallFile("/etc/default/ufw")
	if err != nil {
		return nil, err
	}
	ipv6, known := parseUFWIPv6(defaults)
	if !known {
		return nil, ErrFirewallUnavailable
	}
	profiles := []ufwFamilyState{{
		family:      "ipv4",
		rulesPath:   "/etc/ufw/user.rules",
		saveCommand: commandSpec{Path: "/usr/sbin/iptables-save"},
	}}
	if ipv6 {
		profiles = append(profiles, ufwFamilyState{
			family:      "ipv6",
			rulesPath:   "/etc/ufw/user6.rules",
			saveCommand: commandSpec{Path: "/usr/sbin/ip6tables-save"},
		})
	}
	return profiles, nil
}

func linuxFirewallOwnedFamiliesNative(
	request hostbootstrap.Request,
) ([]string, error) {
	config, err := readRootFirewallFile("/etc/ufw/ufw.conf")
	manager, err := classifyLinuxFirewallManager(config, err)
	if err != nil {
		return nil, err
	}
	if manager == linuxFirewallUFW {
		profiles, err := ufwFamilyProfiles()
		if err != nil {
			return nil, err
		}
		families := make([]string, 0, len(profiles))
		for _, profile := range profiles {
			families = append(families, profile.family)
		}
		return families, nil
	}
	family, err := endpointAddressFamily(request.Binding.Endpoint.Address)
	if err != nil {
		return nil, err
	}
	return []string{family}, nil
}

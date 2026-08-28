<!--
SPDX-FileCopyrightText: Copyright (c) 2026 NVIDIA CORPORATION & AFFILIATES. All rights reserved.
SPDX-License-Identifier: Apache-2.0
-->

# Bill of Materials — Third-Party Go Libraries

Scope: dependencies linked into the thirteen shipped binaries (`ollama-proxy`, `lmstudio-proxy`, `nvpair-node-info`, `nvpair-node-scanner`, `nvpair-manual-nodes`, `nvpair-workload-manager`, `nvpair-errors`, `nvpair-node-settings`, `nvpair-cluster-manager`, `nvpair-ui-broker`, `nvpair-engine-manager`, `nvpair-job-scheduler`, `nvpair-tui`). The local modules `nvpair-shared` and `eapnoob` (the EAP-NOOB implementation under `eap-noob/`, linked by `nvpair-cluster-manager`) are first-party and excluded. The `tests/`, `mdns-test/`, and `broker-test-driver/` modules are development-only and excluded.

`nvpair-tui` is the only component that links the Bubble Tea terminal-UI stack (`charmbracelet/bubbletea` + `lipgloss` + `bubbles`); its transitive `charmbracelet/*`, `muesli/*`, `mattn/*`, `clipperhouse/*`, `atotto/clipboard`, `aymanbagabas/go-osc52`, `lucasb-eyer/go-colorful`, `erikgeiser/coninput`, and `xo/terminfo` dependencies are unique to it.

Where a library is selected at multiple versions across components (Go MVS picks the highest required, per-module), all linked versions are listed separated by `/`. A range with `–` denotes every release in the inclusive interval.

The workers are pure Go services with no GUI/native-webview dependencies, so there are no system-package native dependencies to track beyond the Go libraries below. Build tags keep platform-specific packages out of other targets; in particular, `nvpair-node-info` links the purego-backed gopsutil collector only on macOS.

`nvpair-engine-manager` links no third-party library that the other binaries don't already (only `go-winio` for its Windows named-pipe IPC, plus `golang.org/x/sys`, which it uses directly on Windows to resolve the PID owning a listening port when reclaiming an orphaned managed engine).

As of the mDNS dedup, `grandcat/zeroconf`, `miekg/dns`, and `golang.org/x/net` are linked into the mDNS services **through the first-party `nvpair-shared/mdns` (responder) and `nvpair-shared/discovery` (browser) packages** rather than imported directly by each binary; the Used-By lists below reflect the binaries they end up linked into. Advertising uses only `miekg/dns` + `x/net` (a custom responder); browsing additionally uses `zeroconf`.

## Direct Dependencies

| Library | Version | Used By | License | License URL |
|---------|---------|---------|---------|-------------|
| `github.com/Microsoft/go-winio` | v0.6.2 | ollama-proxy, lmstudio-proxy, nvpair-node-scanner, nvpair-manual-nodes, nvpair-node-settings, nvpair-cluster-manager, nvpair-ui-broker, nvpair-workload-manager, nvpair-errors, nvpair-engine-manager, nvpair-job-scheduler | MIT | [LICENSE](https://github.com/microsoft/go-winio/blob/main/LICENSE) |
| `github.com/charmbracelet/bubbles` | v1.0.0 | nvpair-tui | MIT | [LICENSE](https://github.com/charmbracelet/bubbles/blob/master/LICENSE) |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | nvpair-tui | MIT | [LICENSE](https://github.com/charmbracelet/bubbletea/blob/master/LICENSE) |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | nvpair-tui | MIT | [LICENSE](https://github.com/charmbracelet/lipgloss/blob/master/LICENSE) |
| `github.com/grandcat/zeroconf` | v1.0.0 | ollama-proxy, lmstudio-proxy, nvpair-node-scanner, nvpair-errors, nvpair-cluster-manager | MIT | [LICENSE](https://github.com/grandcat/zeroconf/blob/master/LICENSE) |
| `github.com/jaypipes/ghw` | v0.24.0 | nvpair-node-info | Apache-2.0 | [COPYING](https://github.com/jaypipes/ghw/blob/main/COPYING) |
| `github.com/miekg/dns` | v1.1.55 / v1.1.72 | ollama-proxy, lmstudio-proxy, nvpair-node-scanner, nvpair-errors, nvpair-cluster-manager | BSD-3-Clause | [LICENSE](https://github.com/miekg/dns/blob/master/LICENSE) |
| `github.com/shirou/gopsutil/v4` | v4.26.7 | nvpair-node-info (macOS) | BSD-3-Clause | [LICENSE](https://github.com/shirou/gopsutil/blob/master/LICENSE) |
| `golang.org/x/net` | v0.58.0 | ollama-proxy, lmstudio-proxy, nvpair-node-scanner, nvpair-errors, nvpair-cluster-manager | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/net/+/master:LICENSE) |
| `golang.org/x/sys` | v0.47.0 | nvpair-node-info, nvpair-cluster-manager, nvpair-engine-manager | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/sys/+/master:LICENSE) |
| `howett.net/plist` | v1.0.2-0.20250314 | nvpair-node-info | BSD-2-Clause | [LICENSE](https://github.com/DHowett/go-plist/blob/main/LICENSE) |

## Transitive (Indirect) Dependencies

| Library | Version(s) | Pulled In By | License | License URL |
|---------|-----------|--------------|---------|-------------|
| `github.com/atotto/clipboard` | v0.1.4 | bubbletea (textinput) | BSD-3-Clause | [LICENSE](https://github.com/atotto/clipboard/blob/master/LICENSE) |
| `github.com/aymanbagabas/go-osc52/v2` | v2.0.1 | termenv | MIT | [LICENSE](https://github.com/aymanbagabas/go-osc52/blob/main/LICENSE) |
| `github.com/cenkalti/backoff` | v2.2.1+incompatible | zeroconf | MIT | [LICENSE](https://github.com/cenkalti/backoff/blob/v2/LICENSE) |
| `github.com/charmbracelet/colorprofile` | v0.4.1 | bubbletea | MIT | [LICENSE](https://github.com/charmbracelet/colorprofile/blob/main/LICENSE) |
| `github.com/charmbracelet/x/ansi` | v0.11.6 | lipgloss, bubbletea | MIT | [LICENSE](https://github.com/charmbracelet/x/blob/main/LICENSE) |
| `github.com/charmbracelet/x/cellbuf` | v0.0.15 | bubbletea | MIT | [LICENSE](https://github.com/charmbracelet/x/blob/main/LICENSE) |
| `github.com/charmbracelet/x/term` | v0.2.2 | bubbletea | MIT | [LICENSE](https://github.com/charmbracelet/x/blob/main/LICENSE) |
| `github.com/clipperhouse/displaywidth` | v0.9.0 | charmbracelet/x/ansi | MIT | [LICENSE](https://github.com/clipperhouse/displaywidth/blob/main/LICENSE) |
| `github.com/clipperhouse/stringish` | v0.1.1 | clipperhouse/uax29 | MIT | [LICENSE](https://github.com/clipperhouse/stringish/blob/main/LICENSE) |
| `github.com/clipperhouse/uax29/v2` | v2.5.0 | charmbracelet/x/ansi | MIT | [LICENSE](https://github.com/clipperhouse/uax29/blob/master/LICENSE) |
| `github.com/ebitengine/purego` | v0.10.2 | gopsutil (macOS) | Apache-2.0 | [LICENSE](https://github.com/ebitengine/purego/blob/main/LICENSE) |
| `github.com/erikgeiser/coninput` | v0.0.0-20211004153227 | bubbletea | MIT | [LICENSE](https://github.com/erikgeiser/coninput/blob/main/LICENSE) |
| `github.com/go-ole/go-ole` | v1.2.6 / v1.3.0 | ghw | MIT | [LICENSE](https://github.com/go-ole/go-ole/blob/master/LICENSE) |
| `github.com/jaypipes/pcidb` | v1.1.1 | ghw | Apache-2.0 | [LICENSE](https://github.com/jaypipes/pcidb/blob/main/LICENSE) |
| `github.com/lucasb-eyer/go-colorful` | v1.3.0 | lipgloss, termenv | MIT | [LICENSE](https://github.com/lucasb-eyer/go-colorful/blob/master/LICENSE) |
| `github.com/mattn/go-isatty` | v0.0.20 | bubbletea | MIT | [LICENSE](https://github.com/mattn/go-isatty/blob/master/LICENSE) |
| `github.com/mattn/go-localereader` | v0.0.1 | bubbletea | MIT | [LICENSE](https://github.com/mattn/go-localereader/blob/master/LICENSE) |
| `github.com/mattn/go-runewidth` | v0.0.19 | lipgloss, bubbles | MIT | [LICENSE](https://github.com/mattn/go-runewidth/blob/master/LICENSE) |
| `github.com/muesli/ansi` | v0.0.0-20230316100256 | termenv | MIT | [LICENSE](https://github.com/muesli/ansi/blob/master/LICENSE) |
| `github.com/muesli/cancelreader` | v0.2.2 | bubbletea | MIT | [LICENSE](https://github.com/muesli/cancelreader/blob/master/LICENSE) |
| `github.com/muesli/termenv` | v0.16.0 | lipgloss, bubbletea | MIT | [LICENSE](https://github.com/muesli/termenv/blob/master/LICENSE) |
| `github.com/rivo/uniseg` | v0.4.7 | go-runewidth | MIT | [LICENSE](https://github.com/rivo/uniseg/blob/master/LICENSE.txt) |
| `github.com/tklauser/go-sysconf` | v0.3.16 | gopsutil (macOS) | BSD-3-Clause | [LICENSE](https://github.com/tklauser/go-sysconf/blob/master/LICENSE) |
| `github.com/xo/terminfo` | v0.0.0-20220910002029 | colorprofile | MIT | [LICENSE](https://github.com/xo/terminfo/blob/master/LICENSE) |
| `github.com/yusufpapurcu/wmi` | v1.2.4 | ghw | MIT | [LICENSE](https://github.com/yusufpapurcu/wmi/blob/master/LICENSE) |
| `golang.org/x/mod` | v0.12.0 / v0.17.0 / v0.31.0 | zeroconf (via miekg/dns) | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/mod/+/master:LICENSE) |
| `golang.org/x/sync` | v0.10.0 / v0.19.0 | zeroconf (via miekg/dns) | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/sync/+/master:LICENSE) |
| `golang.org/x/sys` | v0.47.0 | go-winio, zeroconf, ghw, charmbracelet/x/term | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/sys/+/master:LICENSE) |
| `golang.org/x/text` | v0.3.8 | charmbracelet/x/ansi | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/text/+/master:LICENSE) |
| `golang.org/x/tools` | v0.11.0 / v0.21.1-0.20240508 / v0.40.0 | zeroconf (via miekg/dns) | BSD-3-Clause | [LICENSE](https://cs.opensource.google/go/x/tools/+/master:LICENSE) |
| `gopkg.in/yaml.v3` | v3.0.1 | ghw | MIT + Apache-2.0 | [LICENSE](https://github.com/go-yaml/yaml/blob/v3/LICENSE) |

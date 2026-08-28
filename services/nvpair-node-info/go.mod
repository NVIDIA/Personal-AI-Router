module nvpair-node-info

go 1.25.0

require (
	github.com/jaypipes/ghw v0.24.0
	github.com/shirou/gopsutil/v4 v4.26.7
	golang.org/x/sys v0.47.0
	howett.net/plist v1.0.2-0.20250314012144-ee69052608d9
	nvpair-shared v0.0.0-00010101000000-000000000000
)

replace nvpair-shared => ../shared

require (
	github.com/ebitengine/purego v0.10.2 // indirect
	github.com/go-ole/go-ole v1.2.6 // indirect
	github.com/jaypipes/pcidb v1.1.1 // indirect
	github.com/lufia/plan9stats v0.0.0-20211012122336-39d0f177ccd0 // indirect
	github.com/power-devops/perfstat v0.0.0-20240221224432-82ca36839d55 // indirect
	github.com/tklauser/go-sysconf v0.3.16 // indirect
	github.com/tklauser/numcpus v0.11.0 // indirect
	github.com/yusufpapurcu/wmi v1.2.4 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
)

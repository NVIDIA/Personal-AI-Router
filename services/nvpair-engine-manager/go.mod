module nvpair-engine-manager

go 1.26.0

require nvpair-shared v0.0.0-00010101000000-000000000000

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/miekg/dns v1.1.55 // indirect
	golang.org/x/mod v0.12.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/tools v0.11.0 // indirect
)

replace nvpair-shared => ../shared

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	golang.org/x/crypto v0.57.0
	golang.org/x/sys v0.48.0
	gopkg.in/yaml.v3 v3.0.1
)

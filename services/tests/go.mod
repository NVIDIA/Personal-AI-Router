module tests

go 1.25.0

require (
	github.com/grandcat/zeroconf v1.0.0
	nvpair-shared v0.0.0-00010101000000-000000000000
)

replace nvpair-shared => ../shared

require (
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/miekg/dns v1.1.55 // indirect
	golang.org/x/mod v0.12.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/tools v0.11.0 // indirect
)

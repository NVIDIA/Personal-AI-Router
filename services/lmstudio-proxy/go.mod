module lmstudio-proxy

go 1.25.0

require nvpair-shared v0.0.0-00010101000000-000000000000

replace nvpair-shared => ../shared

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	github.com/cenkalti/backoff v2.2.1+incompatible // indirect
	github.com/grandcat/zeroconf v1.0.0 // indirect
	github.com/miekg/dns v1.1.55 // indirect
	golang.org/x/mod v0.17.0 // indirect
	golang.org/x/net v0.58.0 // indirect
	golang.org/x/sync v0.10.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/tools v0.21.1-0.20240508182429-e35e4ccd0d2d // indirect
)

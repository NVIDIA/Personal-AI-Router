module nvpair-host-bootstrap

go 1.25.0

require nvpair-shared v0.0.0-00010101000000-000000000000

require (
	github.com/Microsoft/go-winio v0.6.2
	github.com/go-ole/go-ole v1.2.6
	golang.org/x/crypto v0.43.0
	golang.org/x/sys v0.47.0
)

replace nvpair-shared => ../shared

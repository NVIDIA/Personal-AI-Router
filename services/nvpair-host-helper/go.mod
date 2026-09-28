module nvpair-host-helper

go 1.25.0

require nvpair-shared v0.0.0-00010101000000-000000000000

require (
	github.com/Microsoft/go-winio v0.6.2
	golang.org/x/sys v0.47.0
)

replace nvpair-shared => ../shared

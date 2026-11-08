module nvpair-engine-manager

go 1.25.0

require (
	github.com/stretchr/testify v1.12.1
	nvpair-shared v0.0.0-00010101000000-000000000000
)

require go.yaml.in/yaml/v3 v3.0.5 // indirect

replace nvpair-shared => ../shared

require (
	github.com/Microsoft/go-winio v0.6.2 // indirect
	golang.org/x/sys v0.47.0
)

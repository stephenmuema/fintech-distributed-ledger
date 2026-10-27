module github.com/stephenmuema/finnie/modules/kit

go 1.27.2

require (
	github.com/shopspring/decimal v1.6.0
	github.com/stephenmuema/finnie v0.0.0
)

// Modules build against the core's public contract (pkg/provapi) in this repository. Once a
// module lives in its own repository, replace this with a released version of Zirnis.
replace github.com/stephenmuema/finnie => ../..

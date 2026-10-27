module github.com/stephenmuema/finnie/modules/verto

go 1.27.2

require (
	github.com/shopspring/decimal v1.6.0
	github.com/stephenmuema/finnie v0.0.0
)

// Built against this repository's contract; pin a released version once split out.
replace github.com/stephenmuema/finnie => ../..

package main

import "github.com/stephenmuema/finnie/internal/system"

// registerExtensions compiles the bank's own Go extensions (T24 local routines) into the
// binary. Each one serves one or more hook points from pkg/extapi and is consulted wherever a
// suspense type or nostro lists it by name. Remote extensions (Python, other services) need no
// code here: they are registered at runtime under maker-checker.
//
// For example, with examples/extensions/go/chargeback-classifier:
//
//	sys.App.Extensions.MustRegister(chargebackclassifier.New())
func registerExtensions(sys *system.System) {
	_ = sys
}

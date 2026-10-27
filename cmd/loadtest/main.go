// Command loadtest stress-tests Zirnis through its platform API, against a sandbox only (it uses
// the public demo keys and test card tokens). Three modes:
//
//	loadtest seed  -customers 30000          # a realistic population: locals, visiting families
//	                                          # with sub-accounts, tourists, merchants, corporates
//	loadtest run   -rate 25 -chaos            # realistic open-loop traffic, with provider failures
//	loadtest steps -steps 4,8,16,32           # closed-loop concurrency steps: the ceiling
//
// See docs/runbooks/load-testing.md.
package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: loadtest seed|run|steps [flags]")
		os.Exit(2)
	}
	switch os.Args[1] {
	case "seed":
		seed(os.Args[2:])
	case "run":
		runTraffic(os.Args[2:])
	case "steps":
		steps(os.Args[2:])
	default:
		fmt.Fprintln(os.Stderr, "usage: loadtest seed|run|steps [flags]")
		os.Exit(2)
	}
}

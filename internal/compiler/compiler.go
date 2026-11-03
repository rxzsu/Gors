// Package compiler wires the GoRs front end together: parse, type check,
// borrow check, and lower to a chosen backend.
package compiler

import (
	"gors/internal/cgen"
	"gors/internal/codegen"
	"gors/internal/parser"
	"gors/internal/sema"
)

// Backend selects the code generator.
type Backend string

const (
	// BackendGo emits Go source (fast compile, GC runtime).
	BackendGo Backend = "go"
	// BackendC emits C11 source (no runtime, native -O2 code).
	BackendC Backend = "c"
)

// Compile translates GoRs source to Go source.
func Compile(src string) (string, []error) {
	return CompileWith(src, BackendGo)
}

// CompileWith runs the whole front end and lowers to the selected backend.
// On failure it returns a nil string and a non-empty error list.
func CompileWith(src string, backend Backend) (string, []error) {
	prog, perrs := parser.Parse(src)
	if len(perrs) > 0 {
		return "", wrap(perrs)
	}

	info, cerrs := sema.Check(prog)
	if len(cerrs) > 0 {
		return "", wrap(cerrs)
	}

	if berrs := sema.CheckBorrow(prog, info); len(berrs) > 0 {
		return "", wrap(berrs)
	}

	switch backend {
	case BackendC:
		out, err := cgen.Generate(prog, info)
		if err != nil {
			return "", []error{err}
		}
		return out, nil
	default:
		out, err := codegen.Generate(prog, info)
		if err != nil {
			return "", []error{err}
		}
		return out, nil
	}
}

func wrap[T error](errs []T) []error {
	out := make([]error, len(errs))
	for i, e := range errs {
		out[i] = e
	}
	return out
}

// Package sema performs type checking and ownership (borrow) analysis.
package sema

import (
	"fmt"

	"gors/internal/ast"
	"gors/internal/token"
)

// Type is a resolved semantic type.
type Type interface {
	String() string
}

// Basic is a primitive type: int, float, bool, string.
type Basic struct{ Name string }

func (t *Basic) String() string { return t.Name }

// StructT is a user-defined struct type.
type StructT struct {
	Name   string
	Fields map[string]Type
	Order  []string
}

func (t *StructT) String() string { return t.Name }

// Ptr is a reference: &T or &mut T.
type Ptr struct {
	Elem Type
	Mut  bool
}

func (t *Ptr) String() string {
	if t.Mut {
		return "&mut " + t.Elem.String()
	}
	return "&" + t.Elem.String()
}

// OptionT is Option<T>.
type OptionT struct{ Elem Type }

func (t *OptionT) String() string { return "Option<" + t.Elem.String() + ">" }

// ResultT is Result<T, E>.
type ResultT struct{ Ok, Err Type }

func (t *ResultT) String() string {
	return "Result<" + t.Ok.String() + ", " + t.Err.String() + ">"
}

// Slice is []T.
type Slice struct{ Elem Type }

func (t *Slice) String() string { return "[]" + t.Elem.String() }

// Void is the type of functions that return nothing.
type Void struct{}

func (t *Void) String() string { return "void" }

var (
	Int    = &Basic{Name: "int"}
	Float  = &Basic{Name: "float"}
	Bool   = &Basic{Name: "bool"}
	String = &Basic{Name: "string"}
	VoidT  = &Void{}
)

// Error is a semantic error.
type Error struct {
	Code  string
	Pos   token.Pos
	Msg   string
	Notes []Note
	Hint  string
}

// Note points to an earlier operation that explains a diagnostic.
type Note struct {
	Pos token.Pos
	Msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d:%d: error[%s]: %s", e.Pos.Line, e.Pos.Col, e.Code, e.Msg)
}

// IsCopy reports whether a value of type t is copied instead of moved.
// Primitives (except strings) and references are Copy; structs and strings are not.
func IsCopy(t Type) bool {
	switch tt := t.(type) {
	case *Basic:
		return tt.Name != "string"
	case *Ptr:
		return true
	default:
		return false
	}
}

// isMutRef reports whether t is &mut T.
func isMutRef(t Type) bool {
	p, ok := t.(*Ptr)
	return ok && p.Mut
}

// FuncSig is a function signature.
type FuncSig struct {
	Public bool
	Decl   token.Pos
	Name   string
	Params []Type
	Ret    Type
}

// Info is the result of semantic analysis.
type Info struct {
	Structs map[string]*StructT
	Funcs   map[string]*FuncSig
	// Methods maps a struct name to its methods. The receiver is Params[0].
	Methods map[string]map[string]*FuncSig
	// MethodSel marks Field expressions that resolve to a method.
	MethodSel map[ast.Expr]bool
	// IfLetTypes holds the type bound by `if let PATTERN = expr`.
	MatchTypes map[*ast.MatchArm]Type
	IfLetTypes map[*ast.IfLetStmt]Type
	// Moves marks expressions that transfer ownership.
	Moves     map[ast.Expr]bool
	ExprTypes map[ast.Expr]Type
	LetTypes  map[*ast.LetStmt]Type
}

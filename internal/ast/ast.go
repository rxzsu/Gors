// Package ast defines the GoRs syntax tree.
package ast

import "gors/internal/token"

// TypeExpr is a syntactic type.
type TypeExpr interface {
	Pos() token.Pos
	typeExpr()
}

// NamedType is a primitive, a struct, or a generic type like Option<int>.
type NamedType struct {
	Name string
	Args []TypeExpr
	At   token.Pos
}

// PtrType is &T or &mut T.
type PtrType struct {
	Elem TypeExpr
	Mut  bool
	At   token.Pos
}

// SliceType is []T.
type SliceType struct {
	Elem TypeExpr
	At   token.Pos
}

func (t *NamedType) typeExpr() {}
func (t *PtrType) typeExpr()   {}
func (t *SliceType) typeExpr() {}

func (t *NamedType) Pos() token.Pos { return t.At }
func (t *PtrType) Pos() token.Pos   { return t.At }
func (t *SliceType) Pos() token.Pos { return t.At }

// Expr is an expression node.
type Expr interface {
	Pos() token.Pos
	expr()
}

type Ident struct {
	Name string
	At   token.Pos
}

type IntLit struct {
	Value int64
	At    token.Pos
}

type FloatLit struct {
	Value float64
	At    token.Pos
}

type StringLit struct {
	Value string
	At    token.Pos
}

type BoolLit struct {
	Value bool
	At    token.Pos
}

// Unary is -x or !x.
type Unary struct {
	Op string
	X  Expr
	At token.Pos
}

// Binary is a binary operation.
type Binary struct {
	Op   string
	X, Y Expr
	At   token.Pos
}

// Call is fn(args).
type Call struct {
	Fn   Expr
	Args []Expr
	At   token.Pos
}

// Field is x.name.
type Field struct {
	X    Expr
	Name string
	At   token.Pos
}

// Index is x[i].
type Index struct {
	X   Expr
	Idx Expr
	At  token.Pos
}

// StructLit is Name{ field: value, ... }.
type StructLit struct {
	Type   string
	Fields []FieldInit
	At     token.Pos
}

type FieldInit struct {
	Name  string
	Value Expr
	At    token.Pos
}

// TryExpr is expr? — unwrap Result or return the error.
type TryExpr struct {
	X  Expr
	At token.Pos
}

// AddrOf is &x or &mut x.
type AddrOf struct {
	X   Expr
	Mut bool
	At  token.Pos
}

func (e *Ident) expr()     {}
func (e *IntLit) expr()    {}
func (e *FloatLit) expr()  {}
func (e *StringLit) expr() {}
func (e *BoolLit) expr()   {}
func (e *Unary) expr()     {}
func (e *Binary) expr()    {}
func (e *Call) expr()      {}
func (e *Field) expr()     {}
func (e *Index) expr()     {}
func (e *StructLit) expr() {}
func (e *TryExpr) expr()   {}
func (e *AddrOf) expr()    {}

func (e *Ident) Pos() token.Pos     { return e.At }
func (e *IntLit) Pos() token.Pos    { return e.At }
func (e *FloatLit) Pos() token.Pos  { return e.At }
func (e *StringLit) Pos() token.Pos { return e.At }
func (e *BoolLit) Pos() token.Pos   { return e.At }
func (e *Unary) Pos() token.Pos     { return e.At }
func (e *Binary) Pos() token.Pos    { return e.At }
func (e *Call) Pos() token.Pos      { return e.At }
func (e *Field) Pos() token.Pos     { return e.At }
func (e *Index) Pos() token.Pos     { return e.At }
func (e *StructLit) Pos() token.Pos { return e.At }
func (e *TryExpr) Pos() token.Pos   { return e.At }
func (e *AddrOf) Pos() token.Pos    { return e.At }

// Stmt is a statement node.
type Stmt interface {
	Pos() token.Pos
	stmt()
}

type LetStmt struct {
	Name  string
	Mut   bool
	Type  TypeExpr // may be nil
	Value Expr
	At    token.Pos
}

type AssignStmt struct {
	Target Expr
	Value  Expr
	At     token.Pos
}

type ExprStmt struct {
	X  Expr
	At token.Pos
}

type ReturnStmt struct {
	X  Expr // may be nil
	At token.Pos
}

type IfStmt struct {
	Cond Expr
	Then *Block
	Else Stmt // *Block or *IfStmt, may be nil
	At   token.Pos
}

// IfLetStmt is `if let Some(v) = expr { } else { }`.
type IfLetStmt struct {
	Kind  string // Some, Ok, Err
	Name  string
	Value Expr
	Then  *Block
	Else  Stmt // may be nil
	At    token.Pos
}

type WhileStmt struct {
	Cond Expr
	Body *Block
	At   token.Pos
}

type Block struct {
	Stmts []Stmt
	At    token.Pos
}

func (s *LetStmt) stmt()    {}
func (s *AssignStmt) stmt() {}
func (s *ExprStmt) stmt()   {}
func (s *ReturnStmt) stmt() {}
func (s *IfStmt) stmt()     {}
func (s *IfLetStmt) stmt()  {}
func (s *WhileStmt) stmt()  {}
func (s *Block) stmt()      {}

func (s *LetStmt) Pos() token.Pos    { return s.At }
func (s *AssignStmt) Pos() token.Pos { return s.At }
func (s *ExprStmt) Pos() token.Pos   { return s.At }
func (s *ReturnStmt) Pos() token.Pos { return s.At }
func (s *IfStmt) Pos() token.Pos     { return s.At }
func (s *IfLetStmt) Pos() token.Pos  { return s.At }
func (s *WhileStmt) Pos() token.Pos  { return s.At }
func (s *Block) Pos() token.Pos      { return s.At }

// Param is a function parameter.
type Param struct {
	Name string
	Type TypeExpr
	At   token.Pos
}

// FuncDecl is a top-level function or a method.
type FuncDecl struct {
	Name   string
	Recv   *Param // non-nil for methods: fn (p: &Point) dist2()
	Params []Param
	Ret    TypeExpr // may be nil
	Body   *Block
	At     token.Pos
}

// StructField is one field of a struct declaration.
type StructField struct {
	Name string
	Type TypeExpr
	At   token.Pos
}

// StructDecl is a top-level struct.
type StructDecl struct {
	Name   string
	Fields []StructField
	At     token.Pos
}

// Program is a parsed source file.
type Program struct {
	Structs []*StructDecl
	Funcs   []*FuncDecl
}

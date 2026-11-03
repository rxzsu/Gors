// Package codegen lowers a checked GoRs program to Go source code.
//
// Go is used as the backend on purpose: it gives us a mature runtime and a
// very fast native compiler, while GoRs itself owns the semantics (ownership,
// immutability, no nulls) at compile time.
package codegen

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"gors/internal/ast"
	"gors/internal/sema"
)

// Generate produces a complete Go source file for prog.
func Generate(prog *ast.Program, info *sema.Info) (string, error) {
	g := &gen{info: info}
	var body bytes.Buffer
	g.out = &body

	g.line("package main")
	g.line("")
	g.line("import \"fmt\"")
	g.line("")

	for _, sd := range prog.Structs {
		g.genStruct(sd)
	}
	for _, fn := range prog.Funcs {
		g.genFunc(fn)
	}

	// fmt is always imported; keep it referenced even if unused.
	g.line("var _ = fmt.Println")

	var file bytes.Buffer
	file.WriteString(body.String())
	return file.String(), nil
}

type gen struct {
	out  *bytes.Buffer
	info *sema.Info
	ind  int

	scopes []map[string]bool // used flags per block
}

func (g *gen) line(format string, args ...any) {
	if format == "" {
		g.out.WriteString("\n")
		return
	}
	g.out.WriteString(strings.Repeat("\t", g.ind))
	fmt.Fprintf(g.out, format, args...)
	g.out.WriteString("\n")
}

func (g *gen) push() { g.scopes = append(g.scopes, map[string]bool{}) }
func (g *gen) pop()  { g.scopes = g.scopes[:len(g.scopes)-1] }

func (g *gen) declare(name string) {
	if len(g.scopes) > 0 {
		g.scopes[len(g.scopes)-1][name] = false
	}
}

func (g *gen) use(name string) {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		if _, ok := g.scopes[i][name]; ok {
			g.scopes[i][name] = true
			return
		}
	}
}

// --- declarations ---

func (g *gen) genStruct(sd *ast.StructDecl) {
	g.line("type %s struct {", mangle(sd.Name))
	g.ind++
	for _, f := range sd.Fields {
		g.line("%s %s", fieldName(f.Name), g.typeOf(f.Type))
	}
	g.ind--
	g.line("}")
	g.line("")
}

func (g *gen) genFunc(fn *ast.FuncDecl) {
	var params []string
	for _, p := range fn.Params {
		params = append(params, mangle(p.Name)+" "+g.typeOf(p.Type))
	}
	ret := ""
	if fn.Ret != nil {
		ret = " " + g.typeOf(fn.Ret)
	}
	if fn.Recv != nil {
		g.line("func (%s %s) %s(%s)%s {", mangle(fn.Recv.Name), g.typeOf(fn.Recv.Type),
			mangle(fn.Name), strings.Join(params, ", "), ret)
	} else {
		g.line("func %s(%s)%s {", mangle(fn.Name), strings.Join(params, ", "), ret)
	}
	g.ind++
	g.push()
	if fn.Recv != nil {
		g.declare(mangle(fn.Recv.Name))
	}
	for _, p := range fn.Params {
		g.declare(mangle(p.Name))
	}
	for _, s := range fn.Body.Stmts {
		g.genStmt(s)
	}
	g.emitUnused()
	g.pop()
	g.ind--
	g.line("}")
	g.line("")
}

func (g *gen) emitUnused() {
	if len(g.scopes) == 0 {
		return
	}
	scope := g.scopes[len(g.scopes)-1]
	var unused []string
	for name, used := range scope {
		if !used {
			unused = append(unused, name)
		}
	}
	if len(unused) == 0 {
		return
	}
	// Deterministic order keeps generated output stable.
	sortStrings(unused)
	g.line("_ = %s", strings.Join(unused, "; _ = "))
}

// --- statements ---

func (g *gen) genStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		gt := g.typeOf(st.Type)
		if st.Type == nil {
			if t, ok := g.info.LetTypes[st]; ok {
				gt = goType(t)
			} else {
				gt = "int64"
			}
		}
		name := mangle(st.Name)
		g.line("var %s %s = %s", name, gt, g.expr(st.Value))
		g.declare(name)

	case *ast.AssignStmt:
		g.line("%s = %s", g.expr(st.Target), g.expr(st.Value))

	case *ast.ExprStmt:
		g.line("%s", g.expr(st.X))

	case *ast.ReturnStmt:
		if st.X == nil {
			g.line("return")
		} else {
			g.line("return %s", g.expr(st.X))
		}

	case *ast.IfStmt:
		g.line("if %s {", g.expr(st.Cond))
		g.ind++
		g.push()
		for _, s := range st.Then.Stmts {
			g.genStmt(s)
		}
		g.emitUnused()
		g.pop()
		g.ind--
		if st.Else != nil {
			switch el := st.Else.(type) {
			case *ast.IfStmt:
				g.out.WriteString(strings.Repeat("\t", g.ind) + "} else ")
				g.genIfInline(el)
			case *ast.Block:
				g.line("} else {")
				g.ind++
				g.push()
				for _, s := range el.Stmts {
					g.genStmt(s)
				}
				g.emitUnused()
				g.pop()
				g.ind--
				g.line("}")
			}
		} else {
			g.line("}")
		}

	case *ast.WhileStmt:
		g.line("for %s {", g.expr(st.Cond))
		g.ind++
		g.push()
		for _, s := range st.Body.Stmts {
			g.genStmt(s)
		}
		g.emitUnused()
		g.pop()
		g.ind--
		g.line("}")

	case *ast.Block:
		g.line("{")
		g.ind++
		g.push()
		for _, s := range st.Stmts {
			g.genStmt(s)
		}
		g.emitUnused()
		g.pop()
		g.ind--
		g.line("}")
	}
}

// genIfInline emits `if cond { ... }` without the leading `if` line break,
// used for `else if` chains.
func (g *gen) genIfInline(st *ast.IfStmt) {
	g.out.WriteString(fmt.Sprintf("if %s {\n", g.expr(st.Cond)))
	g.ind++
	g.push()
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	g.emitUnused()
	g.pop()
	g.ind--
	if st.Else != nil {
		if nested, ok := st.Else.(*ast.IfStmt); ok {
			g.out.WriteString(strings.Repeat("\t", g.ind) + "} else ")
			g.genIfInline(nested)
			return
		}
		g.line("} else {")
		g.ind++
		g.push()
		if blk, ok := st.Else.(*ast.Block); ok {
			for _, s := range blk.Stmts {
				g.genStmt(s)
			}
		}
		g.emitUnused()
		g.pop()
		g.ind--
		g.line("}")
		return
	}
	g.line("}")
}

// --- expressions ---

func (g *gen) expr(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.IntLit:
		return strconv.FormatInt(ex.Value, 10)
	case *ast.FloatLit:
		return strconv.FormatFloat(ex.Value, 'g', -1, 64)
	case *ast.StringLit:
		return strconv.Quote(ex.Value)
	case *ast.BoolLit:
		if ex.Value {
			return "true"
		}
		return "false"

	case *ast.Ident:
		name := mangle(ex.Name)
		g.use(name)
		return name

	case *ast.Unary:
		return "(" + ex.Op + g.expr(ex.X) + ")"

	case *ast.Binary:
		return "(" + g.expr(ex.X) + " " + ex.Op + " " + g.expr(ex.Y) + ")"

	case *ast.AddrOf:
		return "&" + g.expr(ex.X)

	case *ast.Field:
		if g.info.MethodSel[ex] {
			return g.expr(ex.X) + "." + mangle(ex.Name)
		}
		return g.expr(ex.X) + "." + fieldName(ex.Name)

	case *ast.Index:
		return g.expr(ex.X) + "[" + g.expr(ex.Idx) + "]"

	case *ast.StructLit:
		var parts []string
		for _, fi := range ex.Fields {
			parts = append(parts, fieldName(fi.Name)+": "+g.expr(fi.Value))
		}
		return mangle(ex.Type) + "{" + strings.Join(parts, ", ") + "}"

	case *ast.Call:
		if id, ok := ex.Fn.(*ast.Ident); ok {
			switch id.Name {
			case "println":
				return "fmt.Println(" + g.printArgs(ex.Args) + ")"
			case "print":
				return "fmt.Print(" + g.printArgs(ex.Args) + ")"
			case "len":
				return "len(" + g.args(ex.Args) + ")"
			}
		}
		return g.expr(ex.Fn) + "(" + g.args(ex.Args) + ")"
	}
	return "/* unsupported */"
}

func (g *gen) args(args []ast.Expr) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = g.expr(a)
	}
	return strings.Join(parts, ", ")
}

// printArgs renders println/print arguments, dereferencing references so that
// `println(r)` prints the pointed-to value rather than an address.
func (g *gen) printArgs(args []ast.Expr) string {
	parts := make([]string, len(args))
	for i, a := range args {
		s := g.expr(a)
		if t, ok := g.info.ExprTypes[a]; ok {
			if _, isPtr := t.(*sema.Ptr); isPtr {
				s = "*(" + s + ")"
			}
		}
		parts[i] = s
	}
	return strings.Join(parts, ", ")
}

func (g *gen) typeOf(te ast.TypeExpr) string {
	if te == nil {
		return "int64"
	}
	switch t := te.(type) {
	case *ast.NamedType:
		switch t.Name {
		case "int":
			return "int64"
		case "float":
			return "float64"
		case "bool":
			return "bool"
		case "string":
			return "string"
		}
		return mangle(t.Name)
	case *ast.PtrType:
		return "*" + g.typeOf(t.Elem)
	case *ast.SliceType:
		return "[]" + g.typeOf(t.Elem)
	}
	return "int64"
}

func goType(t sema.Type) string {
	switch tt := t.(type) {
	case *sema.Basic:
		switch tt.Name {
		case "int":
			return "int64"
		case "float":
			return "float64"
		case "bool":
			return "bool"
		case "string":
			return "string"
		}
		return "int64"
	case *sema.StructT:
		return mangle(tt.Name)
	case *sema.Ptr:
		return "*" + goType(tt.Elem)
	case *sema.Slice:
		return "[]" + goType(tt.Elem)
	}
	return "int64"
}

// --- naming ---

func fieldName(n string) string {
	if n == "" {
		return n
	}
	r := []rune(n)
	r[0] = unicode.ToUpper(r[0])
	return string(r)
}

var goReserved = map[string]bool{
	"break": true, "case": true, "chan": true, "const": true, "continue": true,
	"default": true, "defer": true, "else": true, "fallthrough": true, "for": true,
	"func": true, "go": true, "goto": true, "if": true, "import": true,
	"interface": true, "map": true, "package": true, "range": true, "return": true,
	"select": true, "struct": true, "switch": true, "type": true, "var": true,
	"fmt": true, "nil": true, "iota": true, "make": true, "new": true,
	"append": true, "cap": true, "copy": true, "delete": true, "panic": true,
	"recover": true, "string": true, "int": true, "int64": true, "float64": true,
	"bool": true, "byte": true, "rune": true, "error": true, "any": true,
}

func mangle(name string) string {
	if goReserved[name] {
		return name + "_"
	}
	return name
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

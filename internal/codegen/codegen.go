// Package codegen lowers a checked GoRs program to Go source code.
//
// Go is used as one of two backends: it gives a mature runtime and a very fast
// native compiler, while GoRs itself owns the semantics (ownership,
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
	g := &gen{info: info, out: &bytes.Buffer{}}

	g.line("package main")
	g.line("")
	g.line("import \"fmt\"")
	g.line("")

	for _, sd := range prog.Structs {
		g.genStruct(sd)
	}
	for _, t := range sema.MonoTypes(info) {
		g.genMono(t)
	}
	for _, fn := range prog.Funcs {
		g.genFunc(fn)
	}

	// fmt is always imported; keep it referenced even if unused.
	g.line("var _ = fmt.Println")

	return g.out.String(), nil
}

type gen struct {
	out     *bytes.Buffer
	pending bytes.Buffer
	info    *sema.Info
	ind     int
	tmp     int

	scopes []map[string]bool // used flags per block
}

func (g *gen) temp() string {
	g.tmp++
	return fmt.Sprintf("_t%d", g.tmp)
}

func (g *gen) flush() {
	if g.pending.Len() == 0 {
		return
	}
	g.out.Write(g.pending.Bytes())
	g.pending.Reset()
}

func (g *gen) line(format string, args ...any) {
	g.flush()
	if format == "" {
		g.out.WriteString("\n")
		return
	}
	g.out.WriteString(strings.Repeat("\t", g.ind))
	fmt.Fprintf(g.out, format, args...)
	g.out.WriteString("\n")
}

// emitPending queues a statement that must run before the current one.
func (g *gen) emitPending(format string, args ...any) {
	g.emitPendingAt(0, format, args...)
}

func (g *gen) emitPendingAt(extra int, format string, args ...any) {
	g.pending.WriteString(strings.Repeat("\t", g.ind+extra))
	fmt.Fprintf(&g.pending, format, args...)
	g.pending.WriteString("\n")
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

func (g *gen) genMono(t sema.Type) {
	name := sema.MonoName(t)
	switch tt := t.(type) {
	case *sema.OptionT:
		g.line("type %s struct {", name)
		g.ind++
		g.line("some bool")
		g.line("val %s", goType(tt.Elem))
		g.ind--
		g.line("}")
		g.line("")
	case *sema.ResultT:
		g.line("type %s struct {", name)
		g.ind++
		g.line("ok bool")
		g.line("val %s", goType(tt.Ok))
		g.line("err %s", goType(tt.Err))
		g.ind--
		g.line("}")
		g.line("")
	}
}

func (g *gen) funcSig(fn *ast.FuncDecl) (*sema.FuncSig, int) {
	if fn.Recv != nil {
		rt, ok := g.resolveType(fn.Recv.Type)
		if !ok {
			return nil, 0
		}
		base := rt
		if p, isPtr := base.(*sema.Ptr); isPtr {
			base = p.Elem
		}
		st, ok := base.(*sema.StructT)
		if !ok {
			return nil, 0
		}
		return g.info.Methods[st.Name][fn.Name], 1
	}
	return g.info.Funcs[fn.Name], 0
}

func (g *gen) resolveType(te ast.TypeExpr) (sema.Type, bool) {
	switch t := te.(type) {
	case *ast.NamedType:
		switch t.Name {
		case "int":
			return sema.Int, true
		case "float":
			return sema.Float, true
		case "bool":
			return sema.Bool, true
		case "string":
			return sema.String, true
		}
		if st, ok := g.info.Structs[t.Name]; ok {
			return st, true
		}
	case *ast.PtrType:
		if e, ok := g.resolveType(t.Elem); ok {
			return &sema.Ptr{Elem: e, Mut: t.Mut}, true
		}
	}
	return nil, false
}

func (g *gen) genFunc(fn *ast.FuncDecl) {
	sig, offset := g.funcSig(fn)
	if sig == nil {
		return
	}

	var params []string
	for i, p := range fn.Params {
		params = append(params, mangle(p.Name)+" "+goType(sig.Params[i+offset]))
	}
	ret := ""
	if fn.Ret != nil {
		ret = " " + goType(sig.Ret)
	}

	if fn.Recv != nil {
		rt, _ := g.resolveType(fn.Recv.Type)
		g.line("func (%s %s) %s(%s)%s {", mangle(fn.Recv.Name), goType(rt),
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
	sortStrings(unused)
	g.line("_ = %s", strings.Join(unused, "; _ = "))
}

// --- statements ---

func (g *gen) genStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		gt := "int64"
		if t, ok := g.info.LetTypes[st]; ok {
			gt = goType(t)
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
		g.genElse(st.Else)

	case *ast.IfLetStmt:
		g.genIfLet(st)

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

func (g *gen) genElse(e ast.Stmt) {
	if e == nil {
		g.line("}")
		return
	}
	switch el := e.(type) {
	case *ast.IfStmt:
		g.out.WriteString(strings.Repeat("\t", g.ind) + "} else ")
		g.genIfInline(el)
	case *ast.IfLetStmt:
		// `else if let` needs a nested if statement.
		g.line("} else {")
		g.ind++
		g.push()
		g.genIfLet(el)
		g.emitUnused()
		g.pop()
		g.ind--
		g.line("}")
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
}

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
	g.genElse(st.Else)
}

func (g *gen) genIfLet(st *ast.IfLetStmt) {
	vt, ok := g.info.ExprTypes[st.Value]
	if !ok {
		return
	}
	gt := goType(vt)
	tmp := g.temp()
	g.line("var %s %s = %s", tmp, gt, g.expr(st.Value))

	cond, field := tmp+".some", "val"
	switch st.Kind {
	case "Some":
		cond, field = tmp+".some", "val"
	case "Ok":
		cond, field = tmp+".ok", "val"
	case "Err":
		cond, field = "!"+tmp+".ok", "err"
	}

	g.line("if %s {", cond)
	g.ind++
	g.push()
	g.line("%s := %s.%s", mangle(st.Name), tmp, field)
	g.declare(mangle(st.Name))
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	g.emitUnused()
	g.pop()
	g.ind--
	g.genElse(st.Else)
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
		if ex.Name == "None" {
			if t, ok := g.info.ExprTypes[ex]; ok {
				return sema.MonoName(t) + "{}"
			}
			return "/* None */"
		}
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

	case *ast.TryExpr:
		return g.lowerTry(ex)

	case *ast.Call:
		return g.call(ex)
	}
	return "0"
}

func (g *gen) call(ex *ast.Call) string {
	if id, ok := ex.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "Some":
			rt, ok := g.info.ExprTypes[ex].(*sema.OptionT)
			if !ok {
				return "/* Some */"
			}
			return sema.MonoName(rt) + "{some: true, val: " + g.args(ex.Args) + "}"
		case "Ok", "Err":
			rt, ok := g.info.ExprTypes[ex].(*sema.ResultT)
			if !ok {
				return "/* Ok/Err */"
			}
			if id.Name == "Ok" {
				return sema.MonoName(rt) + "{ok: true, val: " + g.args(ex.Args) + "}"
			}
			return sema.MonoName(rt) + "{err: " + g.args(ex.Args) + "}"
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

// lowerTry turns `expr?` into statements plus a value reference.
func (g *gen) lowerTry(ex *ast.TryExpr) string {
	rt, ok := g.info.ExprTypes[ex.X].(*sema.ResultT)
	if !ok {
		return "0"
	}
	gt := goType(rt)
	inner := g.expr(ex.X)
	tmp := g.temp()
	g.emitPending("var %s %s = %s", tmp, gt, inner)
	g.emitPending("if !%s.ok {", tmp)
	g.emitPendingAt(1, "return %s{err: %s.err}", gt, tmp)
	g.emitPending("}")
	return tmp + ".val"
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
	if t, ok := g.resolveType(te); ok {
		return goType(t)
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
	case *sema.OptionT, *sema.ResultT:
		return sema.MonoName(tt)
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

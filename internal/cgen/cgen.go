// Package cgen lowers a checked GoRs program to C23.
//
// This is the "fast" backend: no runtime, no GC, no goroutine scheduler.
// The generated C is compiled with -std=c23 -O2 and produces the same machine
// code a hand-written C program would.
package cgen

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"gors/internal/ast"
	"gors/internal/sema"
)

const prelude = `#include <stdio.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>

typedef const char* gors_str;

[[maybe_unused]] static gors_str gors_concat(gors_str a, gors_str b) {
    size_t la = strlen(a), lb = strlen(b);
    char* out = (char*)malloc(la + lb + 1);
    if (!out) { fputs("gors: out of memory\n", stderr); exit(1); }
    memcpy(out, a, la);
    memcpy(out + la, b, lb + 1);
    return out;
}
`

// Generate produces a complete C translation unit.
func Generate(prog *ast.Program, info *sema.Info) (string, error) {
	g := &gen{info: info, out: &bytes.Buffer{}}

	g.out.WriteString(prelude)
	g.out.WriteString("\n")

	for _, sd := range prog.Structs {
		g.genStruct(sd)
	}
	for _, fn := range prog.Funcs {
		if fn.Name == "main" {
			continue
		}
		g.line("%s;", g.signature(fn))
	}
	g.out.WriteString("\n")

	for _, fn := range prog.Funcs {
		g.genFunc(fn)
	}

	g.genEntry(prog)
	return g.out.String(), nil
}

type gen struct {
	out  *bytes.Buffer
	info *sema.Info
	ind  int
}

func (g *gen) line(format string, args ...any) {
	if format == "" {
		g.out.WriteString("\n")
		return
	}
	g.out.WriteString(strings.Repeat("    ", g.ind))
	fmt.Fprintf(g.out, format, args...)
	g.out.WriteString("\n")
}

func (g *gen) genStruct(sd *ast.StructDecl) {
	g.line("typedef struct %s {", mangle(sd.Name))
	g.ind++
	for _, f := range sd.Fields {
		g.line("%s %s;", g.typeOf(f.Type), mangle(f.Name))
	}
	g.ind--
	g.line("} %s;", mangle(sd.Name))
	g.line("")
}

func (g *gen) signature(fn *ast.FuncDecl) string {
	name := cFuncName(fn)
	ret := "void"
	if fn.Ret != nil {
		ret = g.typeOf(fn.Ret)
	}
	var params []string
	if fn.Recv != nil {
		params = append(params, g.typeOf(fn.Recv.Type)+" "+mangle(fn.Recv.Name))
	}
	for _, p := range fn.Params {
		params = append(params, g.typeOf(p.Type)+" "+mangle(p.Name))
	}
	// C23: empty parentheses mean "no parameters".
	prefix := "[[maybe_unused]] static "
	if fn.Name == "main" && fn.Recv == nil {
		prefix = ""
	}
	return fmt.Sprintf("%s%s %s(%s)", prefix, ret, name, strings.Join(params, ", "))
}

// cFuncName flattens methods into Point_dist2 so C keeps a flat namespace.
func cFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		return mangle(receiverBase(fn.Recv.Type) + "_" + fn.Name)
	}
	return mangle(fn.Name)
}

func receiverBase(te ast.TypeExpr) string {
	switch t := te.(type) {
	case *ast.NamedType:
		return t.Name
	case *ast.PtrType:
		return receiverBase(t.Elem)
	}
	return ""
}

func (g *gen) genFunc(fn *ast.FuncDecl) {
	if fn.Name == "main" && fn.Recv == nil {
		return // emitted by genEntry
	}
	g.line("%s {", g.signature(fn))
	g.ind++
	for _, s := range fn.Body.Stmts {
		g.genStmt(s)
	}
	g.ind--
	g.line("}")
	g.line("")
}

func (g *gen) genEntry(prog *ast.Program) {
	var mainFn *ast.FuncDecl
	for _, fn := range prog.Funcs {
		if fn.Name == "main" && fn.Recv == nil {
			mainFn = fn
		}
	}
	if mainFn == nil {
		return
	}
	g.line("int main() {")
	g.ind++
	for _, s := range mainFn.Body.Stmts {
		g.genStmt(s)
	}
	g.line("return 0;")
	g.ind--
	g.line("}")
}

// --- statements ---

func (g *gen) genStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		ct := g.typeOf(st.Type)
		if st.Type == nil {
			if t, ok := g.info.LetTypes[st]; ok {
				ct = cType(t)
			} else {
				ct = "int64_t"
			}
		}
		g.line("%s %s = %s;", ct, mangle(st.Name), g.expr(st.Value))

	case *ast.AssignStmt:
		g.line("%s = %s;", g.expr(st.Target), g.expr(st.Value))

	case *ast.ExprStmt:
		if g.isVoidCall(st.X) {
			g.line("%s;", g.expr(st.X))
		} else {
			g.line("(void)(%s);", g.expr(st.X))
		}

	case *ast.ReturnStmt:
		if st.X == nil {
			g.line("return;")
		} else {
			g.line("return %s;", g.expr(st.X))
		}

	case *ast.IfStmt:
		g.line("if (%s) {", g.expr(st.Cond))
		g.ind++
		for _, s := range st.Then.Stmts {
			g.genStmt(s)
		}
		g.ind--
		if st.Else != nil {
			switch el := st.Else.(type) {
			case *ast.IfStmt:
				g.out.WriteString(strings.Repeat("    ", g.ind) + "} else ")
				g.genIfInline(el)
			case *ast.Block:
				g.line("} else {")
				g.ind++
				for _, s := range el.Stmts {
					g.genStmt(s)
				}
				g.ind--
				g.line("}")
			}
		} else {
			g.line("}")
		}

	case *ast.WhileStmt:
		g.line("while (%s) {", g.expr(st.Cond))
		g.ind++
		for _, s := range st.Body.Stmts {
			g.genStmt(s)
		}
		g.ind--
		g.line("}")

	case *ast.Block:
		g.line("{")
		g.ind++
		for _, s := range st.Stmts {
			g.genStmt(s)
		}
		g.ind--
		g.line("}")
	}
}

func (g *gen) genIfInline(st *ast.IfStmt) {
	g.out.WriteString(fmt.Sprintf("if (%s) {\n", g.expr(st.Cond)))
	g.ind++
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	g.ind--
	if st.Else != nil {
		if nested, ok := st.Else.(*ast.IfStmt); ok {
			g.out.WriteString(strings.Repeat("    ", g.ind) + "} else ")
			g.genIfInline(nested)
			return
		}
		g.line("} else {")
		g.ind++
		if blk, ok := st.Else.(*ast.Block); ok {
			for _, s := range blk.Stmts {
				g.genStmt(s)
			}
		}
		g.ind--
		g.line("}")
		return
	}
	g.line("}")
}

func (g *gen) isVoidCall(e ast.Expr) bool {
	call, ok := e.(*ast.Call)
	if !ok {
		return false
	}
	if field, ok := call.Fn.(*ast.Field); ok && g.info.MethodSel[field] {
		recvType := g.info.ExprTypes[field.X]
		base := recvType
		if p, ok := base.(*sema.Ptr); ok {
			base = p.Elem
		}
		if st, ok := base.(*sema.StructT); ok {
			if sig := g.info.Methods[st.Name][field.Name]; sig != nil {
				if _, isVoid := sig.Ret.(*sema.Void); isVoid {
					return true
				}
			}
		}
		return false
	}
	if id, ok := call.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "println", "print":
			return true
		}
		if sig, ok := g.info.Funcs[id.Name]; ok {
			if _, isVoid := sig.Ret.(*sema.Void); isVoid {
				return true
			}
		}
	}
	return false
}

// --- expressions ---

func (g *gen) expr(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.IntLit:
		return strconv.FormatInt(ex.Value, 10) + "LL"
	case *ast.FloatLit:
		return strconv.FormatFloat(ex.Value, 'g', -1, 64)
	case *ast.StringLit:
		return cQuote(ex.Value)
	case *ast.BoolLit:
		if ex.Value {
			return "true"
		}
		return "false"

	case *ast.Ident:
		return mangle(ex.Name)

	case *ast.Unary:
		return "(" + ex.Op + g.expr(ex.X) + ")"

	case *ast.Binary:
		if ex.Op == "+" && isStringType(g.info.ExprTypes[ex.X]) {
			return "gors_concat(" + g.expr(ex.X) + ", " + g.expr(ex.Y) + ")"
		}
		return "(" + g.expr(ex.X) + " " + ex.Op + " " + g.expr(ex.Y) + ")"

	case *ast.AddrOf:
		return "&" + g.expr(ex.X)

	case *ast.Field:
		sep := "."
		if _, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
			sep = "->"
		}
		return g.expr(ex.X) + sep + mangle(ex.Name)

	case *ast.Index:
		return g.expr(ex.X) + "[" + g.expr(ex.Idx) + "]"

	case *ast.StructLit:
		var parts []string
		for _, fi := range ex.Fields {
			parts = append(parts, "."+mangle(fi.Name)+" = "+g.expr(fi.Value))
		}
		return "(" + mangle(ex.Type) + "){ " + strings.Join(parts, ", ") + " }"

	case *ast.Call:
		if field, ok := ex.Fn.(*ast.Field); ok && g.info.MethodSel[field] {
			return g.methodCall(ex, field)
		}
		if id, ok := ex.Fn.(*ast.Ident); ok {
			switch id.Name {
			case "println":
				return g.printCall(ex.Args, true)
			case "print":
				return g.printCall(ex.Args, false)
			case "len":
				return "strlen(" + g.args(ex.Args) + ")"
			}
		}
		return mangle(callName(ex.Fn)) + "(" + g.args(ex.Args) + ")"
	}
	return "0"
}

// methodCall lowers p.dist2(x) to Point_dist2(&p, x).
func (g *gen) methodCall(c *ast.Call, field *ast.Field) string {
	recvType := g.info.ExprTypes[field.X]
	base := recvType
	if p, ok := base.(*sema.Ptr); ok {
		base = p.Elem
	}
	st, ok := base.(*sema.StructT)
	if !ok {
		return "0"
	}
	name := mangle(st.Name + "_" + field.Name)
	recv := g.expr(field.X)
	_, havePtr := recvType.(*sema.Ptr)
	wantPtr := false
	if sig := g.info.Methods[st.Name][field.Name]; sig != nil && len(sig.Params) > 0 {
		if _, ok := sig.Params[0].(*sema.Ptr); ok {
			wantPtr = true
		}
	}
	switch {
	case wantPtr && !havePtr:
		recv = "&(" + recv + ")"
	case !wantPtr && havePtr:
		recv = "*(" + recv + ")"
	}
	if args := g.args(c.Args); args != "" {
		return name + "(" + recv + ", " + args + ")"
	}
	return name + "(" + recv + ")"
}

func callName(e ast.Expr) string {
	if id, ok := e.(*ast.Ident); ok {
		return id.Name
	}
	return "?"
}

func (g *gen) args(args []ast.Expr) string {
	parts := make([]string, len(args))
	for i, a := range args {
		parts[i] = g.expr(a)
	}
	return strings.Join(parts, ", ")
}

// printCall builds a single printf with a matching format string.
func (g *gen) printCall(args []ast.Expr, newline bool) string {
	var fmts []string
	var vals []string
	for _, a := range args {
		t := g.info.ExprTypes[a]
		expr := g.expr(a)
		if p, ok := t.(*sema.Ptr); ok {
			expr = "*(" + expr + ")"
			t = p.Elem
		}
		switch tt := t.(type) {
		case *sema.Basic:
			switch tt.Name {
			case "int":
				fmts = append(fmts, "%lld")
				vals = append(vals, "(long long)("+expr+")")
			case "float":
				fmts = append(fmts, "%g")
				vals = append(vals, "(double)("+expr+")")
			case "bool":
				fmts = append(fmts, "%s")
				vals = append(vals, "(("+expr+") ? \"true\" : \"false\")")
			case "string":
				fmts = append(fmts, "%s")
				vals = append(vals, expr)
			}
		default:
			fmts = append(fmts, "%p")
			vals = append(vals, "(void*)&("+expr+")")
		}
	}
	format := strings.Join(fmts, " ")
	if newline {
		format += "\n"
	}
	if len(vals) == 0 {
		return "printf(" + cQuote(format) + ")"
	}
	return "printf(" + cQuote(format) + ", " + strings.Join(vals, ", ") + ")"
}

func (g *gen) typeOf(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}
	switch t := te.(type) {
	case *ast.NamedType:
		switch t.Name {
		case "int":
			return "int64_t"
		case "float":
			return "double"
		case "bool":
			return "bool"
		case "string":
			return "gors_str"
		}
		return mangle(t.Name)
	case *ast.PtrType:
		return g.typeOf(t.Elem) + "*"
	case *ast.SliceType:
		return g.typeOf(t.Elem) + "*"
	}
	return "void"
}

func cType(t sema.Type) string {
	switch tt := t.(type) {
	case *sema.Basic:
		switch tt.Name {
		case "int":
			return "int64_t"
		case "float":
			return "double"
		case "bool":
			return "bool"
		case "string":
			return "gors_str"
		}
	case *sema.StructT:
		return mangle(tt.Name)
	case *sema.Ptr:
		return cType(tt.Elem) + "*"
	case *sema.Slice:
		return cType(tt.Elem) + "*"
	}
	return "int64_t"
}

func isStringType(t sema.Type) bool {
	b, ok := t.(*sema.Basic)
	return ok && b.Name == "string"
}

// --- naming ---

func cQuote(s string) string {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString("\\\"")
		case '\\':
			sb.WriteString("\\\\")
		case '\n':
			sb.WriteString("\\n")
		case '\t':
			sb.WriteString("\\t")
		case '\r':
			sb.WriteString("\\r")
		default:
			if r < 0x20 {
				sb.WriteString(fmt.Sprintf("\\x%02x", r))
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return sb.String()
}

var cReserved = map[string]bool{
	"auto": true, "break": true, "case": true, "char": true, "const": true,
	"continue": true, "default": true, "do": true, "double": true, "else": true,
	"enum": true, "extern": true, "float": true, "for": true, "goto": true,
	"if": true, "inline": true, "int": true, "long": true, "register": true,
	"restrict": true, "return": true, "short": true, "signed": true, "sizeof": true,
	"static": true, "struct": true, "switch": true, "typedef": true, "union": true,
	"unsigned": true, "void": true, "volatile": true, "while": true,
	"bool": true, "true": true, "false": true, "NULL": true,
	"int64_t": true, "uint64_t": true, "size_t": true, "double_t": true,
	"printf": true, "puts": true, "malloc": true, "free": true, "memcpy": true,
	"strlen": true, "exit": true, "stderr": true, "stdout": true, "main": true,
	"gors_str": true, "gors_concat": true,
}

func mangle(name string) string {
	if cReserved[name] {
		return name + "_"
	}
	if name == "" {
		return name
	}
	// Field names in C live in a separate namespace, but keywords still break.
	r := []rune(name)
	if unicode.IsDigit(r[0]) {
		return "_" + name
	}
	return name
}

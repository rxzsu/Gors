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

static inline void gors_bounds(int64_t i, int64_t n) {
    if (i < 0 || i >= n) {
        fprintf(stderr, "gors: index %lld out of bounds (len %lld)\n",
                (long long)i, (long long)n);
        exit(1);
    }
}

#define GORS_AT(s, i) (gors_bounds((i), (s).len), (s).data[(i)])

static void* gors_dup(const void* src, int64_t n, size_t elem) {
    size_t bytes = (size_t)n * elem;
    void* out = malloc(bytes ? bytes : 1);
    if (!out) { fputs("gors: out of memory\n", stderr); exit(1); }
    if (bytes) memcpy(out, src, bytes);
    return out;
}

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
	for _, t := range sema.MonoTypes(info) {
		g.genMono(t)
	}
	for _, fn := range prog.Funcs {
		if fn.Name == "main" && fn.Recv == nil {
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
	out     *bytes.Buffer
	pending bytes.Buffer
	info    *sema.Info
	ind     int
	tmp     int
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
	g.out.WriteString(strings.Repeat("    ", g.ind))
	fmt.Fprintf(g.out, format, args...)
	g.out.WriteString("\n")
}

func (g *gen) emitPending(format string, args ...any) {
	g.emitPendingAt(0, format, args...)
}

func (g *gen) emitPendingAt(extra int, format string, args ...any) {
	g.pending.WriteString(strings.Repeat("    ", g.ind+extra))
	fmt.Fprintf(&g.pending, format, args...)
	g.pending.WriteString("\n")
}

// --- declarations ---

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

func (g *gen) genMono(t sema.Type) {
	name := sema.MonoName(t)
	switch tt := t.(type) {
	case *sema.OptionT:
		g.line("typedef struct %s { bool some; %s val; } %s;", name, cType(tt.Elem), name)
	case *sema.ResultT:
		g.line("typedef struct %s { bool ok; %s val; %s err; } %s;",
			name, cType(tt.Ok), cType(tt.Err), name)
	case *sema.Slice:
		elem := cType(tt.Elem)
		g.line("typedef struct %s { %s* data; int64_t len; int64_t cap; } %s;",
			name, elem, name)
		g.line("[[maybe_unused]] static void %s_push(%s* s, %s v) {", name, name, elem)
		g.ind++
		g.line("if (s->len == s->cap) {")
		g.ind++
		g.line("int64_t nc = s->cap ? s->cap * 2 : 4;")
		g.line("%s* nd = (%s*)realloc(s->data, (size_t)nc * sizeof(%s));", elem, elem, elem)
		g.line("if (!nd) { fputs(\"gors: out of memory\\n\", stderr); exit(1); }")
		g.line("s->data = nd;")
		g.line("s->cap = nc;")
		g.ind--
		g.line("}")
		g.line("s->data[s->len++] = v;")
		g.ind--
		g.line("}")
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
	case *ast.SliceType:
		if e, ok := g.resolveType(t.Elem); ok {
			return &sema.Slice{Elem: e}, true
		}
	}
	return nil, false
}

func (g *gen) signature(fn *ast.FuncDecl) string {
	sig, offset := g.funcSig(fn)
	name := cFuncName(fn)
	ret := "void"
	if fn.Ret != nil && sig != nil {
		ret = cType(sig.Ret)
	}

	var params []string
	if fn.Recv != nil {
		rt, _ := g.resolveType(fn.Recv.Type)
		params = append(params, cType(rt)+" "+mangle(fn.Recv.Name))
	}
	if sig != nil {
		for i, p := range fn.Params {
			params = append(params, cType(sig.Params[i+offset])+" "+mangle(p.Name))
		}
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
		ct := "int64_t"
		if t, ok := g.info.LetTypes[st]; ok {
			ct = cType(t)
		}
		g.line("%s %s = %s;", ct, mangle(st.Name), g.expr(st.Value))

	case *ast.AssignStmt:
		if idx, ok := st.Target.(*ast.Index); ok {
			base := g.expr(idx.X)
			i := g.expr(idx.Idx)
			g.line("gors_bounds(%s, (%s).len);", i, base)
			g.line("(%s).data[%s] = %s;", base, i, g.expr(st.Value))
			return
		}
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
		g.genElse(st.Else)

	case *ast.IfLetStmt:
		g.genIfLet(st)

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

func (g *gen) genElse(e ast.Stmt) {
	if e == nil {
		g.line("}")
		return
	}
	switch el := e.(type) {
	case *ast.IfStmt:
		g.out.WriteString(strings.Repeat("    ", g.ind) + "} else ")
		g.genIfInline(el)
	case *ast.IfLetStmt:
		g.line("} else {")
		g.ind++
		g.genIfLet(el)
		g.ind--
		g.line("}")
	case *ast.Block:
		g.line("} else {")
		g.ind++
		for _, s := range el.Stmts {
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
	g.genElse(st.Else)
}

func (g *gen) genIfLet(st *ast.IfLetStmt) {
	vt, ok := g.info.ExprTypes[st.Value]
	if !ok {
		return
	}
	ct := cType(vt)
	tmp := g.temp()
	g.line("%s %s = %s;", ct, tmp, g.expr(st.Value))

	cond, field := tmp+".some", "val"
	switch st.Kind {
	case "Some":
		cond, field = tmp+".some", "val"
	case "Ok":
		cond, field = tmp+".ok", "val"
	case "Err":
		cond, field = "!"+tmp+".ok", "err"
	}

	bound, _ := g.info.IfLetTypes[st]
	g.line("if (%s) {", cond)
	g.ind++
	g.line("%s %s = %s.%s;", cType(bound), mangle(st.Name), tmp, field)
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	g.ind--
	g.genElse(st.Else)
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
		if ex.Name == "None" {
			if t, ok := g.info.ExprTypes[ex]; ok {
				return "(" + sema.MonoName(t) + "){0}"
			}
			return "0"
		}
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
		if g.info.MethodSel[ex] {
			sep := "."
			if _, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
				sep = "->"
			}
			return g.expr(ex.X) + sep + mangle(ex.Name)
		}
		sep := "."
		if _, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
			sep = "->"
		}
		return g.expr(ex.X) + sep + mangle(ex.Name)

	case *ast.Index:
		if _, isSlice := g.info.ExprTypes[ex.X].(*sema.Slice); isSlice {
			return "GORS_AT(" + g.expr(ex.X) + ", " + g.expr(ex.Idx) + ")"
		}
		return g.expr(ex.X) + "[" + g.expr(ex.Idx) + "]"

	case *ast.StructLit:
		var parts []string
		for _, fi := range ex.Fields {
			parts = append(parts, "."+mangle(fi.Name)+" = "+g.expr(fi.Value))
		}
		return "(" + mangle(ex.Type) + "){ " + strings.Join(parts, ", ") + " }"

	case *ast.SliceLit:
		return g.sliceLit(ex)

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
				return "0"
			}
			return "(" + sema.MonoName(rt) + "){ .some = true, .val = " + g.args(ex.Args) + " }"
		case "Ok", "Err":
			rt, ok := g.info.ExprTypes[ex].(*sema.ResultT)
			if !ok {
				return "0"
			}
			if id.Name == "Ok" {
				return "(" + sema.MonoName(rt) + "){ .ok = true, .val = " + g.args(ex.Args) + " }"
			}
			return "(" + sema.MonoName(rt) + "){ .err = " + g.args(ex.Args) + " }"
		case "println":
			return g.printCall(ex.Args, true)
		case "print":
			return g.printCall(ex.Args, false)
		case "len":
			if len(ex.Args) == 1 {
				if _, isSlice := g.info.ExprTypes[ex.Args[0]].(*sema.Slice); isSlice {
					return "(" + g.expr(ex.Args[0]) + ").len"
				}
			}
			return "strlen(" + g.args(ex.Args) + ")"
		case "push":
			if len(ex.Args) == 2 {
				if addr, ok := ex.Args[0].(*ast.AddrOf); ok {
					if sl, ok := g.info.ExprTypes[addr.X].(*sema.Slice); ok {
						return sema.MonoName(sl) + "_push(&" + g.expr(addr.X) + ", " + g.expr(ex.Args[1]) + ")"
					}
				}
			}
			return "0"
		}
	}
	if field, ok := ex.Fn.(*ast.Field); ok && g.info.MethodSel[field] {
		return g.methodCall(ex, field)
	}
	return mangle(callName(ex.Fn)) + "(" + g.args(ex.Args) + ")"
}

func (g *gen) sliceLit(ex *ast.SliceLit) string {
	sl, ok := g.info.ExprTypes[ex].(*sema.Slice)
	if !ok {
		return "0"
	}
	name := sema.MonoName(sl)
	if len(ex.Elems) == 0 {
		return "(" + name + "){0}"
	}
	var parts []string
	for _, e := range ex.Elems {
		parts = append(parts, g.expr(e))
	}
	n := strconv.Itoa(len(ex.Elems))
	elem := cType(sl.Elem)
	return "(" + name + "){ .data = (" + elem + "*)gors_dup((" + elem + "[]){" +
		strings.Join(parts, ", ") + "}, " + n + ", sizeof(" + elem + ")), .len = " + n + ", .cap = " + n + " }"
}

// lowerTry turns `expr?` into statements plus a value reference.
func (g *gen) lowerTry(ex *ast.TryExpr) string {
	rt, ok := g.info.ExprTypes[ex.X].(*sema.ResultT)
	if !ok {
		return "0"
	}
	ct := cType(rt)
	inner := g.expr(ex.X)
	tmp := g.temp()
	g.emitPending("%s %s = %s;", ct, tmp, inner)
	g.emitPending("if (!%s.ok) {", tmp)
	g.emitPendingAt(1, "return (%s){ .err = %s.err };", ct, tmp)
	g.emitPending("}")
	return tmp + ".val"
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
		case "println", "print", "push":
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

func (g *gen) typeOf(te ast.TypeExpr) string {
	if te == nil {
		return "void"
	}
	if t, ok := g.resolveType(te); ok {
		return cType(t)
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
		return sema.MonoName(&sema.Slice{Elem: &sema.Basic{Name: "int"}})
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
		return sema.MonoName(tt)
	case *sema.OptionT, *sema.ResultT:
		return sema.MonoName(tt)
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
	"bool": true, "true": true, "false": true, "NULL": true, "nullptr": true,
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
	r := []rune(name)
	if unicode.IsDigit(r[0]) {
		return "_" + name
	}
	return name
}

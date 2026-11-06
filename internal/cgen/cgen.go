// Package cgen lowers a checked GoRs program to C23.
//
// This is the "fast" backend: no runtime, no GC, no goroutine scheduler.
// The generated C is compiled with -std=c23 -O2 and produces the same machine
// code a hand-written C program would.
//
// Memory: every owning value (string, slice, struct/option/result containing
// them) is released at the end of its scope and on every return path. GoRs has
// move semantics, so there is exactly one owner at any time and no refcounts
// are needed.
package cgen

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"

	"gors/internal/ast"
	"gors/internal/sema"
)

const prelude = `#include <stdio.h>
#include <stdint.h>
#include <string.h>
#include <stdlib.h>

typedef char* gors_str;

static inline void gors_oom(void) {
    fputs("gors: out of memory\n", stderr);
    exit(1);
}

// Allocation accounting: set GORS_LEAKCHECK=1 to report leaks at exit.
static int64_t gors_live_allocs = 0;

static void gors_leak_report(void) {
    const char* flag = getenv("GORS_LEAKCHECK");
    if (flag && flag[0] == '1' && gors_live_allocs != 0) {
        fprintf(stderr, "gors: %lld leaked allocation(s)\n",
                (long long)gors_live_allocs);
    }
}

static void* gors_malloc(size_t n) {
    static int registered = 0;
    if (!registered) { atexit(gors_leak_report); registered = 1; }
    void* p = malloc(n);
    if (!p) gors_oom();
    gors_live_allocs++;
    return p;
}

static void gors_free(void* p) {
    if (!p) return;
    gors_live_allocs--;
    free(p);
}

static void* gors_realloc(void* p, size_t n) {
    if (!p) return gors_malloc(n);
    void* q = realloc(p, n);
    if (!q) gors_oom();
    return q;
}

[[maybe_unused]] static gors_str gors_str_new(const char* s) {
    size_t n = strlen(s) + 1;
    char* p = (char*)gors_malloc(n);
    if (!p) gors_oom();
    memcpy(p, s, n);
    return p;
}

[[maybe_unused]] static void gors_str_drop(gors_str s) {
    gors_free(s);
}

[[maybe_unused]] static int gors_str_eq(gors_str a, gors_str b) {
    return strcmp(a, b) == 0;
}

static inline void gors_bounds(int64_t i, int64_t n) {
    if (i < 0 || i >= n) {
        fprintf(stderr, "gors: index %lld out of bounds (len %lld)\n",
                (long long)i, (long long)n);
        exit(1);
    }
}

#define GORS_AT(s, i) (gors_bounds((i), (s).len), (s).data[(i)])

static inline void gors_slice_bounds(int64_t low, int64_t high, int64_t len) {
    if (low < 0 || high < low || high > len) {
        fprintf(stderr, "gors: slice bounds out of range [%lld:%lld] (len %lld)\n",
                (long long)low, (long long)high, (long long)len);
        exit(1);
    }
}

[[maybe_unused]] static gors_str gors_str_slice(gors_str s, int64_t low, int64_t high) {
    int64_t len = (int64_t)strlen(s);
    gors_slice_bounds(low, high, len);
    int64_t n = high - low;
    char* p = (char*)gors_malloc((size_t)n + 1);
    memcpy(p, s + low, (size_t)n);
    p[n] = '\0';
    return p;
}

static void* gors_dup(const void* src, int64_t n, size_t elem) {
    size_t bytes = (size_t)n * elem;
    void* out = gors_malloc(bytes ? bytes : 1);
    if (!out) gors_oom();
    if (bytes) memcpy(out, src, bytes);
    return out;
}

[[maybe_unused]] static gors_str gors_concat_impl(gors_str a, gors_str b) {
    size_t la = strlen(a), lb = strlen(b);
    char* out = (char*)gors_malloc(la + lb + 1);
    if (!out) gors_oom();
    memcpy(out, a, la);
    memcpy(out + la, b, lb + 1);
    return out;
}

[[maybe_unused]] static gors_str gors_concat(gors_str a, gors_str b) {
    return gors_concat_impl(a, b);
}
`

// Generate produces a complete C translation unit.
func Generate(prog *ast.Program, info *sema.Info) (string, error) {
	g := &gen{info: info, out: &bytes.Buffer{}}

	g.out.WriteString(prelude)
	g.out.WriteString("\n")

	// 1. Type definitions in dependency order (C needs complete types for
	// struct fields, so a plain forward declaration is not enough).
	mono := sema.MonoTypes(info)
	order := g.topoSort(prog, mono)
	for _, t := range order {
		g.line("typedef struct %s %s;", typeName(t), typeName(t))
	}
	g.out.WriteString("\n")
	for _, t := range order {
		g.genTypeDef(t)
	}
	g.out.WriteString("\n")

	// 2. Drop and push prototypes.
	for _, sd := range prog.Structs {
		if st, ok := info.Structs[sd.Name]; ok && structOwns(st) {
			g.line("[[maybe_unused]] static void %s_drop(%s* p);", mangle(sd.Name), mangle(sd.Name))
		}
	}
	for _, t := range mono {
		if ownsValue(t) {
			g.line("[[maybe_unused]] static void %s_drop(%s* p);", sema.MonoName(t), sema.MonoName(t))
		}
		if sl, ok := t.(*sema.Slice); ok {
			g.line("[[maybe_unused]] static void %s_push(%s* s, %s v);",
				sema.MonoName(sl), sema.MonoName(sl), cType(sl.Elem))
			g.line("[[maybe_unused]] static %s %s_slice(%s s, int64_t low, int64_t high);",
				sema.MonoName(sl), sema.MonoName(sl), sema.MonoName(sl))
		}
	}
	g.out.WriteString("\n")

	// 3. Drop and push implementations.
	for _, sd := range prog.Structs {
		if st, ok := info.Structs[sd.Name]; ok && structOwns(st) {
			g.genStructDrop(st)
		}
	}
	for _, t := range mono {
		if ownsValue(t) {
			g.genMonoDrop(t)
		}
		if sl, ok := t.(*sema.Slice); ok {
			g.genSlicePush(sl)
		}
	}
	g.out.WriteString("\n")

	// 4. Function prototypes and bodies.
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

type ownedVar struct {
	name      string
	typ       sema.Type
	owned     bool
	temporary bool
}

type scope struct {
	vars []ownedVar
}

type gen struct {
	out     *bytes.Buffer
	pending bytes.Buffer
	info    *sema.Info
	ind     int
	tmp     int

	transfers  [][]string
	entry      bool
	retType    sema.Type
	scopes     []*scope
	loopScopes []int
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

// --- ownership ---

func (g *gen) pushScope() { g.scopes = append(g.scopes, &scope{}) }
func (g *gen) popScope()  { g.scopes = g.scopes[:len(g.scopes)-1] }

func (g *gen) declareOwned(name string, t sema.Type) {
	if len(g.scopes) == 0 {
		return
	}
	sc := g.scopes[len(g.scopes)-1]
	sc.vars = append(sc.vars, ownedVar{name: name, typ: t, owned: ownsValue(t)})
}

func (g *gen) markMoved(name string) {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		sc := g.scopes[i]
		for j := range sc.vars {
			if sc.vars[j].name == name {
				sc.vars[j].owned = false
				return
			}
		}
	}
}

func (g *gen) emitDrop(name string, t sema.Type) {
	switch tt := t.(type) {
	case *sema.Basic:
		if tt.Name == "string" {
			g.line("gors_str_drop(%s);", name)
		}
	case *sema.Slice:
		g.line("%s_drop(&%s);", sema.MonoName(tt), name)
	case *sema.StructT:
		if structOwns(tt) {
			g.line("%s_drop(&%s);", mangle(tt.Name), name)
		}
	case *sema.OptionT, *sema.ResultT:
		if ownsValue(tt) {
			g.line("%s_drop(&%s);", sema.MonoName(tt), name)
		}
	}
}

func (g *gen) dropScope() {
	if len(g.scopes) == 0 {
		return
	}
	sc := g.scopes[len(g.scopes)-1]
	for i := len(sc.vars) - 1; i >= 0; i-- {
		v := &sc.vars[i]
		if v.owned {
			g.emitDrop(v.name, v.typ)
			v.owned = false
		}
	}
}

func (g *gen) dropToLoop() {
	if len(g.loopScopes) == 0 {
		return
	}
	targetScope := g.loopScopes[len(g.loopScopes)-1]
	for i := len(g.scopes) - 1; i >= targetScope; i-- {
		sc := g.scopes[i]
		for j := len(sc.vars) - 1; j >= 0; j-- {
			v := &sc.vars[j]
			if v.owned {
				g.emitDrop(v.name, v.typ)
				v.owned = false
			}
		}
	}
}

func (g *gen) dropAll() {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		sc := g.scopes[i]
		for j := len(sc.vars) - 1; j >= 0; j-- {
			v := &sc.vars[j]
			if v.owned {
				g.emitDrop(v.name, v.typ)
				v.owned = false
			}
		}
	}
}

// ownsValue reports whether a value of type t must be released.
func ownsValue(t sema.Type) bool {
	switch tt := t.(type) {
	case *sema.Basic:
		return tt.Name == "string"
	case *sema.Slice:
		return true
	case *sema.StructT:
		return structOwns(tt)
	case *sema.OptionT:
		return ownsValue(tt.Elem)
	case *sema.ResultT:
		return ownsValue(tt.Ok) || ownsValue(tt.Err)
	}
	return false
}

func structOwns(st *sema.StructT) bool {
	return structOwnsVisited(st, map[*sema.StructT]bool{})
}

func structOwnsVisited(st *sema.StructT, seen map[*sema.StructT]bool) bool {
	if seen[st] {
		return false
	}
	seen[st] = true
	for _, name := range st.Order {
		if ownsValue(st.Fields[name]) {
			return true
		}
	}
	return false
}

// --- declarations ---

func typeName(t sema.Type) string {
	if st, ok := t.(*sema.StructT); ok {
		return mangle(st.Name)
	}
	return sema.MonoName(t)
}

// topoSort orders type definitions so that every type is declared after the
// types it contains by value.
func (g *gen) topoSort(prog *ast.Program, mono []sema.Type) []sema.Type {
	var order []sema.Type
	done := map[string]bool{}

	var visit func(t sema.Type)
	visit = func(t sema.Type) {
		if t == nil {
			return
		}
		switch t.(type) {
		case *sema.Basic, *sema.Ptr, *sema.Void:
			return
		}
		key := typeName(t)
		if done[key] {
			return
		}
		done[key] = true

		switch tt := t.(type) {
		case *sema.StructT:
			for _, f := range tt.Order {
				if _, isPtr := tt.Fields[f].(*sema.Ptr); !isPtr {
					visit(tt.Fields[f])
				}
			}
		case *sema.OptionT:
			visit(tt.Elem)
		case *sema.ResultT:
			visit(tt.Ok)
			visit(tt.Err)
		case *sema.Slice:
			visit(tt.Elem)
		}
		order = append(order, t)
	}

	for _, sd := range prog.Structs {
		if st, ok := g.info.Structs[sd.Name]; ok {
			visit(st)
		}
	}
	for _, t := range mono {
		visit(t)
	}
	return order
}

func (g *gen) genTypeDef(t sema.Type) {
	switch tt := t.(type) {
	case *sema.StructT:
		g.genStructDef(tt)
	default:
		g.genMonoTypedef(t)
	}
}

func (g *gen) genStructDef(st *sema.StructT) {
	g.line("struct %s {", mangle(st.Name))
	g.ind++
	for _, f := range st.Order {
		g.line("%s %s;", cType(st.Fields[f]), mangle(f))
	}
	g.ind--
	g.line("};")
	g.line("")
}

func (g *gen) genStructDrop(st *sema.StructT) {
	name := mangle(st.Name)
	g.line("[[maybe_unused]] static void %s_drop(%s* p) {", name, name)
	g.ind++
	for _, f := range st.Order {
		ft := st.Fields[f]
		if ownsValue(ft) {
			field := "p->" + mangle(f)
			switch tt := ft.(type) {
			case *sema.Basic:
				g.line("gors_str_drop(%s);", field)
			case *sema.Slice:
				g.line("%s_drop(&%s);", sema.MonoName(tt), field)
			case *sema.StructT:
				g.line("%s_drop(&%s);", mangle(tt.Name), field)
			case *sema.OptionT, *sema.ResultT:
				g.line("%s_drop(&%s);", sema.MonoName(tt), field)
			}
		}
	}
	g.ind--
	g.line("}")
	g.line("")
}

func (g *gen) genMonoTypedef(t sema.Type) {
	name := sema.MonoName(t)
	switch tt := t.(type) {
	case *sema.OptionT:
		g.line("struct %s { bool some; %s val; };", name, cType(tt.Elem))
	case *sema.ResultT:
		g.line("struct %s { bool ok; %s val; %s err; };", name, cType(tt.Ok), cType(tt.Err))
	case *sema.Slice:
		g.line("struct %s { %s* data; int64_t len; int64_t cap; };", name, cType(tt.Elem))
	}
}

func (g *gen) genMonoDrop(t sema.Type) {
	name := sema.MonoName(t)
	switch tt := t.(type) {
	case *sema.OptionT:
		g.line("[[maybe_unused]] static void %s_drop(%s* p) {", name, name)
		g.ind++
		g.line("if (p->some) {")
		g.ind++
		g.emitFieldDrop("p->val", tt.Elem)
		g.ind--
		g.line("}")
		g.ind--
		g.line("}")
	case *sema.ResultT:
		g.line("[[maybe_unused]] static void %s_drop(%s* p) {", name, name)
		g.ind++
		g.line("if (p->ok) {")
		g.ind++
		g.emitFieldDrop("p->val", tt.Ok)
		g.ind--
		g.line("} else {")
		g.ind++
		g.emitFieldDrop("p->err", tt.Err)
		g.ind--
		g.line("}")
		g.ind--
		g.line("}")
	case *sema.Slice:
		g.line("[[maybe_unused]] static void %s_drop(%s* s) {", name, name)
		g.ind++
		if ownsValue(tt.Elem) {
			g.line("for (int64_t i = 0; i < s->len; i++) {")
			g.ind++
			g.emitFieldDrop("s->data[i]", tt.Elem)
			g.ind--
			g.line("}")
		}
		g.line("gors_free(s->data);")
		g.line("s->data = 0;")
		g.line("s->len = 0;")
		g.line("s->cap = 0;")
		g.ind--
		g.line("}")
	}
	g.line("")
}

func (g *gen) emitFieldDrop(expr string, t sema.Type) {
	switch tt := t.(type) {
	case *sema.Basic:
		if tt.Name == "string" {
			g.line("gors_str_drop(%s);", expr)
		}
	case *sema.Slice:
		g.line("%s_drop(&%s);", sema.MonoName(tt), expr)
	case *sema.StructT:
		if structOwns(tt) {
			g.line("%s_drop(&%s);", mangle(tt.Name), expr)
		}
	case *sema.OptionT, *sema.ResultT:
		if ownsValue(tt) {
			g.line("%s_drop(&%s);", sema.MonoName(tt), expr)
		}
	}
}

func (g *gen) genSlicePush(sl *sema.Slice) {
	name := sema.MonoName(sl)
	elem := cType(sl.Elem)
	g.line("[[maybe_unused]] static %s %s_slice(%s s, int64_t low, int64_t high) {", name, name, name)
	g.ind++
	g.line("gors_slice_bounds(low, high, s.len);")
	g.line("int64_t n = high - low;")
	g.line("if (n == 0) return (%s){0};", name)
	g.line("return (%s){ .data = (%s*)gors_dup(s.data + low, n, sizeof(%s)), .len = n, .cap = n };", name, elem, elem)
	g.ind--
	g.line("}")
	g.line("[[maybe_unused]] static void %s_push(%s* s, %s v) {", name, name, elem)
	g.ind++
	g.line("if (s->len == s->cap) {")
	g.ind++
	g.line("int64_t nc = s->cap ? s->cap * 2 : 4;")
	g.line("%s* nd = (%s*)realloc(s->data, (size_t)nc * sizeof(%s));", elem, elem, elem)
	g.line("if (!nd) gors_oom();")
	g.line("s->data = nd;")
	g.line("s->cap = nc;")
	g.ind--
	g.line("}")
	g.line("s->data[s->len++] = v;")
	g.ind--
	g.line("}")
	g.line("")
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
		params = append(params, cType(rt)+" "+parameterName(fn.Recv.Name, 0))
	}
	if sig != nil {
		for i, p := range fn.Params {
			params = append(params, cType(sig.Params[i+offset])+" "+parameterName(p.Name, i+offset))
		}
	}

	prefix := "[[maybe_unused]] static "
	if fn.Name == "main" && fn.Recv == nil {
		prefix = ""
	}
	return fmt.Sprintf("%s%s %s(%s)", prefix, ret, name, strings.Join(params, ", "))
}

func cFuncName(fn *ast.FuncDecl) string {
	if fn.Recv != nil {
		return methodName(receiverBase(fn.Recv.Type), fn.Name)
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
	g.entry = false
	if fn.Name == "main" && fn.Recv == nil {
		return // emitted by genEntry
	}
	g.line("%s {", g.signature(fn))
	g.ind++
	g.genBody(fn)
	g.ind--
	g.line("}")
	g.line("")
}

// genBody emits a function body with ownership tracking.
func (g *gen) genBody(fn *ast.FuncDecl) {
	sig, offset := g.funcSig(fn)
	g.retType = sig.Ret
	g.pushScope()
	if fn.Recv != nil && sig != nil {
		g.declareOwned(parameterName(fn.Recv.Name, 0), sig.Params[0])
	}
	if sig != nil {
		for i, p := range fn.Params {
			g.declareOwned(parameterName(p.Name, i+offset), sig.Params[i+offset])
		}
	}
	for _, s := range fn.Body.Stmts {
		g.genStmt(s)
	}
	if !blockTerminates(fn.Body.Stmts) {
		g.dropScope()
	}
	g.popScope()
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
	g.entry = true
	g.retType = sema.VoidT
	g.line("int main() {")
	g.ind++
	g.pushScope()
	for _, s := range mainFn.Body.Stmts {
		g.genStmt(s)
	}
	if !blockTerminates(mainFn.Body.Stmts) {
		g.dropScope()
	}
	g.popScope()
	g.line("return 0;")
	g.ind--
	g.line("}")
}

// --- statements ---

func (g *gen) genStmt(s ast.Stmt) {
	defer g.dropTemps()
	switch st := s.(type) {
	case *ast.LetStmt:
		t, ok := g.info.LetTypes[st]
		if !ok {
			t = sema.Int
		}
		name := mangle(st.Name)
		if name == "_" {
			name = g.temp()
		}
		g.line("%s %s = %s;", cType(t), name, g.take(st.Value))
		g.declareOwned(name, t)

	case *ast.AssignStmt:
		// Evaluate the new value before releasing the old one (x = x + "!").
		target := g.place(st.Target)
		value := g.take(st.Value)
		tmp := g.temp()
		g.line("%s %s = %s;", cType(g.info.ExprTypes[st.Value]), tmp, value)
		g.flush()
		if t := g.info.ExprTypes[st.Target]; ownsValue(t) {
			g.emitDrop(target, t)
		}
		g.line("%s = %s;", target, tmp)

	case *ast.ExprStmt:
		if g.isVoidCall(st.X) {
			g.line("%s;", g.expr(st.X))
		} else {
			g.line("(void)(%s);", g.expr(st.X))
		}

	case *ast.ReturnStmt:
		g.genReturn(st)

	case *ast.IfStmt:
		g.line("if (%s) {", g.condition(st.Cond))
		g.ind++
		g.pushScope()
		for _, s := range st.Then.Stmts {
			g.genStmt(s)
		}
		if !blockTerminates(st.Then.Stmts) {
			g.dropScope()
		}
		g.popScope()
		g.ind--
		g.genElse(st.Else)

	case *ast.MatchStmt:
		g.genMatch(st)
	case *ast.IfLetStmt:
		g.genIfLet(st)

	case *ast.WhileStmt:
		g.line("while (true) {")
		g.ind++
		g.line("if (!(%s)) break;", g.condition(st.Cond))
		g.ind--
		g.ind++
		g.pushScope()
		g.loopScopes = append(g.loopScopes, len(g.scopes)-1)
		for _, s := range st.Body.Stmts {
			g.genStmt(s)
		}
		g.loopScopes = g.loopScopes[:len(g.loopScopes)-1]
		if !blockTerminates(st.Body.Stmts) {
			g.dropScope()
		}
		g.popScope()
		g.ind--
		g.line("}")

	case *ast.BreakStmt:
		snap := g.snapshotOwned()
		g.dropToLoop()
		g.restoreOwned(snap)
		g.line("break;")

	case *ast.ContinueStmt:
		snap := g.snapshotOwned()
		g.dropToLoop()
		g.restoreOwned(snap)
		g.line("continue;")

	case *ast.ForStmt:
		iterT := g.info.ExprTypes[st.Iter]
		var sl *sema.Slice
		if s, ok := iterT.(*sema.Slice); ok {
			sl = s
		} else if p, ok := iterT.(*sema.Ptr); ok {
			if s, ok := p.Elem.(*sema.Slice); ok {
				sl = s
			}
		}

		g.line("{")
		g.ind++
		g.pushScope()

		tmpPtr := g.temp()
		if _, isRef := iterT.(*sema.Ptr); isRef {
			iterVal := g.expr(st.Iter)
			g.line("%s* %s = %s;", sema.MonoName(sl), tmpPtr, iterVal)
		} else if sema.IsPlace(st.Iter) {
			iterVal := g.rawExpr(st.Iter)
			g.line("%s* %s = &%s;", sema.MonoName(sl), tmpPtr, iterVal)
		} else {
			iterVal := g.expr(st.Iter)
			g.line("%s* %s = &%s;", sema.MonoName(sl), tmpPtr, iterVal)
		}

		tmpLen := g.temp()
		g.line("int64_t %s = %s->len;", tmpLen, tmpPtr)

		tmpIdx := g.temp()
		g.line("for (int64_t %s = 0; %s < %s; %s++) {", tmpIdx, tmpIdx, tmpLen, tmpIdx)
		g.ind++
		g.pushScope()
		g.loopScopes = append(g.loopScopes, len(g.scopes)-1)

		if st.Key != "" && st.Key != "_" {
			keyName := mangle(st.Key)
			g.line("int64_t %s = %s;", keyName, tmpIdx)
		}
		if st.Value != "" && st.Value != "_" {
			vt := g.info.ForTypes[st]
			valName := mangle(st.Value)
			if _, isRef := vt.(*sema.Ptr); isRef {
				g.line("%s* %s = &%s->data[%s];", cType(sl.Elem), valName, tmpPtr, tmpIdx)
			} else {
				g.line("%s %s = %s->data[%s];", cType(sl.Elem), valName, tmpPtr, tmpIdx)
			}
		}

		for _, s := range st.Body.Stmts {
			g.genStmt(s)
		}

		g.loopScopes = g.loopScopes[:len(g.loopScopes)-1]
		if !blockTerminates(st.Body.Stmts) {
			g.dropScope()
		}
		g.popScope()
		g.ind--
		g.line("}")

		g.dropScope()
		g.popScope()
		g.ind--
		g.line("}")

	case *ast.Block:
		g.line("{")
		g.ind++
		g.pushScope()
		for _, s := range st.Stmts {
			g.genStmt(s)
		}
		g.dropScope()
		g.popScope()
		g.ind--
		g.line("}")
	}
}

// genReturn releases every live owner on the way out, keeping the returned
// value alive in a temporary.
func (g *gen) genReturn(st *ast.ReturnStmt) {
	// Drops are emitted for this exit path only; the ownership state of the
	// enclosing flow must stay intact because more statements may follow.
	emit := func(ret string) {
		snap := g.snapshotOwned()
		g.dropAll()
		g.restoreOwned(snap)
		if ret == "" {
			if g.entry {
				g.line("return 0;")
			} else {
				g.line("return;")
			}
		} else {
			g.line("return %s;", ret)
		}
	}

	if st.X == nil {
		emit("")
		return
	}
	retT := g.info.ExprTypes[st.X]
	value := g.take(st.X)
	tmp := g.temp()
	g.line("%s %s = %s;", cType(retT), tmp, value)
	emit(tmp)
}

func (g *gen) genElse(e ast.Stmt) {
	if e == nil {
		g.line("}")
		return
	}
	switch el := e.(type) {
	case *ast.IfStmt:
		g.line("} else {")
		g.ind++
		g.genIfInline(el)
		g.ind--
		g.line("}")
	case *ast.IfLetStmt:
		g.line("} else {")
		g.ind++
		g.pushScope()
		g.genIfLet(el)
		g.dropScope()
		g.popScope()
		g.ind--
		g.line("}")
	case *ast.Block:
		g.line("} else {")
		g.ind++
		g.pushScope()
		for _, s := range el.Stmts {
			g.genStmt(s)
		}
		if !blockTerminates(el.Stmts) {
			g.dropScope()
		}
		g.popScope()
		g.ind--
		g.line("}")
	}
}

func (g *gen) genIfInline(st *ast.IfStmt) {
	g.line("if (%s) {", g.condition(st.Cond))
	g.ind++
	g.pushScope()
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	if !blockTerminates(st.Then.Stmts) {
		g.dropScope()
	}
	g.popScope()
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
	g.line("%s %s = %s;", ct, tmp, g.take(st.Value))
	// The temporary owns the payload until it is moved into the binding.
	g.declareOwned(tmp, vt)

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
	snap := g.snapshotOwned()

	g.line("if (%s) {", cond)
	g.ind++
	g.pushScope()
	g.line("%s %s = %s.%s;", cType(bound), mangle(st.Name), tmp, field)
	g.declareOwned(mangle(st.Name), bound)
	// The binding took the payload out of the temporary.
	g.markMoved(tmp)
	for _, s := range st.Then.Stmts {
		g.genStmt(s)
	}
	g.dropScope()
	g.popScope()
	g.ind--
	g.restoreOwned(snap)

	// If the pattern did not match, the temporary still owns the value.
	g.line("} else {")
	g.ind++
	g.pushScope()
	g.dropScopeIfOwned(tmp, vt)
	if st.Else != nil {
		switch el := st.Else.(type) {
		case *ast.Block:
			for _, s := range el.Stmts {
				g.genStmt(s)
			}
		case *ast.IfStmt:
			g.genIfInline(el)
		case *ast.IfLetStmt:
			g.genIfLet(el)
		}
	}
	g.dropScope()
	g.popScope()
	g.ind--
	g.line("}")
}

// dropScopeIfOwned releases a single temporary if it is still owned.
func (g *gen) dropScopeIfOwned(name string, t sema.Type) {
	for i := len(g.scopes) - 1; i >= 0; i-- {
		sc := g.scopes[i]
		for j := range sc.vars {
			if sc.vars[j].name == name {
				if sc.vars[j].owned {
					g.emitDrop(name, t)
					sc.vars[j].owned = false
				}
				return
			}
		}
	}
}

// blockTerminates reports whether control always leaves the block via return.
func blockTerminates(stmts []ast.Stmt) bool {
	if len(stmts) == 0 {
		return false
	}
	switch stmts[len(stmts)-1].(type) {
	case *ast.ReturnStmt, *ast.BreakStmt, *ast.ContinueStmt:
		return true
	}
	return ast.Terminates(&ast.Block{Stmts: stmts})
}

func (g *gen) genMatch(st *ast.MatchStmt) {
	t := g.info.ExprTypes[st.Value]
	value := g.take(st.Value)
	tmp := g.temp()
	g.line("%s %s = %s;", cType(t), tmp, value)
	g.declareOwned(tmp, t)
	snap := g.snapshotOwned()
	cond := tmp + ".some"
	if _, ok := t.(*sema.ResultT); ok {
		cond = tmp + ".ok"
	}
	if st.Arms[0].Kind == "None" || st.Arms[0].Kind == "Err" {
		cond = "!" + cond
	}
	g.line("if (%s) {", cond)
	for i, arm := range st.Arms {
		if i > 0 {
			g.line("} else {")
		}
		g.restoreOwned(snap)
		g.ind++
		g.pushScope()
		if bound := g.info.MatchTypes[arm]; bound != nil {
			field := "val"
			if arm.Kind == "Err" {
				field = "err"
			}
			g.line("%s %s = %s.%s;", cType(bound), mangle(arm.Name), tmp, field)
			g.declareOwned(mangle(arm.Name), bound)
		}
		g.markMoved(tmp)
		for _, s := range arm.Body.Stmts {
			g.genStmt(s)
		}
		if !ast.Terminates(arm.Body) {
			g.dropScope()
		}
		g.popScope()
		g.ind--
	}
	g.line("}")
	g.markMoved(tmp)
}

// dropTemps releases borrowed expression results after their full statement.
func (g *gen) dropTemps() {
	if len(g.scopes) == 0 {
		return
	}
	sc := g.scopes[len(g.scopes)-1]
	for i := len(sc.vars) - 1; i >= 0; i-- {
		v := &sc.vars[i]
		if v.temporary && v.owned {
			g.emitDrop(v.name, v.typ)
			v.owned = false
		}
	}
}

// place renders a storage location without moving it or evaluating it twice.
func (g *gen) place(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.Ident:
		return mangle(ex.Name)
	case *ast.Field:
		sep := "."
		if _, ok := g.info.ExprTypes[ex.X].(*sema.Ptr); ok {
			sep = "->"
		}
		return "(" + g.place(ex.X) + ")" + sep + mangle(ex.Name)
	case *ast.Index:
		base := g.place(ex.X)
		idx := g.temp()
		g.line("int64_t %s = %s;", idx, g.expr(ex.Idx))
		dot := "."
		if _, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
			dot = "->"
		}
		g.line("gors_bounds(%s, (%s)%slen);", idx, base, dot)
		return "(" + base + ")" + dot + "data[" + idx + "]"
	case *ast.Unary:
		if ex.Op == "*" {
			return "(*" + g.expr(ex.X) + ")"
		}
	}
	return g.expr(e)
}

// take transfers a value. Zeroing only the moved place leaves sibling fields
// available for their owner's eventual drop, including on conditional paths.
func (g *gen) take(e ast.Expr) string {
	t := g.info.ExprTypes[e]
	if id, ok := e.(*ast.Ident); ok && id.Name == "None" {
		return g.expr(e)
	}
	if ownsValue(t) {
		switch e.(type) {
		case *ast.Ident, *ast.Field, *ast.Index:
			name := g.place(e)
			tmp := g.temp()
			g.line("%s %s = %s;", cType(t), tmp, name)
			g.line("%s = (%s){0};", name, cType(t))
			g.declareOwned(tmp, t)
			sc := g.scopes[len(g.scopes)-1]
			sc.vars[len(sc.vars)-1].temporary = true
			g.transfer(tmp)
			return tmp
		}
	}
	value := g.expr(e)
	if ownsValue(t) {
		g.transfer(value)
	}
	return value
}

func (g *gen) condition(e ast.Expr) string {
	g.pushScope()
	value := g.expr(e)
	tmp := g.temp()
	g.line("bool %s = %s;", tmp, value)
	g.dropScope()
	g.popScope()
	return tmp
}

func (g *gen) transfer(name string) {
	if len(g.transfers) == 0 {
		g.markMoved(name)
		return
	}
	i := len(g.transfers) - 1
	g.transfers[i] = append(g.transfers[i], name)
}

func (g *gen) expr(e ast.Expr) string {
	// Keep operands owned until this operation is evaluated. A later operand
	// may return early via ?, in which case earlier operands must still drop.
	g.transfers = append(g.transfers, nil)
	value := g.rawExpr(e)
	transfers := g.transfers[len(g.transfers)-1]
	g.transfers = g.transfers[:len(g.transfers)-1]
	defer func() {
		for _, name := range transfers {
			g.markMoved(name)
		}
	}()
	t := g.info.ExprTypes[e]
	fresh := false
	switch e.(type) {
	case *ast.Ident, *ast.Field, *ast.Index, *ast.MatchExpr:
		return value
	case *ast.Call:
		fresh = !g.isVoidCall(e)
	default:
		fresh = ownsValue(t)
	}
	if !fresh {
		return value
	}
	tmp := g.temp()
	g.line("%s %s = %s;", cType(t), tmp, value)
	if ownsValue(t) {
		g.declareOwned(tmp, t)
		sc := g.scopes[len(g.scopes)-1]
		sc.vars[len(sc.vars)-1].temporary = true
	}
	return tmp
}

// --- expressions ---

func (g *gen) rawExpr(e ast.Expr) string {
	switch ex := e.(type) {
	case *ast.IntLit:
		return strconv.FormatInt(ex.Value, 10) + "LL"
	case *ast.FloatLit:
		return strconv.FormatFloat(ex.Value, 'g', -1, 64)
	case *ast.StringLit:
		return "gors_str_new(" + cQuote(ex.Value) + ")"
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
		name := mangle(ex.Name)
		return name

	case *ast.Unary:
		return "(" + ex.Op + g.expr(ex.X) + ")"

	case *ast.Binary:
		if (ex.Op == "==" || ex.Op == "!=") && isStringType(g.info.ExprTypes[ex.X]) {
			cmp := "gors_str_eq(" + g.strOperand(ex.X) + ", " + g.strOperand(ex.Y) + ")"
			if ex.Op == "!=" {
				return "(!(" + cmp + "))"
			}
			return cmp
		}
		if ex.Op == "+" && isStringType(g.info.ExprTypes[ex.X]) {
			return "gors_concat(" + g.strOperand(ex.X) + ", " + g.strOperand(ex.Y) + ")"
		}
		if ex.Op == "&&" || ex.Op == "||" {
			left := g.expr(ex.X)
			tmp := g.temp()
			g.line("bool %s = %s;", tmp, left)
			cond := tmp
			if ex.Op == "||" {
				cond = "!" + tmp
			}
			g.line("if (%s) {", cond)
			g.ind++
			g.pushScope()
			right := g.expr(ex.Y)
			g.line("%s = %s;", tmp, right)
			g.dropScope()
			g.popScope()
			g.ind--
			g.line("}")
			return tmp
		}
		left := g.expr(ex.X)
		right := g.expr(ex.Y)
		return "(" + left + " " + ex.Op + " " + right + ")"

	case *ast.AddrOf:
		return "&(" + g.place(ex.X) + ")"

	case *ast.Field:
		sep := "."
		if _, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
			sep = "->"
		}
		return g.expr(ex.X) + sep + mangle(ex.Name)

	case *ast.Index:
		if _, isSlice := g.info.ExprTypes[ex.X].(*sema.Slice); isSlice {
			return "GORS_AT(" + g.expr(ex.X) + ", " + g.expr(ex.Idx) + ")"
		}
		if p, isPtr := g.info.ExprTypes[ex.X].(*sema.Ptr); isPtr {
			if _, isSlice := p.Elem.(*sema.Slice); isSlice {
				return "GORS_AT((*" + g.expr(ex.X) + "), " + g.expr(ex.Idx) + ")"
			}
		}
		return g.expr(ex.X) + "[" + g.expr(ex.Idx) + "]"

	case *ast.StructLit:
		var parts []string
		for _, fi := range ex.Fields {
			parts = append(parts, "."+mangle(fi.Name)+" = "+g.take(fi.Value))
		}
		return "(" + mangle(ex.Type) + "){ " + strings.Join(parts, ", ") + " }"

	case *ast.SliceLit:
		return g.sliceLit(ex)

	case *ast.SliceExpr:
		xt := g.info.ExprTypes[ex.X]
		var base string
		var lenExpr string
		isStr := false
		var slName string

		if p, ok := xt.(*sema.Ptr); ok {
			base = "(*" + g.expr(ex.X) + ")"
			sl := p.Elem.(*sema.Slice)
			slName = sema.MonoName(sl)
			lenExpr = base + ".len"
		} else if sl, ok := xt.(*sema.Slice); ok {
			base = g.expr(ex.X)
			slName = sema.MonoName(sl)
			lenExpr = base + ".len"
		} else {
			base = g.expr(ex.X)
			isStr = true
			lenExpr = "(int64_t)strlen(" + base + ")"
		}

		lowStr := "0LL"
		if ex.Low != nil {
			lowStr = g.expr(ex.Low)
		}
		highStr := lenExpr
		if ex.High != nil {
			highStr = g.expr(ex.High)
		}

		if isStr {
			return fmt.Sprintf("gors_str_slice(%s, %s, %s)", base, lowStr, highStr)
		}
		return fmt.Sprintf("%s_slice(%s, %s, %s)", slName, base, lowStr, highStr)

	case *ast.TryExpr:
		return g.lowerTry(ex)

	case *ast.MatchExpr:
		retT := g.info.ExprTypes[ex]
		resTmp := g.temp()
		g.line("%s %s = {0};", cType(retT), resTmp)
		t := g.info.ExprTypes[ex.Value]
		valTmp := g.temp()
		g.line("%s %s = %s;", cType(t), valTmp, g.take(ex.Value))
		g.declareOwned(valTmp, t)
		snap := g.snapshotOwned()
		cond := valTmp + ".some"
		if _, ok := t.(*sema.ResultT); ok {
			cond = valTmp + ".ok"
		}
		if ex.Arms[0].Kind == "None" || ex.Arms[0].Kind == "Err" {
			cond = "!" + cond
		}
		g.line("if (%s) {", cond)
		for i, arm := range ex.Arms {
			if i > 0 {
				g.line("} else {")
			}
			g.restoreOwned(snap)
			g.ind++
			g.pushScope()
			if bound := g.info.MatchTypes[arm]; bound != nil {
				field := "val"
				if arm.Kind == "Err" {
					field = "err"
				}
				g.line("%s %s = %s.%s;", cType(bound), mangle(arm.Name), valTmp, field)
				g.declareOwned(mangle(arm.Name), bound)
			}
			g.markMoved(valTmp)
			if arm.Body != nil {
				for _, s := range arm.Body.Stmts {
					g.genStmt(s)
				}
			}
			if arm.Expr != nil {
				val := g.take(arm.Expr)
				g.line("%s = %s;", resTmp, val)
				if ownsValue(retT) {
					g.markMoved(val)
				}
			}
			if arm.Body == nil || !ast.Terminates(arm.Body) {
				g.dropScope()
			}
			g.popScope()
			g.ind--
		}
		g.line("}")
		g.markMoved(valTmp)
		if ownsValue(retT) {
			g.declareOwned(resTmp, retT)
			sc := g.scopes[len(g.scopes)-1]
			sc.vars[len(sc.vars)-1].temporary = true
		}
		return resTmp

	case *ast.Call:
		return g.call(ex)
	}
	return "0"
}

// strOperand renders a string operand without heap-allocating literals.
func (g *gen) strOperand(e ast.Expr) string {
	if lit, ok := e.(*ast.StringLit); ok {
		return cQuote(lit.Value)
	}
	return g.expr(e)
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
			return "strlen(" + g.expr(ex.Args[0]) + ")"
		case "push":
			ptr := g.temp()
			g.line("%s %s = %s;", cType(g.info.ExprTypes[ex.Args[0]]), ptr, g.expr(ex.Args[0]))
			sl := g.info.ExprTypes[ex.Args[0]].(*sema.Ptr).Elem.(*sema.Slice)
			return sema.MonoName(sl) + "_push(" + ptr + ", " + g.take(ex.Args[1]) + ")"

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
		parts = append(parts, g.take(e))
	}
	n := strconv.Itoa(len(ex.Elems))
	elem := cType(sl.Elem)
	return "(" + name + "){ .data = (" + elem + "*)gors_dup((" + elem + "[]){" +
		strings.Join(parts, ", ") + "}, " + n + ", sizeof(" + elem + ")), .len = " + n + ", .cap = " + n + " }"
}

func (g *gen) lowerTry(ex *ast.TryExpr) string {
	rt, ok := g.info.ExprTypes[ex.X].(*sema.ResultT)
	if !ok {
		return "0"
	}
	ct := cType(rt)
	inner := g.take(ex.X)
	if ownsValue(g.info.ExprTypes[ex.X]) {
		g.markMoved(inner)
	}
	tmp := g.temp()
	g.emitPending("%s %s = %s;", ct, tmp, inner)
	g.emitPending("if (!%s.ok) {", tmp)

	// Emit the early-return branch separately so it can release every live
	// owner without disturbing the ownership state of the happy path.
	g.flush()
	snap := g.snapshotOwned()
	var buf bytes.Buffer
	oldOut, oldInd := g.out, g.ind
	g.out = &buf
	g.ind = oldInd + 1
	g.line("%s __gors_ret = { .err = %s.err };", cType(g.retType), tmp)
	g.dropAll()
	g.line("return __gors_ret;")
	g.out = oldOut
	g.ind = oldInd
	g.restoreOwned(snap)

	g.pending.Write(buf.Bytes())
	g.emitPending("}")
	return tmp + ".val"
}

func (g *gen) snapshotOwned() []bool {
	var out []bool
	for _, sc := range g.scopes {
		for _, v := range sc.vars {
			out = append(out, v.owned)
		}
	}
	return out
}

func (g *gen) restoreOwned(snap []bool) {
	i := 0
	for _, sc := range g.scopes {
		for j := range sc.vars {
			if i < len(snap) {
				sc.vars[j].owned = snap[i]
			}
			i++
		}
	}
}

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
	name := methodName(st.Name, field.Name)
	var recv string
	_, havePtr := recvType.(*sema.Ptr)
	wantPtr := false
	if sig := g.info.Methods[st.Name][field.Name]; sig != nil && len(sig.Params) > 0 {
		if _, ok := sig.Params[0].(*sema.Ptr); ok {
			wantPtr = true
		}
	}
	if !wantPtr && ownsValue(recvType) {
		recv = g.take(field.X)
	} else {
		recv = g.expr(field.X)
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
		parts[i] = g.take(a)
	}
	return strings.Join(parts, ", ")
}

func (g *gen) printCall(args []ast.Expr, newline bool) string {
	var fmts []string
	var vals []string
	for _, a := range args {
		t := g.info.ExprTypes[a]
		// String literals passed to println are not allocated.
		if lit, ok := a.(*ast.StringLit); ok {
			fmts = append(fmts, "%s")
			vals = append(vals, cQuote(lit.Value))
			continue
		}
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
		return "Slice_int64"
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

// Keep user names in a separate, injective namespace from C and runtime names.
func mangle(name string) string {
	if name == "_" || name == "" {
		return name
	}
	return "__gors_user_" + name
}
func parameterName(name string, index int) string {
	if name == "_" {
		return fmt.Sprintf("__gors_arg_%d", index)
	}
	return mangle(name)
}
func methodName(base, name string) string {
	return fmt.Sprintf("__gors_method_%d_%s_%s", len(base), base, name)
}

package sema

import (
	"fmt"

	"gors/internal/ast"
	"gors/internal/token"
)

// Check resolves types, validates declarations and checks all bodies.
func Check(prog *ast.Program) (*Info, []*Error) {
	c := &checker{
		info: &Info{
			Structs:    map[string]*StructT{},
			Funcs:      map[string]*FuncSig{},
			Methods:    map[string]map[string]*FuncSig{},
			MethodSel:  map[ast.Expr]bool{},
			IfLetTypes: map[*ast.IfLetStmt]Type{},
			ExprTypes:  map[ast.Expr]Type{},
			LetTypes:   map[*ast.LetStmt]Type{},
		},
	}
	c.declareStructs(prog)
	c.declareFuncs(prog)
	for _, fn := range prog.Funcs {
		c.checkFunc(fn)
	}
	return c.info, c.errs
}

type checker struct {
	info   *Info
	errs   []*Error
	scopes []map[string]*varInfo

	curRet  Type
	curFunc string
}

type varInfo struct {
	Type Type
	Mut  bool
}

func (c *checker) errf(code string, pos token.Pos, format string, args ...any) {
	c.errs = append(c.errs, &Error{Code: code, Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

// --- declarations ---

func (c *checker) declareStructs(prog *ast.Program) {
	for _, sd := range prog.Structs {
		if _, dup := c.info.Structs[sd.Name]; dup {
			c.errf("E100", sd.At, "struct %q is already declared", sd.Name)
			continue
		}
		st := &StructT{Name: sd.Name, Fields: map[string]Type{}}
		c.info.Structs[sd.Name] = st
	}

	for _, sd := range prog.Structs {
		st, ok := c.info.Structs[sd.Name]
		if !ok {
			continue
		}
		seen := map[string]bool{}
		for _, f := range sd.Fields {
			if seen[f.Name] {
				c.errf("E101", f.At, "duplicate field %q in struct %q", f.Name, sd.Name)
				continue
			}
			seen[f.Name] = true
			ft := c.resolveType(f.Type)
			if ft == nil {
				continue
			}
			st.Fields[f.Name] = ft
			st.Order = append(st.Order, f.Name)
		}
	}
}

func (c *checker) declareFuncs(prog *ast.Program) {
	for _, fn := range prog.Funcs {
		if fn.Recv != nil {
			c.declareMethod(fn)
			continue
		}
		if _, dup := c.info.Funcs[fn.Name]; dup {
			c.errf("E102", fn.At, "function %q is already declared", fn.Name)
			continue
		}
		sig := &FuncSig{Name: fn.Name, Ret: VoidT}
		for _, p := range fn.Params {
			pt := c.resolveType(p.Type)
			if pt == nil {
				pt = Int
			}
			sig.Params = append(sig.Params, pt)
		}
		if fn.Ret != nil {
			if rt := c.resolveType(fn.Ret); rt != nil {
				sig.Ret = rt
			}
		}
		c.info.Funcs[fn.Name] = sig
	}
}

func (c *checker) declareMethod(fn *ast.FuncDecl) {
	rt := c.resolveType(fn.Recv.Type)
	if rt == nil {
		return
	}
	base := rt
	if p, ok := base.(*Ptr); ok {
		base = p.Elem
	}
	st, ok := base.(*StructT)
	if !ok {
		c.errf("E140", fn.Recv.At, "method receiver must be a struct or &struct, got %s", rt.String())
		return
	}
	if c.info.Methods[st.Name] == nil {
		c.info.Methods[st.Name] = map[string]*FuncSig{}
	}
	if _, dup := c.info.Methods[st.Name][fn.Name]; dup {
		c.errf("E141", fn.At, "method %q is already declared on %q", fn.Name, st.Name)
		return
	}
	if _, isField := st.Fields[fn.Name]; isField {
		c.errf("E142", fn.At, "method %q conflicts with a field of %q", fn.Name, st.Name)
	}

	sig := &FuncSig{Name: st.Name + "." + fn.Name, Ret: VoidT}
	sig.Params = append(sig.Params, rt) // receiver is Params[0]
	for _, p := range fn.Params {
		pt := c.resolveType(p.Type)
		if pt == nil {
			pt = Int
		}
		sig.Params = append(sig.Params, pt)
	}
	if fn.Ret != nil {
		if r := c.resolveType(fn.Ret); r != nil {
			sig.Ret = r
		}
	}
	c.info.Methods[st.Name][fn.Name] = sig
}

func (c *checker) resolveType(te ast.TypeExpr) Type {
	if te == nil {
		return nil
	}
	switch t := te.(type) {
	case *ast.NamedType:
		switch t.Name {
		case "int":
			return Int
		case "float":
			return Float
		case "bool":
			return Bool
		case "string":
			return String
		case "Option":
			if len(t.Args) != 1 {
				c.errf("E145", t.At, "Option requires exactly one type argument")
				return nil
			}
			elem := c.resolveType(t.Args[0])
			if elem == nil {
				return nil
			}
			return &OptionT{Elem: elem}
		case "Result":
			if len(t.Args) != 2 {
				c.errf("E146", t.At, "Result requires exactly two type arguments")
				return nil
			}
			ok := c.resolveType(t.Args[0])
			bad := c.resolveType(t.Args[1])
			if ok == nil || bad == nil {
				return nil
			}
			return &ResultT{Ok: ok, Err: bad}
		}
		if len(t.Args) > 0 {
			c.errf("E147", t.At, "type %q is not generic", t.Name)
			return nil
		}
		if st, ok := c.info.Structs[t.Name]; ok {
			return st
		}
		c.errf("E103", t.At, "unknown type %q", t.Name)
		return nil
	case *ast.PtrType:
		elem := c.resolveType(t.Elem)
		if elem == nil {
			return nil
		}
		return &Ptr{Elem: elem, Mut: t.Mut}
	case *ast.SliceType:
		elem := c.resolveType(t.Elem)
		if elem == nil {
			return nil
		}
		return &Slice{Elem: elem}
	}
	return nil
}

// --- scopes ---

func (c *checker) pushScope() { c.scopes = append(c.scopes, map[string]*varInfo{}) }

func (c *checker) popScope() { c.scopes = c.scopes[:len(c.scopes)-1] }

func (c *checker) declare(name string, v *varInfo) bool {
	scope := c.scopes[len(c.scopes)-1]
	if _, dup := scope[name]; dup {
		return false
	}
	scope[name] = v
	return true
}

func (c *checker) lookup(name string) (*varInfo, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if v, ok := c.scopes[i][name]; ok {
			return v, true
		}
	}
	return nil, false
}

// --- function bodies ---

func (c *checker) checkFunc(fn *ast.FuncDecl) {
	var sig *FuncSig
	offset := 0
	if fn.Recv != nil {
		rt := c.resolveType(fn.Recv.Type)
		if rt == nil {
			return
		}
		base := rt
		if p, ok := base.(*Ptr); ok {
			base = p.Elem
		}
		st, ok := base.(*StructT)
		if !ok {
			return
		}
		sig = c.info.Methods[st.Name][fn.Name]
		offset = 1
	} else {
		sig = c.info.Funcs[fn.Name]
	}
	if sig == nil {
		return
	}
	c.curRet = sig.Ret
	c.curFunc = fn.Name
	c.scopes = nil
	c.pushScope()
	if fn.Recv != nil {
		c.declare(fn.Recv.Name, &varInfo{Type: sig.Params[0], Mut: false})
	}
	for i, p := range fn.Params {
		if !c.declare(p.Name, &varInfo{Type: sig.Params[i+offset], Mut: false}) {
			c.errf("E104", p.At, "duplicate parameter %q", p.Name)
		}
	}
	c.checkBlock(fn.Body)
	c.popScope()
}

func (c *checker) checkBlock(b *ast.Block) {
	c.pushScope()
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	c.popScope()
}

func (c *checker) checkStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		var declared Type
		if st.Type != nil {
			declared = c.resolveType(st.Type)
			if declared == nil {
				return
			}
		}
		var vt Type
		if declared != nil {
			vt = c.checkExprExpected(st.Value, declared)
		} else {
			vt = c.checkExpr(st.Value)
		}
		if vt == nil {
			// Keep the name bound to avoid cascading "undefined name" errors.
			c.declare(st.Name, &varInfo{Type: Int, Mut: st.Mut})
			return
		}
		if _, isVoid := vt.(*Void); isVoid {
			c.errf("E105", st.At, "let binding requires a value, got void expression")
			return
		}
		final := vt
		if declared != nil {
			if !assignable(declared, vt) {
				c.errf("E106", st.At, "cannot bind %s to %s", vt.String(), declared.String())
				return
			}
			final = declared
		}
		if !c.declare(st.Name, &varInfo{Type: final, Mut: st.Mut}) {
			c.errf("E107", st.At, "%q is already declared in this scope", st.Name)
		}
		c.info.LetTypes[st] = final
		c.info.ExprTypes[st.Value] = vt

	case *ast.AssignStmt:
		tt := c.checkExpr(st.Target)
		if tt == nil {
			return
		}
		vt := c.checkExprExpected(st.Value, tt)
		if tt == nil || vt == nil {
			return
		}
		if !isPlace(st.Target) {
			c.errf("E108", st.At, "cannot assign to this expression")
			return
		}
		if !assignable(tt, vt) {
			c.errf("E109", st.At, "cannot assign %s to %s", vt.String(), tt.String())
		}

	case *ast.ExprStmt:
		c.checkExpr(st.X)

	case *ast.ReturnStmt:
		if st.X == nil {
			if _, isVoid := c.curRet.(*Void); !isVoid {
				c.errf("E110", st.At, "function %q must return %s", c.curFunc, c.curRet.String())
			}
			return
		}
		vt := c.checkExprExpected(st.X, c.curRet)
		if vt == nil {
			return
		}
		if _, isVoid := c.curRet.(*Void); isVoid {
			c.errf("E111", st.At, "function %q returns nothing", c.curFunc)
			return
		}
		if !assignable(c.curRet, vt) {
			c.errf("E112", st.At, "cannot return %s from function returning %s", vt.String(), c.curRet.String())
		}

	case *ast.IfStmt:
		ct := c.checkExpr(st.Cond)
		if ct != nil {
			if b, ok := ct.(*Basic); !ok || b.Name != "bool" {
				c.errf("E113", st.Cond.Pos(), "if condition must be bool, got %s", ct.String())
			}
		}
		c.checkBlock(st.Then)
		if st.Else != nil {
			c.checkStmt(st.Else)
		}

	case *ast.WhileStmt:
		ct := c.checkExpr(st.Cond)
		if ct != nil {
			if b, ok := ct.(*Basic); !ok || b.Name != "bool" {
				c.errf("E113", st.Cond.Pos(), "while condition must be bool, got %s", ct.String())
			}
		}
		c.checkBlock(st.Body)

	case *ast.IfLetStmt:
		vt := c.checkExpr(st.Value)
		if vt == nil {
			return
		}
		var bound Type
		switch st.Kind {
		case "Some":
			opt, ok := vt.(*OptionT)
			if !ok {
				c.errf("E151", st.At, "if let Some requires Option<T>, got %s", vt.String())
				return
			}
			bound = opt.Elem
		case "Ok", "Err":
			res, ok := vt.(*ResultT)
			if !ok {
				c.errf("E151", st.At, "if let %s requires Result<T, E>, got %s", st.Kind, vt.String())
				return
			}
			if st.Kind == "Ok" {
				bound = res.Ok
			} else {
				bound = res.Err
			}
		default:
			c.errf("E152", st.At, "unknown pattern %q; expected Some, Ok or Err", st.Kind)
			return
		}
		c.info.IfLetTypes[st] = bound
		c.checkBlockWith(st.Then, st.Name, bound)
		if st.Else != nil {
			c.checkStmt(st.Else)
		}

	case *ast.Block:
		c.checkBlock(st)
	}
}

func (c *checker) checkBlockWith(b *ast.Block, name string, t Type) {
	c.pushScope()
	c.declare(name, &varInfo{Type: t, Mut: false})
	for _, s := range b.Stmts {
		c.checkStmt(s)
	}
	c.popScope()
}

// checkExprExpected checks e knowing the type the context expects.
// It is what makes `None` and `Ok(x)` inferable.
func (c *checker) checkExprExpected(e ast.Expr, expected Type) Type {
	if expected == nil {
		return c.checkExpr(e)
	}
	if id, ok := e.(*ast.Ident); ok && id.Name == "None" {
		if _, isOpt := expected.(*OptionT); isOpt {
			c.info.ExprTypes[e] = expected
			return expected
		}
		c.errf("E150", id.At, "cannot infer the type of None; annotate it as Option<T>")
		return nil
	}
	if lit, ok := e.(*ast.SliceLit); ok && len(lit.Elems) == 0 {
		if sl, isSlice := expected.(*Slice); isSlice {
			c.info.ExprTypes[e] = sl
			return sl
		}
		c.errf("E153", lit.At, "cannot infer the element type of an empty slice literal; annotate it as []T")
		return nil
	}
	if call, ok := e.(*ast.Call); ok {
		if id, ok := call.Fn.(*ast.Ident); ok && (id.Name == "Ok" || id.Name == "Err") {
			rt, isRes := expected.(*ResultT)
			if !isRes {
				c.errf("E150", call.At, "cannot infer the Result type of %s; annotate it", id.Name)
				for _, a := range call.Args {
					c.checkExpr(a)
				}
				return nil
			}
			if len(call.Args) != 1 {
				c.errf("E148", call.At, "%s takes exactly one argument", id.Name)
				return rt
			}
			at := c.checkExpr(call.Args[0])
			want := rt.Ok
			if id.Name == "Err" {
				want = rt.Err
			}
			if at != nil && !assignable(want, at) {
				c.errf("E149", call.At, "%s expects %s, got %s", id.Name, want.String(), at.String())
			}
			c.info.ExprTypes[e] = rt
			return rt
		}
	}
	return c.checkExpr(e)
}

func (c *checker) checkExpr(e ast.Expr) Type {
	t := c.expr(e)
	if t != nil {
		c.info.ExprTypes[e] = t
	}
	return t
}

func (c *checker) expr(e ast.Expr) Type {
	switch ex := e.(type) {
	case *ast.IntLit:
		return Int
	case *ast.FloatLit:
		return Float
	case *ast.StringLit:
		return String
	case *ast.BoolLit:
		return Bool

	case *ast.Ident:
		if v, ok := c.lookup(ex.Name); ok {
			return v.Type
		}
		if ex.Name == "None" {
			c.errf("E150", ex.At, "cannot infer the type of None; annotate it as Option<T>")
			return nil
		}
		if _, ok := c.info.Funcs[ex.Name]; ok {
			c.errf("E114", ex.At, "function %q used as a value; call it instead", ex.Name)
			return nil
		}
		if isBuiltin(ex.Name) {
			c.errf("E114", ex.At, "%q is a builtin and cannot be used as a value", ex.Name)
			return nil
		}
		c.errf("E115", ex.At, "undefined name %q", ex.Name)
		return nil

	case *ast.Unary:
		xt := c.checkExpr(ex.X)
		if xt == nil {
			return nil
		}
		switch ex.Op {
		case "-":
			if !isNumeric(xt) {
				c.errf("E116", ex.At, "unary - requires a number, got %s", xt.String())
				return nil
			}
			return xt
		case "!":
			if !isBool(xt) {
				c.errf("E117", ex.At, "! requires bool, got %s", xt.String())
				return nil
			}
			return Bool
		}
		return nil

	case *ast.Binary:
		xt := c.checkExpr(ex.X)
		yt := c.checkExpr(ex.Y)
		if xt == nil || yt == nil {
			return nil
		}
		switch ex.Op {
		case "+", "-", "*", "/", "%":
			if ex.Op == "+" && isString(xt) && isString(yt) {
				return String
			}
			if !isNumeric(xt) || !isNumeric(yt) {
				c.errf("E118", ex.At, "operator %s requires numbers, got %s and %s", ex.Op, xt.String(), yt.String())
				return nil
			}
			if !sameType(xt, yt) {
				c.errf("E119", ex.At, "mismatched types in %s: %s and %s", ex.Op, xt.String(), yt.String())
				return nil
			}
			return xt
		case "==", "!=":
			if !sameType(xt, yt) {
				c.errf("E119", ex.At, "mismatched types in %s: %s and %s", ex.Op, xt.String(), yt.String())
				return nil
			}
			return Bool
		case "<", ">", "<=", ">=":
			if !isNumeric(xt) || !isNumeric(yt) || !sameType(xt, yt) {
				c.errf("E120", ex.At, "operator %s requires two numbers of the same type", ex.Op)
				return nil
			}
			return Bool
		case "&&", "||":
			if !isBool(xt) || !isBool(yt) {
				c.errf("E121", ex.At, "operator %s requires bools", ex.Op)
				return nil
			}
			return Bool
		}
		c.errf("E122", ex.At, "unknown operator %q", ex.Op)
		return nil

	case *ast.AddrOf:
		if !isPlace(ex.X) {
			c.errf("E123", ex.At, "cannot take a reference to this expression")
			return nil
		}
		xt := c.checkExpr(ex.X)
		if xt == nil {
			return nil
		}
		if ex.Mut {
			root, ok := rootIdent(ex.X)
			if ok {
				if v, found := c.lookup(root); found && !v.Mut {
					c.errf("E124", ex.At, "cannot take &mut of immutable %q; declare it with `let mut`", root)
				}
			}
		}
		return &Ptr{Elem: xt, Mut: ex.Mut}

	case *ast.Field:
		xt := c.checkExpr(ex.X)
		if xt == nil {
			return nil
		}
		base := xt
		if p, ok := base.(*Ptr); ok {
			base = p.Elem
		}
		st, ok := base.(*StructT)
		if !ok {
			c.errf("E125", ex.At, "type %s has no field %q", xt.String(), ex.Name)
			return nil
		}
		ft, ok := st.Fields[ex.Name]
		if !ok {
			if methods := c.info.Methods[st.Name]; methods != nil {
				if _, isMethod := methods[ex.Name]; isMethod {
					c.errf("E143", ex.At, "method %q must be called: %s.%s(...)", ex.Name, st.Name, ex.Name)
					return nil
				}
			}
			c.errf("E125", ex.At, "struct %q has no field %q", st.Name, ex.Name)
			return nil
		}
		return ft

	case *ast.StructLit:
		st, ok := c.info.Structs[ex.Type]
		if !ok {
			c.errf("E126", ex.At, "unknown struct %q", ex.Type)
			return nil
		}
		seen := map[string]bool{}
		for _, fi := range ex.Fields {
			if seen[fi.Name] {
				c.errf("E127", fi.At, "duplicate field %q", fi.Name)
				continue
			}
			seen[fi.Name] = true
			ft, exists := st.Fields[fi.Name]
			if !exists {
				c.errf("E128", fi.At, "struct %q has no field %q", st.Name, fi.Name)
				continue
			}
			vt := c.checkExprExpected(fi.Value, ft)
			if vt == nil {
				continue
			}
			if !assignable(ft, vt) {
				c.errf("E129", fi.At, "field %q expects %s, got %s", fi.Name, ft.String(), vt.String())
			}
		}
		for _, name := range st.Order {
			if !seen[name] {
				c.errf("E130", ex.At, "missing field %q in %s literal", name, st.Name)
			}
		}
		return st

	case *ast.TryExpr:
		xt := c.checkExpr(ex.X)
		if xt == nil {
			return nil
		}
		rt, ok := xt.(*ResultT)
		if !ok {
			c.errf("E160", ex.At, "operator ? requires Result<T, E>, got %s", xt.String())
			return nil
		}
		cur, ok := c.curRet.(*ResultT)
		if !ok {
			c.errf("E161", ex.At, "operator ? requires the enclosing function to return Result<T, E>")
			return nil
		}
		if !sameType(cur.Err, rt.Err) {
			c.errf("E162", ex.At, "? propagates %s but the function returns %s", rt.Err.String(), cur.Err.String())
			return nil
		}
		return rt.Ok

	case *ast.Call:
		return c.checkCall(ex)

	case *ast.SliceLit:
		if len(ex.Elems) == 0 {
			c.errf("E153", ex.At, "cannot infer the element type of an empty slice literal; annotate it as []T")
			return nil
		}
		var elem Type
		for i, e := range ex.Elems {
			t := c.checkExpr(e)
			if t == nil {
				continue
			}
			if elem == nil {
				elem = t
				continue
			}
			if !assignable(elem, t) {
				c.errf("E154", e.Pos(), "slice element %d has type %s, expected %s", i+1, t.String(), elem.String())
			}
		}
		if elem == nil {
			return nil
		}
		return &Slice{Elem: elem}

	case *ast.Index:
		xt := c.checkExpr(ex.X)
		if xt == nil {
			return nil
		}
		it := c.checkExpr(ex.Idx)
		if it != nil && !isInt(it) {
			c.errf("E131", ex.At, "index must be int, got %s", it.String())
		}
		if sl, ok := xt.(*Slice); ok {
			return sl.Elem
		}
		c.errf("E132", ex.At, "type %s is not indexable", xt.String())
		return nil
	}
	return nil
}

func (c *checker) checkCall(ex *ast.Call) Type {
	if id, ok := ex.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "Some":
			if len(ex.Args) != 1 {
				c.errf("E148", ex.At, "Some takes exactly one argument")
				return nil
			}
			at := c.checkExpr(ex.Args[0])
			if at == nil {
				return nil
			}
			return &OptionT{Elem: at}
		case "Ok", "Err":
			c.errf("E150", ex.At, "cannot infer the Result type of %s; annotate it", id.Name)
			for _, a := range ex.Args {
				c.checkExpr(a)
			}
			return nil
		}
	}

	// Method call: p.dist2(...)
	if field, ok := ex.Fn.(*ast.Field); ok {
		if sig, _ := c.resolveMethod(field); sig != nil {
			c.info.MethodSel[ex.Fn] = true
			if len(ex.Args) != len(sig.Params)-1 {
				c.errf("E144", ex.At, "method %q expects %d arguments, got %d",
					field.Name, len(sig.Params)-1, len(ex.Args))
				return sig.Ret
			}
			for i, a := range ex.Args {
				at := c.checkExprExpected(a, sig.Params[i+1])
				if at == nil {
					continue
				}
				if _, isVoid := at.(*Void); isVoid {
					c.errf("E138", a.Pos(), "argument %d is a void expression", i+1)
					continue
				}
				if !assignable(sig.Params[i+1], at) {
					c.errf("E139", a.Pos(), "argument %d of %q expects %s, got %s",
						i+1, field.Name, sig.Params[i+1].String(), at.String())
				}
			}
			return sig.Ret
		}
	}

	// Builtins.
	if id, ok := ex.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "println", "print":
			for _, a := range ex.Args {
				c.checkExpr(a)
			}
			return VoidT
		case "push":
			if len(ex.Args) != 2 {
				c.errf("E155", ex.At, "push takes exactly two arguments: push(&mut xs, value)")
				return VoidT
			}
			at := c.checkExpr(ex.Args[0])
			if at == nil {
				return VoidT
			}
			ptr, ok := at.(*Ptr)
			if !ok || !ptr.Mut {
				c.errf("E156", ex.Args[0].Pos(), "push requires &mut []T as its first argument")
				return VoidT
			}
			sl, ok := ptr.Elem.(*Slice)
			if !ok {
				c.errf("E156", ex.Args[0].Pos(), "push requires &mut []T, got %s", at.String())
				return VoidT
			}
			vt := c.checkExprExpected(ex.Args[1], sl.Elem)
			if vt != nil && !assignable(sl.Elem, vt) {
				c.errf("E157", ex.Args[1].Pos(), "cannot push %s into []%s", vt.String(), sl.Elem.String())
			}
			return VoidT

		case "len":
			if len(ex.Args) != 1 {
				c.errf("E133", ex.At, "len takes exactly one argument")
				return nil
			}
			at := c.checkExpr(ex.Args[0])
			if at == nil {
				return nil
			}
			if !isString(at) {
				if _, ok := at.(*Slice); !ok {
					c.errf("E134", ex.At, "len requires a string or a slice, got %s", at.String())
					return nil
				}
			}
			return Int
		}
	}

	fnIdent, ok := ex.Fn.(*ast.Ident)
	if !ok {
		c.errf("E135", ex.Fn.Pos(), "only named functions can be called")
		return nil
	}
	sig, ok := c.info.Funcs[fnIdent.Name]
	if !ok {
		c.errf("E136", ex.At, "undefined function %q", fnIdent.Name)
		return nil
	}
	if len(ex.Args) != len(sig.Params) {
		c.errf("E137", ex.At, "function %q expects %d arguments, got %d", sig.Name, len(sig.Params), len(ex.Args))
		return sig.Ret
	}
	for i, a := range ex.Args {
		at := c.checkExprExpected(a, sig.Params[i])
		if at == nil {
			continue
		}
		if _, isVoid := at.(*Void); isVoid {
			c.errf("E138", a.Pos(), "argument %d is a void expression", i+1)
			continue
		}
		if !assignable(sig.Params[i], at) {
			c.errf("E139", a.Pos(), "argument %d of %q expects %s, got %s", i+1, sig.Name, sig.Params[i].String(), at.String())
		}
	}
	return sig.Ret
}

// resolveMethod finds the method behind a selector expression like p.dist2.
func (c *checker) resolveMethod(field *ast.Field) (*FuncSig, *StructT) {
	t := c.checkExpr(field.X)
	if t == nil {
		return nil, nil
	}
	base := t
	if p, ok := base.(*Ptr); ok {
		base = p.Elem
	}
	st, ok := base.(*StructT)
	if !ok {
		return nil, nil
	}
	methods := c.info.Methods[st.Name]
	if methods == nil {
		return nil, nil
	}
	sig, ok := methods[field.Name]
	if !ok {
		return nil, nil
	}
	return sig, st
}

// --- helpers ---

func isBuiltin(name string) bool {
	switch name {
	case "println", "print", "len":
		return true
	}
	return false
}

func isPlace(e ast.Expr) bool {
	switch ex := e.(type) {
	case *ast.Ident:
		return true
	case *ast.Field:
		return isPlace(ex.X)
	case *ast.Index:
		return true
	}
	return false
}

func rootIdent(e ast.Expr) (string, bool) {
	switch ex := e.(type) {
	case *ast.Ident:
		return ex.Name, true
	case *ast.Field:
		return rootIdent(ex.X)
	case *ast.Index:
		return rootIdent(ex.X)
	}
	return "", false
}

func isNumeric(t Type) bool {
	b, ok := t.(*Basic)
	return ok && (b.Name == "int" || b.Name == "float")
}

func isInt(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "int"
}

func isBool(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "bool"
}

func isString(t Type) bool {
	b, ok := t.(*Basic)
	return ok && b.Name == "string"
}

func sameType(a, b Type) bool {
	return a.String() == b.String()
}

func assignable(dst, src Type) bool {
	return sameType(dst, src)
}

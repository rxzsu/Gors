package sema

import (
	"fmt"

	"gors/internal/ast"
	"gors/internal/token"
)

// CheckBorrow runs the ownership and borrowing analysis over all functions.
//
// The rules are a pragmatic subset of Rust's borrow checker:
//   - values of non-Copy types are moved, not copied;
//   - using a moved value is an error;
//   - a place may have many shared borrows OR exactly one mutable borrow;
//   - you cannot move/assign a place while it is borrowed;
//   - a borrow ends at the last use of the reference (NLL-style).
func CheckBorrow(prog *ast.Program, info *Info) []*Error {
	b := &borrowChecker{info: info, seqOf: map[ast.Expr]int{}, lastUse: map[string]int{}}
	for _, fn := range prog.Funcs {
		b.checkFunc(fn)
	}
	return b.errs
}

type varState struct {
	typ   Type
	mut   bool
	moved bool
}

type loan struct {
	root     string
	mutable  bool
	borrower string
	end      int
}

type borrowChecker struct {
	info *Info
	errs []*Error

	scopes  []map[string]*varState
	loans   []*loan
	seqOf   map[ast.Expr]int
	lastUse map[string]int

	curSeq int
	maxSeq int
}

func (b *borrowChecker) errf(code string, pos token.Pos, format string, args ...any) {
	b.errs = append(b.errs, &Error{Code: code, Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

// --- numbering pass ---

func (b *borrowChecker) numberFunc(fn *ast.FuncDecl) {
	b.seqOf = map[ast.Expr]int{}
	b.lastUse = map[string]int{}
	b.curSeq = 0
	b.numberBlock(fn.Body)
	b.maxSeq = b.curSeq
}

func (b *borrowChecker) numberBlock(blk *ast.Block) {
	for _, s := range blk.Stmts {
		b.numberStmt(s)
	}
}

func (b *borrowChecker) numberStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		b.numberExpr(st.Value)
	case *ast.AssignStmt:
		b.numberExpr(st.Target)
		b.numberExpr(st.Value)
	case *ast.ExprStmt:
		b.numberExpr(st.X)
	case *ast.ReturnStmt:
		if st.X != nil {
			b.numberExpr(st.X)
		}
	case *ast.IfStmt:
		b.numberExpr(st.Cond)
		b.numberBlock(st.Then)
		if st.Else != nil {
			b.numberStmt(st.Else)
		}
	case *ast.IfLetStmt:
		b.numberExpr(st.Value)
		b.numberBlock(st.Then)
		if st.Else != nil {
			b.numberStmt(st.Else)
		}
	case *ast.WhileStmt:
		b.numberExpr(st.Cond)
		b.numberBlock(st.Body)
	case *ast.Block:
		b.numberBlock(st)
	}
}

func (b *borrowChecker) numberExpr(e ast.Expr) {
	if e == nil {
		return
	}
	b.curSeq++
	b.seqOf[e] = b.curSeq

	switch ex := e.(type) {
	case *ast.Ident:
		b.lastUse[ex.Name] = b.curSeq
	case *ast.Unary:
		b.numberExpr(ex.X)
	case *ast.Binary:
		b.numberExpr(ex.X)
		b.numberExpr(ex.Y)
	case *ast.Call:
		for _, a := range ex.Args {
			b.numberExpr(a)
		}
	case *ast.Field:
		b.numberExpr(ex.X)
	case *ast.Index:
		b.numberExpr(ex.X)
		b.numberExpr(ex.Idx)
	case *ast.StructLit:
		for _, fi := range ex.Fields {
			b.numberExpr(fi.Value)
		}
	case *ast.SliceLit:
		for _, e := range ex.Elems {
			b.numberExpr(e)
		}
	case *ast.TryExpr:
		b.numberExpr(ex.X)
	case *ast.AddrOf:
		b.numberExpr(ex.X)
	}
}

// --- checking pass ---

func (b *borrowChecker) checkFunc(fn *ast.FuncDecl) {
	sig, ok := b.info.Funcs[fn.Name]
	if !ok {
		return
	}
	b.numberFunc(fn)

	b.scopes = nil
	b.loans = nil
	b.pushScope()
	for i, p := range fn.Params {
		b.declare(p.Name, &varState{typ: sig.Params[i], mut: false})
	}
	b.checkBlock(fn.Body)
	b.popScope()
}

func (b *borrowChecker) pushScope() { b.scopes = append(b.scopes, map[string]*varState{}) }
func (b *borrowChecker) popScope()  { b.scopes = b.scopes[:len(b.scopes)-1] }

func (b *borrowChecker) declare(name string, st *varState) {
	b.scopes[len(b.scopes)-1][name] = st
}

func (b *borrowChecker) lookup(name string) *varState {
	for i := len(b.scopes) - 1; i >= 0; i-- {
		if v, ok := b.scopes[i][name]; ok {
			return v
		}
	}
	return nil
}

func (b *borrowChecker) at(e ast.Expr) {
	if s, ok := b.seqOf[e]; ok {
		b.curSeq = s
	}
	b.prune()
}

func (b *borrowChecker) prune() {
	alive := b.loans[:0]
	for _, l := range b.loans {
		if l.end >= b.curSeq {
			alive = append(alive, l)
		}
	}
	b.loans = alive
}

func (b *borrowChecker) activeLoan(root string, mutableOnly bool) *loan {
	for _, l := range b.loans {
		if l.root != root {
			continue
		}
		if mutableOnly && !l.mutable {
			continue
		}
		return l
	}
	return nil
}

func (b *borrowChecker) checkBlock(blk *ast.Block) {
	b.pushScope()
	for _, s := range blk.Stmts {
		b.checkStmt(s)
	}
	// Loans whose borrower was declared in this block die with the block.
	scope := b.scopes[len(b.scopes)-1]
	alive := b.loans[:0]
	for _, l := range b.loans {
		if l.borrower != "" {
			if _, local := scope[l.borrower]; local {
				continue
			}
		}
		alive = append(alive, l)
	}
	b.loans = alive
	b.popScope()
}

func (b *borrowChecker) checkStmt(s ast.Stmt) {
	switch st := s.(type) {
	case *ast.LetStmt:
		vt := b.info.LetTypes[st]
		if vt == nil {
			return
		}
		if addr, ok := st.Value.(*ast.AddrOf); ok {
			b.at(st.Value)
			b.checkPlace(addr.X)
			b.newBorrow(addr, st.Name)
		} else {
			b.at(st.Value)
			b.checkExpr(st.Value, !IsCopy(vt))
		}
		b.declare(st.Name, &varState{typ: vt, mut: st.Mut})

	case *ast.AssignStmt:
		b.at(st.Target)
		root, ok := rootIdent(st.Target)
		if !ok {
			return
		}
		if v := b.lookup(root); v != nil {
			if !v.mut && !isMutRef(v.typ) {
				b.errf("E207", st.Target.Pos(), "cannot assign to %q: it is immutable (use `let mut %s`)", root, root)
			}
			if v.moved {
				b.errf("E200", st.Target.Pos(), "use of moved value %q", root)
			}
			if l := b.activeLoan(root, false); l != nil {
				b.errf("E206", st.Target.Pos(), "cannot assign to %q: it is borrowed", root)
			}
			v.moved = false
		}
		b.checkExpr(st.Value, true)

	case *ast.ExprStmt:
		b.at(st.X)
		b.checkExpr(st.X, false)

	case *ast.ReturnStmt:
		if st.X != nil {
			b.at(st.X)
			b.checkExpr(st.X, true)
		}

	case *ast.IfStmt:
		b.at(st.Cond)
		b.checkExpr(st.Cond, false)
		beforeMoved, beforeLoans := b.snapshot()

		b.checkBlock(st.Then)
		thenMoved, thenLoans := b.snapshot()

		b.restore(beforeMoved, beforeLoans)
		if st.Else != nil {
			b.checkStmt(st.Else)
		}
		elseMoved, elseLoans := b.snapshot()

		// Merge: a value is moved if it is moved on any path.
		merged := map[string]bool{}
		for k, v := range thenMoved {
			merged[k] = v
		}
		for k, v := range elseMoved {
			merged[k] = merged[k] || v
		}
		b.restore(merged, nil)
		b.loans = mergeLoans(thenLoans, elseLoans)

	case *ast.WhileStmt:
		b.at(st.Cond)
		b.checkExpr(st.Cond, false)
		beforeMoved, beforeLoans := b.snapshot()
		b.checkBlock(st.Body)
		bodyMoved, _ := b.snapshot()
		// The body may run zero times or many times: keep the union of
		// pre-loop loans and pre-loop moved flags.
		merged := map[string]bool{}
		for k, v := range beforeMoved {
			merged[k] = v
		}
		for k, v := range bodyMoved {
			merged[k] = merged[k] || v
		}
		b.restore(merged, beforeLoans)

	case *ast.IfLetStmt:
		b.at(st.Value)
		b.checkExpr(st.Value, true)
		bound := b.info.IfLetTypes[st]
		b.checkBlockWithName(st.Then, st.Name, bound)
		if st.Else != nil {
			b.checkStmt(st.Else)
		}

	case *ast.Block:
		b.checkBlock(st)
	}
}

// checkBlockWithName checks a block with one extra binding in scope.
func (b *borrowChecker) checkBlockWithName(blk *ast.Block, name string, t Type) {
	b.pushScope()
	b.declare(name, &varState{typ: t, mut: false})
	for _, s := range blk.Stmts {
		b.checkStmt(s)
	}
	scope := b.scopes[len(b.scopes)-1]
	alive := b.loans[:0]
	for _, l := range b.loans {
		if l.borrower != "" {
			if _, local := scope[l.borrower]; local {
				continue
			}
		}
		alive = append(alive, l)
	}
	b.loans = alive
	b.popScope()
}

func (b *borrowChecker) snapshot() (map[string]bool, []*loan) {
	moved := map[string]bool{}
	for _, sc := range b.scopes {
		for name, st := range sc {
			moved[name] = st.moved
		}
	}
	loans := make([]*loan, len(b.loans))
	copy(loans, b.loans)
	return moved, loans
}

func (b *borrowChecker) restore(moved map[string]bool, loans []*loan) {
	for _, sc := range b.scopes {
		for name, st := range sc {
			if m, ok := moved[name]; ok {
				st.moved = m
			}
		}
	}
	if loans != nil {
		b.loans = make([]*loan, len(loans))
		copy(b.loans, loans)
	}
}

func mergeLoans(a, c []*loan) []*loan {
	seen := map[string]bool{}
	var out []*loan
	for _, l := range append(append([]*loan{}, a...), c...) {
		key := l.root + "|" + l.borrower + "|" + boolKey(l.mutable)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	return out
}

func boolKey(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

func (b *borrowChecker) checkExpr(e ast.Expr, moveCtx bool) {
	if e == nil {
		return
	}
	if _, ok := b.seqOf[e]; ok {
		b.curSeq = b.seqOf[e]
		b.prune()
	}

	switch ex := e.(type) {
	case *ast.Ident:
		if moveCtx && !IsCopy(b.typeOf(ex)) {
			b.moveOut(ex.Name, ex.At)
			b.info.Moves[ex] = true
			return
		}
		b.readIdent(ex.Name, ex.At)

	case *ast.Unary:
		b.checkExpr(ex.X, false)

	case *ast.Binary:
		b.checkExpr(ex.X, false)
		b.checkExpr(ex.Y, false)

	case *ast.Field:
		b.checkExpr(ex.X, false)
		if moveCtx && !IsCopy(b.typeOf(ex)) {
			if root, ok := rootIdent(ex.X); ok {
				b.moveOut(root, ex.At)
			}
			b.info.Moves[ex] = true
		}

	case *ast.Index:
		b.checkExpr(ex.X, false)
		b.checkExpr(ex.Idx, false)

	case *ast.StructLit:
		for _, fi := range ex.Fields {
			b.checkExpr(fi.Value, true)
		}

	case *ast.AddrOf:
		b.checkPlace(ex.X)
		b.newBorrow(ex, "")

	case *ast.SliceLit:
		for _, e := range ex.Elems {
			b.checkExpr(e, true)
		}

	case *ast.TryExpr:
		b.checkExpr(ex.X, true)

	case *ast.Call:
		b.checkCall(ex)
	}
}

func (b *borrowChecker) checkCall(c *ast.Call) {
	if id, ok := c.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "Some", "Ok", "Err":
			for _, a := range c.Args {
				b.checkExpr(a, true)
			}
			return
		case "push":
			if len(c.Args) == 2 {
				if addr, ok := c.Args[0].(*ast.AddrOf); ok {
					b.checkPlace(addr.X)
					b.borrowPlace(addr.X, true, addr.At, "")
				} else {
					b.checkExpr(c.Args[0], false)
				}
				b.checkExpr(c.Args[1], true)
			}
			return
		}
	}
	if field, ok := c.Fn.(*ast.Field); ok && b.info.MethodSel[c.Fn] {
		b.checkMethodCall(c, field)
		return
	}
	if id, ok := c.Fn.(*ast.Ident); ok {
		switch id.Name {
		case "println", "print", "len":
			for _, a := range c.Args {
				b.checkExpr(a, false)
			}
			return
		}
		sig, ok := b.info.Funcs[id.Name]
		if !ok {
			return
		}
		for i, a := range c.Args {
			if i >= len(sig.Params) {
				b.checkExpr(a, false)
				continue
			}
			b.checkExpr(a, !IsCopy(sig.Params[i]))
		}
		return
	}
	for _, a := range c.Args {
		b.checkExpr(a, false)
	}
}

// checkPlace validates a place expression used as a borrow target.
// checkMethodCall applies receiver rules: &T borrows, &mut T borrows mutably,
// a by-value receiver moves the value.
func (b *borrowChecker) checkMethodCall(c *ast.Call, field *ast.Field) {
	sig := b.methodSig(field)
	if sig == nil || len(sig.Params) == 0 {
		for _, a := range c.Args {
			b.checkExpr(a, false)
		}
		return
	}
	recvType := sig.Params[0]
	if p, isPtr := recvType.(*Ptr); isPtr {
		b.borrowPlace(field.X, p.Mut, field.X.Pos(), "")
	} else if !IsCopy(recvType) {
		if root, ok := rootIdent(field.X); ok {
			b.moveOut(root, field.X.Pos())
		}
	}
	for i, a := range c.Args {
		if i+1 < len(sig.Params) {
			b.checkExpr(a, !IsCopy(sig.Params[i+1]))
		} else {
			b.checkExpr(a, false)
		}
	}
}

func (b *borrowChecker) methodSig(field *ast.Field) *FuncSig {
	t := b.info.ExprTypes[field.X]
	base := t
	if p, ok := base.(*Ptr); ok {
		base = p.Elem
	}
	st, ok := base.(*StructT)
	if !ok {
		return nil
	}
	return b.info.Methods[st.Name][field.Name]
}

func (b *borrowChecker) checkPlace(e ast.Expr) {
	root, ok := rootIdent(e)
	if !ok {
		return
	}
	if st := b.lookup(root); st != nil && st.moved {
		b.errf("E200", e.Pos(), "cannot borrow moved value %q", root)
	}
}

func (b *borrowChecker) typeOf(e ast.Expr) Type {
	if t, ok := b.info.ExprTypes[e]; ok {
		return t
	}
	return nil
}

func (b *borrowChecker) readIdent(name string, pos token.Pos) {
	st := b.lookup(name)
	if st == nil {
		return
	}
	if st.moved {
		b.errf("E200", pos, "use of moved value %q", name)
		return
	}
	if l := b.activeLoan(name, true); l != nil {
		b.errf("E202", pos, "cannot use %q while it is mutably borrowed", name)
	}
}

func (b *borrowChecker) moveOut(name string, pos token.Pos) {
	st := b.lookup(name)
	if st == nil {
		return
	}
	if st.moved {
		b.errf("E200", pos, "use of moved value %q", name)
		return
	}
	if l := b.activeLoan(name, false); l != nil {
		b.errf("E203", pos, "cannot move %q while it is borrowed", name)
		return
	}
	st.moved = true
}

func (b *borrowChecker) newBorrow(addr *ast.AddrOf, borrower string) {
	b.borrowPlace(addr.X, addr.Mut, addr.At, borrower)
}

// borrowPlace registers a borrow of the place e and validates it against
// existing loans.
func (b *borrowChecker) borrowPlace(e ast.Expr, mut bool, pos token.Pos, borrower string) {
	root, ok := rootIdent(e)
	if !ok {
		return
	}
	st := b.lookup(root)
	if st == nil {
		return
	}
	if st.moved {
		b.errf("E200", pos, "cannot borrow moved value %q", root)
		return
	}
	if mut {
		if l := b.activeLoan(root, false); l != nil {
			b.errf("E204", pos, "cannot borrow %q as mutable: it is already borrowed", root)
			return
		}
	} else {
		if l := b.activeLoan(root, true); l != nil {
			b.errf("E205", pos, "cannot borrow %q as immutable: it is already mutably borrowed", root)
			return
		}
	}

	end := b.curSeq
	if borrower != "" {
		end = b.maxSeq
		if u, ok := b.lastUse[borrower]; ok && u > 0 {
			end = u
		}
	}
	b.loans = append(b.loans, &loan{root: root, mutable: mut, borrower: borrower, end: end})
}

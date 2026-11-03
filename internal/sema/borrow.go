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
	typ     Type
	mut     bool
	moved   bool
	movedAt token.Pos
}

type loan struct {
	root     string
	mutable  bool
	borrower string
	end      int
	pos      token.Pos
}

type borrowChecker struct {
	info *Info
	errs []*Error

	scopes  []map[string]*varState
	loans   []*loan
	seqOf   map[ast.Expr]int
	lastUse map[string]int

	curSeq     int
	maxSeq     int
	callEnd    int
	endOf      map[ast.Expr]int
	loopEnd    map[*ast.WhileStmt]int
	loopOwners []map[*varState]bool
}

func (b *borrowChecker) errf(code string, pos token.Pos, format string, args ...any) {
	e := &Error{Code: code, Pos: pos, Msg: fmt.Sprintf(format, args...)}
	if len(args) > 0 {
		if name, ok := args[0].(string); ok {
			if code == "E200" {
				if st := b.lookup(name); st != nil && st.movedAt.Line > 0 {
					e.Notes = append(e.Notes, Note{st.movedAt, "value moved here"})
				}
				e.Hint = "Borrow the value with & instead of transferring ownership, or create a new value."
			} else if code == "E202" || code == "E203" || code == "E204" || code == "E205" || code == "E206" {
				if l := b.activeLoan(name, code == "E202" || code == "E205"); l != nil {
					e.Notes = append(e.Notes, Note{l.pos, "borrow starts here"})
				}
				e.Hint = "Finish using the existing reference before this operation."
			} else if code == "E207" {
				e.Hint = "Declare the binding with let mut if it needs to change."
			}
		}
	}
	b.errs = append(b.errs, e)
}

// --- numbering pass ---

func (b *borrowChecker) numberFunc(fn *ast.FuncDecl) {
	b.seqOf = map[ast.Expr]int{}
	b.endOf = map[ast.Expr]int{}
	b.loopEnd = map[*ast.WhileStmt]int{}
	b.callEnd = 0
	b.loopOwners = nil
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
		b.loopEnd[st] = b.curSeq
	case *ast.MatchStmt:
		b.numberExpr(st.Value)
		for _, arm := range st.Arms {
			b.numberBlock(arm.Body)
		}

	case *ast.Block:
		b.numberBlock(st)
	}
}

func (b *borrowChecker) numberExpr(e ast.Expr) {
	if e == nil {
		return
	}
	defer func() { b.endOf[e] = b.curSeq }()
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
		if _, method := ex.Fn.(*ast.Field); method {
			b.numberExpr(ex.Fn)
		}
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
	var sig *FuncSig
	offset := 0
	if fn.Recv != nil {
		receiver := fn.Recv.Type
		if ptr, ok := receiver.(*ast.PtrType); ok {
			receiver = ptr.Elem
		}
		if named, ok := receiver.(*ast.NamedType); ok {
			sig = b.info.Methods[named.Name][fn.Name]
		}
		offset = 1
	} else {
		sig = b.info.Funcs[fn.Name]
	}
	if sig == nil {
		return
	}
	b.numberFunc(fn)

	b.scopes = nil
	b.loans = nil
	b.pushScope()
	if fn.Recv != nil {
		b.declare(fn.Recv.Name, &varState{typ: sig.Params[0]})
	}
	for i, p := range fn.Params {
		b.declare(p.Name, &varState{typ: sig.Params[i+offset], mut: false})
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
			if ptr, ok := vt.(*Ptr); ok {
				if id, ok := st.Value.(*ast.Ident); ok {
					if ptr.Mut {
						b.errf("E209", st.At, "mutable reference %q cannot be copied", id.Name)
					} else {
						var aliases []*loan
						for _, l := range b.loans {
							if l.borrower == id.Name {
								copy := *l
								copy.borrower = st.Name
								copy.end = b.lastUse[st.Name]
								aliases = append(aliases, &copy)
							}
						}
						b.loans = append(b.loans, aliases...)
					}
				}
			}
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
		b.checkExpr(st.Target, false)
		b.checkExpr(st.Value, true)
		if v := b.lookup(root); v != nil {
			v.moved = false
		}

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
		merged := map[string]moveState{}
		for k, v := range thenMoved {
			merged[k] = v
		}
		for k, v := range elseMoved {
			if v.moved {
				merged[k] = v
			}
		}
		b.restore(merged, nil)
		b.loans = mergeLoans(thenLoans, elseLoans)

	case *ast.WhileStmt:
		owners := map[*varState]bool{}
		for _, sc := range b.scopes {
			for _, v := range sc {
				owners[v] = true
			}
		}
		b.loopOwners = append(b.loopOwners, owners)
		defer func() { b.loopOwners = b.loopOwners[:len(b.loopOwners)-1] }()
		for _, l := range b.loans {
			if l.end >= b.seqOf[st.Cond] && l.end < b.loopEnd[st] {
				l.end = b.loopEnd[st]
			}
		}
		b.at(st.Cond)
		b.checkExpr(st.Cond, false)
		beforeMoved, beforeLoans := b.snapshot()
		b.checkBlock(st.Body)
		bodyMoved, _ := b.snapshot()
		// The body may run zero times or many times: keep the union of
		// pre-loop loans and pre-loop moved flags.
		merged := map[string]moveState{}
		for k, v := range beforeMoved {
			merged[k] = v
		}
		for k, v := range bodyMoved {
			if v.moved {
				merged[k] = v
			}
		}
		b.restore(merged, beforeLoans)

	case *ast.IfLetStmt:
		b.at(st.Value)
		b.checkExpr(st.Value, true)
		before, loans := b.snapshot()
		bound := b.info.IfLetTypes[st]
		b.checkBlockWithName(st.Then, st.Name, bound)
		then, thenLoans := b.snapshot()
		b.restore(before, loans)
		if st.Else != nil {
			b.checkStmt(st.Else)
		}
		other, otherLoans := b.snapshot()
		for name, state := range then {
			if state.moved {
				other[name] = state
			}
		}
		b.restore(other, mergeLoans(thenLoans, otherLoans))

	case *ast.MatchStmt:
		b.at(st.Value)
		b.checkExpr(st.Value, true)
		before, loans := b.snapshot()
		merged := map[string]moveState{}
		var mergedLoans []*loan
		for _, arm := range st.Arms {
			b.restore(before, loans)
			if bound := b.info.MatchTypes[arm]; bound != nil {
				b.checkBlockWithName(arm.Body, arm.Name, bound)
			} else {
				b.checkBlock(arm.Body)
			}
			moved, ls := b.snapshot()
			for name, state := range moved {
				if old, ok := merged[name]; !ok || !old.moved {
					merged[name] = state
				}
			}
			mergedLoans = mergeLoans(mergedLoans, ls)
		}
		b.restore(merged, mergedLoans)

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

type moveState struct {
	moved bool
	pos   token.Pos
}

func (b *borrowChecker) snapshot() (map[string]moveState, []*loan) {
	moved := map[string]moveState{}
	for _, sc := range b.scopes {
		for name, st := range sc {
			moved[name] = moveState{st.moved, st.movedAt}
		}
	}
	loans := make([]*loan, len(b.loans))
	copy(loans, b.loans)
	return moved, loans
}

func (b *borrowChecker) restore(moved map[string]moveState, loans []*loan) {
	for _, sc := range b.scopes {
		for name, st := range sc {
			if m, ok := moved[name]; ok {
				st.moved = m.moved
				st.movedAt = m.pos
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
		saved := b.callEnd
		b.callEnd = b.endOf[ex]
		b.checkExpr(ex.X, false)
		b.borrowRead(ex.X)
		b.checkExpr(ex.Y, false)
		b.borrowRead(ex.Y)
		b.curSeq = b.endOf[ex] + 1
		b.prune()
		b.callEnd = saved

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
		if moveCtx && !IsCopy(b.typeOf(ex)) {
			if root, ok := rootIdent(ex.X); ok {
				b.moveOut(root, ex.At)
			}
			b.info.Moves[ex] = true
		}

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
	// A reference argument remains borrowed throughout the whole call, even
	// when its final textual occurrence is the first argument.
	for _, a := range c.Args {
		b.holdReference(a, b.endOf[c])
	}
	if field, ok := c.Fn.(*ast.Field); ok {
		b.holdReference(field.X, b.endOf[c])
	}

	before := b.callEnd
	b.callEnd = b.endOf[c]
	defer func() { b.curSeq = b.endOf[c] + 1; b.prune(); b.callEnd = before }()
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
					b.borrowPlace(c.Args[0], true, c.Args[0].Pos(), "")
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
				b.borrowRead(a)
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
			b.checkArgument(a, sig.Params[i])
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
		b.checkExpr(field.X, false)
		b.borrowPlace(field.X, p.Mut, field.X.Pos(), "")
	} else {
		b.checkExpr(field.X, !IsCopy(recvType))
	}
	for i, a := range c.Args {
		if i+1 < len(sig.Params) {
			b.checkArgument(a, sig.Params[i+1])
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
	if _, borrowed := st.typ.(*Ptr); borrowed {
		b.errf("E208", pos, "cannot move out of borrowed value %q", name)
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
	for _, owners := range b.loopOwners {
		if owners[st] {
			b.errf("E212", pos, "cannot move %q declared outside a repeating loop", name)
			return
		}
	}
	st.moved = true
	st.movedAt = pos
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
	if mut && !st.mut && !isMutRef(st.typ) {
		b.errf("E124", pos, "cannot borrow immutable %q as mutable", root)
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
	if b.callEnd > end {
		end = b.callEnd
	}
	if borrower != "" {
		end = b.maxSeq
		if u, ok := b.lastUse[borrower]; ok && u > 0 {
			end = u
		}
	}
	b.loans = append(b.loans, &loan{root: root, mutable: mut, borrower: borrower, end: end, pos: pos})
}

func (b *borrowChecker) checkArgument(a ast.Expr, t Type) {
	b.checkExpr(a, !IsCopy(t))
	if p, ref := t.(*Ptr); ref {
		if _, addr := a.(*ast.AddrOf); !addr {
			b.borrowPlace(a, p.Mut, a.Pos(), "")
		}
	}
}

// Builtins and string operations borrow owning places until they finish; a
// later operand must not free a string that an earlier operand still reads.
func (b *borrowChecker) borrowRead(e ast.Expr) {
	b.holdReference(e, b.callEnd)
	if t := b.typeOf(e); t != nil && !IsCopy(t) {
		b.borrowPlace(e, false, e.Pos(), "")
	}
}

func (b *borrowChecker) holdReference(e ast.Expr, end int) {
	root, ok := rootIdent(e)
	if !ok {
		return
	}
	st := b.lookup(root)
	if st == nil {
		return
	}
	if _, ref := st.typ.(*Ptr); !ref {
		return
	}
	for _, l := range b.loans {
		if l.borrower == root && l.end < end {
			l.end = end
		}
	}
}

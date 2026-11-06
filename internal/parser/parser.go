// Package parser builds a GoRs syntax tree with recursive descent.
package parser

import (
	"fmt"
	"strconv"
	"strings"

	"gors/internal/ast"
	"gors/internal/lexer"
	"gors/internal/token"
)

// Error is a parse error with a source position.
type Error struct {
	Pos token.Pos
	Msg string
}

func (e *Error) Error() string {
	return fmt.Sprintf("%d:%d: error: %s", e.Pos.Line, e.Pos.Col, e.Msg)
}

// Parser holds parser state.
type Parser struct {
	toks []token.Token
	pos  int
	errs []*Error

	noStructLit int // >0 while parsing if/while conditions
}

// Parse parses source code. It always returns a (possibly partial) program
// together with all collected errors.
func Parse(src string) (*ast.Program, []*Error) {
	return ParseFile(src, "")
}

func ParseFile(src, file string) (*ast.Program, []*Error) {
	toks := lexer.New(src).Tokens()
	for i := range toks {
		toks[i].Pos.File = file
	}
	p := &Parser{toks: toks}
	for _, t := range toks {
		reserved := strings.HasPrefix(t.Lit, "__gors_")
		if strings.HasPrefix(t.Lit, "_t") {
			if _, err := strconv.Atoi(strings.TrimPrefix(t.Lit, "_t")); err == nil {
				reserved = true
			}
		}
		if t.Type == token.IDENT && reserved {
			p.errorf(t.Pos, "identifier %q is reserved by the compiler", t.Lit)
		}
	}
	prog := p.parseProgram()
	return prog, p.errs
}

func (p *Parser) cur() token.Token { return p.toks[p.pos] }
func (p *Parser) peek() token.Token {
	if p.pos+1 < len(p.toks) {
		return p.toks[p.pos+1]
	}
	return p.toks[len(p.toks)-1]
}

func (p *Parser) advance() token.Token {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}

func (p *Parser) check(t token.Type) bool { return p.cur().Type == t }

func (p *Parser) accept(t token.Type) bool {
	if p.check(t) {
		p.advance()
		return true
	}
	return false
}

func (p *Parser) expect(t token.Type) token.Token {
	if p.check(t) {
		return p.advance()
	}
	p.errorf(p.cur().Pos, "expected %q, got %q", t, p.cur().Type)
	return token.Token{Type: t, Pos: p.cur().Pos}
}

func (p *Parser) errorf(pos token.Pos, format string, args ...any) {
	p.errs = append(p.errs, &Error{Pos: pos, Msg: fmt.Sprintf(format, args...)})
}

func (p *Parser) sync() {
	for !p.check(token.EOF) {
		if p.check(token.FN) || p.check(token.STRUCT) || p.check(token.RBRACE) {
			return
		}
		p.advance()
	}
}

func (p *Parser) parseProgram() *ast.Program {
	prog := &ast.Program{}
	for !p.check(token.EOF) {
		before := p.pos
		public := p.accept(token.PUB)
		switch p.cur().Type {
		case token.IMPORT:
			start := p.advance()
			alias := p.expect(token.IDENT)
			path := p.expect(token.STRING)
			prog.Imports = append(prog.Imports, ast.Import{Alias: alias.Lit, Path: path.Lit, At: start.Pos})
			if public {
				p.errorf(start.Pos, "an import cannot be public")
			}
		case token.STRUCT:
			if d := p.parseStructDecl(); d != nil {
				d.Public = public
				prog.Structs = append(prog.Structs, d)
			}
		case token.FN:
			if d := p.parseFuncDecl(); d != nil {
				d.Public = public
				prog.Funcs = append(prog.Funcs, d)
			}
		case token.SEMI:
			p.advance()
		default:
			p.errorf(p.cur().Pos, "unexpected token %q at top level", p.cur().Type)
			p.sync()
		}
		if p.pos == before {
			p.advance()
		}
	}
	return prog
}

func (p *Parser) parseStructDecl() *ast.StructDecl {
	start := p.expect(token.STRUCT)
	name := p.expect(token.IDENT)
	p.expect(token.LBRACE)
	if reservedGlobal(name.Lit) {
		p.errorf(name.Pos, "reserved declaration name %q", name.Lit)
	}
	decl := &ast.StructDecl{Name: name.Lit, At: start.Pos}
	for !p.check(token.RBRACE) && !p.check(token.EOF) {
		fname := p.expect(token.IDENT)
		p.expect(token.COLON)
		ftype := p.parseType()
		decl.Fields = append(decl.Fields, ast.StructField{Name: fname.Lit, Type: ftype, At: fname.Pos})
		if !p.accept(token.COMMA) {
			break
		}
	}
	p.expect(token.RBRACE)
	return decl
}

func (p *Parser) parseFuncDecl() *ast.FuncDecl {
	start := p.expect(token.FN)
	decl := &ast.FuncDecl{At: start.Pos}

	// Optional method receiver: fn (p: &Point) dist2() -> int
	if p.check(token.LPAREN) {
		lp := p.advance()
		rname := p.expect(token.IDENT)
		p.expect(token.COLON)
		rtype := p.parseType()
		p.expect(token.RPAREN)
		decl.Recv = &ast.Param{Name: rname.Lit, Type: rtype, At: lp.Pos}
	}

	name := p.expect(token.IDENT)
	p.expect(token.LPAREN)
	if name.Lit == "_" || (decl.Recv == nil && reservedGlobal(name.Lit)) {
		p.errorf(name.Pos, "reserved declaration name %q", name.Lit)
	}
	decl.Name = name.Lit
	for !p.check(token.RPAREN) && !p.check(token.EOF) {
		pname := p.expect(token.IDENT)
		p.expect(token.COLON)
		ptype := p.parseType()
		decl.Params = append(decl.Params, ast.Param{Name: pname.Lit, Type: ptype, At: pname.Pos})
		if !p.accept(token.COMMA) {
			break
		}
	}
	p.expect(token.RPAREN)
	if p.accept(token.ARROW) {
		decl.Ret = p.parseType()
	}
	decl.Body = p.parseBlock()
	return decl
}

func (p *Parser) parseType() ast.TypeExpr {
	t := p.cur()
	switch {
	case p.accept(token.AMP):
		mut := p.accept(token.MUT)
		elem := p.parseType()
		return &ast.PtrType{Elem: elem, Mut: mut, At: t.Pos}
	case p.accept(token.LBRACK):
		p.expect(token.RBRACK)
		elem := p.parseType()
		return &ast.SliceType{Elem: elem, At: t.Pos}
	case p.check(token.IDENT):
		p.advance()
		name := t.Lit
		if p.accept(token.PATH) {
			name += "::" + p.expect(token.IDENT).Lit
		}
		nt := &ast.NamedType{Name: name, At: t.Pos}
		if p.check(token.LT) {
			p.advance()
			for {
				nt.Args = append(nt.Args, p.parseType())
				if !p.accept(token.COMMA) {
					break
				}
			}
			p.expect(token.GT)
		}
		return nt
	default:
		p.errorf(t.Pos, "expected a type, got %q", t.Type)
		p.advance()
		return &ast.NamedType{Name: "int", At: t.Pos}
	}
}

func (p *Parser) parseBlock() *ast.Block {
	start := p.expect(token.LBRACE)
	b := &ast.Block{At: start.Pos}
	for !p.check(token.RBRACE) && !p.check(token.EOF) {
		if p.accept(token.SEMI) {
			continue
		}
		before := p.pos
		s := p.parseStmt()
		if p.pos == before {
			p.advance()
		}
		if s != nil {
			b.Stmts = append(b.Stmts, s)
		}
	}
	p.expect(token.RBRACE)
	return b
}

func (p *Parser) parseStmt() ast.Stmt {
	switch p.cur().Type {
	case token.MATCH:
		return p.parseMatch()
	case token.LET:
		return p.parseLet()
	case token.IF:
		return p.parseIf()
	case token.WHILE:
		return p.parseWhile()
	case token.FOR:
		return p.parseFor()
	case token.BREAK:
		start := p.advance()
		return &ast.BreakStmt{At: start.Pos}
	case token.CONTINUE:
		start := p.advance()
		return &ast.ContinueStmt{At: start.Pos}
	case token.RETURN:
		return p.parseReturn()
	case token.LBRACE:
		return p.parseBlock()
	default:
		return p.parseSimpleStmt()
	}
}

func (p *Parser) parseLet() ast.Stmt {
	start := p.expect(token.LET)
	mut := p.accept(token.MUT)
	name := p.expect(token.IDENT)
	var typ ast.TypeExpr
	if p.accept(token.COLON) {
		typ = p.parseType()
	}
	p.expect(token.ASSIGN)
	val := p.parseExpr()
	return &ast.LetStmt{Name: name.Lit, Mut: mut, Type: typ, Value: val, At: start.Pos}
}

func (p *Parser) parseIf() ast.Stmt {
	start := p.expect(token.IF)
	if p.accept(token.LET) {
		kind := p.expect(token.IDENT)
		p.expect(token.LPAREN)
		name := p.expect(token.IDENT)
		p.expect(token.RPAREN)
		p.expect(token.ASSIGN)
		p.noStructLit++
		val := p.parseExpr()
		p.noStructLit--
		then := p.parseBlock()
		stmt := &ast.IfLetStmt{Kind: kind.Lit, Name: name.Lit, Value: val, Then: then, At: start.Pos}
		if p.accept(token.ELSE) {
			if p.check(token.IF) {
				stmt.Else = p.parseIf()
			} else {
				stmt.Else = p.parseBlock()
			}
		}
		return stmt
	}
	p.noStructLit++
	cond := p.parseExpr()
	p.noStructLit--
	then := p.parseBlock()
	stmt := &ast.IfStmt{Cond: cond, Then: then, At: start.Pos}
	if p.accept(token.ELSE) {
		if p.check(token.IF) {
			stmt.Else = p.parseIf()
		} else {
			stmt.Else = p.parseBlock()
		}
	}
	return stmt
}

func (p *Parser) parseWhile() ast.Stmt {
	start := p.expect(token.WHILE)
	p.noStructLit++
	cond := p.parseExpr()
	p.noStructLit--
	body := p.parseBlock()
	return &ast.WhileStmt{Cond: cond, Body: body, At: start.Pos}
}

func (p *Parser) parseFor() ast.Stmt {
	start := p.expect(token.FOR)
	first := p.expect(token.IDENT)
	var key, val string
	if p.accept(token.COMMA) {
		second := p.expect(token.IDENT)
		key = first.Lit
		val = second.Lit
	} else {
		val = first.Lit
	}
	p.expect(token.IN)
	p.noStructLit++
	iter := p.parseExpr()
	p.noStructLit--
	body := p.parseBlock()
	return &ast.ForStmt{
		Key:   key,
		Value: val,
		Iter:  iter,
		Body:  body,
		At:    start.Pos,
	}
}

func (p *Parser) parseReturn() ast.Stmt {
	start := p.expect(token.RETURN)
	// A return without a value either ends the block or sits on its own line.
	if p.check(token.RBRACE) || p.check(token.SEMI) || p.check(token.EOF) || p.cur().Pos.Line != start.Pos.Line {
		return &ast.ReturnStmt{At: start.Pos}
	}
	return &ast.ReturnStmt{X: p.parseExpr(), At: start.Pos}
}

func (p *Parser) parseSimpleStmt() ast.Stmt {
	start := p.cur().Pos
	x := p.parseExpr()
	if p.accept(token.ASSIGN) {
		val := p.parseExpr()
		return &ast.AssignStmt{Target: x, Value: val, At: start}
	}
	return &ast.ExprStmt{X: x, At: start}
}

// --- expressions ---

func (p *Parser) parseExpr() ast.Expr { return p.parseOr() }

func (p *Parser) parseOr() ast.Expr {
	x := p.parseAnd()
	for p.check(token.OR) {
		op := p.advance()
		y := p.parseAnd()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseAnd() ast.Expr {
	x := p.parseEq()
	for p.check(token.AND) {
		op := p.advance()
		y := p.parseEq()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseEq() ast.Expr {
	x := p.parseCmp()
	for p.check(token.EQ) || p.check(token.NE) {
		op := p.advance()
		y := p.parseCmp()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseCmp() ast.Expr {
	x := p.parseAdd()
	for p.check(token.LT) || p.check(token.GT) || p.check(token.LE) || p.check(token.GE) {
		op := p.advance()
		y := p.parseAdd()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseAdd() ast.Expr {
	x := p.parseMul()
	for p.check(token.PLUS) || p.check(token.MINUS) {
		op := p.advance()
		y := p.parseMul()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseMul() ast.Expr {
	x := p.parseUnary()
	for p.check(token.STAR) || p.check(token.SLASH) || p.check(token.PERCENT) {
		op := p.advance()
		y := p.parseUnary()
		x = &ast.Binary{Op: op.Lit, X: x, Y: y, At: op.Pos}
	}
	return x
}

func (p *Parser) parseUnary() ast.Expr {
	t := p.cur()
	switch {
	case p.accept(token.MINUS):
		return &ast.Unary{Op: "-", X: p.parseUnary(), At: t.Pos}
	case p.accept(token.NOT):
		return &ast.Unary{Op: "!", X: p.parseUnary(), At: t.Pos}
	case p.accept(token.STAR):
		return &ast.Unary{Op: "*", X: p.parseUnary(), At: t.Pos}
	case p.accept(token.AMP):
		mut := p.accept(token.MUT)
		return &ast.AddrOf{X: p.parseUnary(), Mut: mut, At: t.Pos}
	default:
		return p.parsePostfix()
	}
}

func (p *Parser) parsePostfix() ast.Expr {
	x := p.parsePrimary()
	for {
		switch {
		case p.check(token.LPAREN):
			start := p.advance()
			var args []ast.Expr
			for !p.check(token.RPAREN) && !p.check(token.EOF) {
				args = append(args, p.parseExpr())
				if !p.accept(token.COMMA) {
					break
				}
			}
			p.expect(token.RPAREN)
			x = &ast.Call{Fn: x, Args: args, At: start.Pos}
		case p.check(token.DOT):
			dot := p.advance()
			name := p.expect(token.IDENT)
			x = &ast.Field{X: x, Name: name.Lit, At: dot.Pos}
		case p.check(token.LBRACK):
			start := p.advance()
			var low, high ast.Expr
			if p.accept(token.COLON) {
				if !p.check(token.RBRACK) {
					high = p.parseExpr()
				}
				p.expect(token.RBRACK)
				x = &ast.SliceExpr{X: x, Low: low, High: high, At: start.Pos}
			} else {
				first := p.parseExpr()
				if p.accept(token.COLON) {
					low = first
					if !p.check(token.RBRACK) {
						high = p.parseExpr()
					}
					p.expect(token.RBRACK)
					x = &ast.SliceExpr{X: x, Low: low, High: high, At: start.Pos}
				} else {
					p.expect(token.RBRACK)
					x = &ast.Index{X: x, Idx: first, At: start.Pos}
				}
			}
		case p.check(token.QUESTION):
			q := p.advance()
			x = &ast.TryExpr{X: x, At: q.Pos}
		default:
			return x
		}
	}
}

func (p *Parser) parsePrimary() ast.Expr {
	t := p.cur()
	switch t.Type {
	case token.INT:
		p.advance()
		v, err := strconv.ParseInt(t.Lit, 10, 64)
		if err != nil {
			p.errorf(t.Pos, "invalid integer literal %q", t.Lit)
		}
		return &ast.IntLit{Value: v, At: t.Pos}
	case token.FLOAT:
		p.advance()
		v, err := strconv.ParseFloat(t.Lit, 64)
		if err != nil {
			p.errorf(t.Pos, "invalid float literal %q", t.Lit)
		}
		return &ast.FloatLit{Value: v, At: t.Pos}
	case token.STRING:
		p.advance()
		return &ast.StringLit{Value: t.Lit, At: t.Pos}
	case token.TRUE:
		p.advance()
		return &ast.BoolLit{Value: true, At: t.Pos}
	case token.FALSE:
		p.advance()
		return &ast.BoolLit{Value: false, At: t.Pos}
	case token.IDENT:
		p.advance()
		if p.accept(token.PATH) {
			t.Lit += "::" + p.expect(token.IDENT).Lit
		}
		if p.noStructLit == 0 && p.check(token.LBRACE) {
			return p.parseStructLit(t)
		}
		return &ast.Ident{Name: t.Lit, At: t.Pos}
	case token.LBRACK:
		start := p.advance()
		lit := &ast.SliceLit{At: start.Pos}
		for !p.check(token.RBRACK) && !p.check(token.EOF) {
			lit.Elems = append(lit.Elems, p.parseExpr())
			if !p.accept(token.COMMA) {
				break
			}
		}
		p.expect(token.RBRACK)
		return lit
	case token.LPAREN:
		p.advance()
		x := p.parseExpr()
		p.expect(token.RPAREN)
		return x
	case token.MATCH:
		return p.parseMatchExpr()
	default:
		p.errorf(t.Pos, "unexpected token %q in expression", t.Type)
		p.advance()
		return &ast.IntLit{Value: 0, At: t.Pos}
	}
}

func (p *Parser) parseStructLit(name token.Token) ast.Expr {
	p.expect(token.LBRACE)
	lit := &ast.StructLit{Type: name.Lit, At: name.Pos}
	for !p.check(token.RBRACE) && !p.check(token.EOF) {
		fname := p.expect(token.IDENT)
		p.expect(token.COLON)
		val := p.parseExpr()
		lit.Fields = append(lit.Fields, ast.FieldInit{Name: fname.Lit, Value: val, At: fname.Pos})
		if !p.accept(token.COMMA) {
			break
		}
	}
	p.expect(token.RBRACE)
	return lit
}

func (p *Parser) parseMatchArms() []*ast.MatchArm {
	p.expect(token.LBRACE)
	var arms []*ast.MatchArm
	for !p.check(token.RBRACE) && !p.check(token.EOF) {
		before := p.pos
		kind := p.expect(token.IDENT)
		arm := &ast.MatchArm{Kind: kind.Lit, At: kind.Pos}
		if p.accept(token.LPAREN) {
			arm.Name = p.expect(token.IDENT).Lit
			p.expect(token.RPAREN)
		}
		p.expect(token.FATARROW)
		if p.check(token.LBRACE) {
			arm.Body = p.parseBlock()
		} else {
			arm.Expr = p.parseExpr()
		}
		arms = append(arms, arm)
		p.accept(token.COMMA)
		if before == p.pos {
			p.advance()
		}
	}
	p.expect(token.RBRACE)
	return arms
}

func (p *Parser) parseMatch() ast.Stmt {
	start := p.expect(token.MATCH)
	p.noStructLit++
	value := p.parseExpr()
	p.noStructLit--
	arms := p.parseMatchArms()
	for _, arm := range arms {
		if arm.Body == nil && arm.Expr != nil {
			arm.Body = &ast.Block{
				Stmts: []ast.Stmt{&ast.ExprStmt{X: arm.Expr, At: arm.Expr.Pos()}},
				At:    arm.Expr.Pos(),
			}
			arm.Expr = nil
		}
	}
	return &ast.MatchStmt{Value: value, Arms: arms, At: start.Pos}
}

func (p *Parser) parseMatchExpr() ast.Expr {
	start := p.expect(token.MATCH)
	p.noStructLit++
	value := p.parseExpr()
	p.noStructLit--
	arms := p.parseMatchArms()
	for _, arm := range arms {
		if arm.Body != nil && arm.Expr == nil && len(arm.Body.Stmts) > 0 {
			if es, ok := arm.Body.Stmts[len(arm.Body.Stmts)-1].(*ast.ExprStmt); ok {
				arm.Expr = es.X
				arm.Body.Stmts = arm.Body.Stmts[:len(arm.Body.Stmts)-1]
			}
		}
	}
	return &ast.MatchExpr{Value: value, Arms: arms, At: start.Pos}
}

func reservedGlobal(name string) bool {
	switch name {
	case "_", "int", "float", "string", "bool", "Option", "Result", "Some", "None", "Ok", "Err", "print", "println", "len", "push":
		return true
	}
	return false
}

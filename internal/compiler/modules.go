package compiler

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gors/internal/ast"
	"gors/internal/parser"
	"gors/internal/sema"
	"gors/internal/token"
)

// SourceError retains the source of the module in which a diagnostic occurred.
type SourceError struct {
	Path, Source string
	Err          error
}

func (e *SourceError) Error() string                             { return e.Path + ":" + e.Err.Error() }
func (e *SourceError) Unwrap() error                             { return e.Err }
func (e *SourceError) SourceDiagnostic() (string, string, error) { return e.Path, e.Source, e.Err }

type module struct {
	path, source, prefix string
	prog                 *ast.Program
	imports              map[string]*module
	names                map[string]bool
	public               map[string]bool
}
type loader struct {
	modules  map[string]*module
	visiting map[string]bool
	order    []*module
}

// CompileFile links explicit local imports and compiles the entry point.
// Imports are relative to the importing file, never downloaded or searched on PATH.
func CompileFile(path string, backend Backend) (string, []error) {
	l := &loader{modules: map[string]*module{}, visiting: map[string]bool{}}
	entry, err := l.load(path, true)
	if err != nil {
		return "", []error{err}
	}
	found := false
	for _, fn := range entry.prog.Funcs {
		if fn.Recv == nil && fn.Name == "main" {
			found = true
		}
	}
	if !found {
		return "", []error{&SourceError{entry.path, entry.source, &parser.Error{Pos: token.Pos{File: entry.path, Line: 1, Col: 1}, Msg: "entry module must declare fn main()"}}}
	}
	linked := &ast.Program{}
	for _, m := range l.order {
		if err := m.resolve(); err != nil {
			return "", []error{&SourceError{m.path, m.source, err}}
		}
		linked.Structs = append(linked.Structs, m.prog.Structs...)
		linked.Funcs = append(linked.Funcs, m.prog.Funcs...)
	}
	out, errs := compileProgram(linked, backend)
	for i, err := range errs {
		var pos token.Pos
		switch e := err.(type) {
		case *sema.Error:
			pos = e.Pos
		case *parser.Error:
			pos = e.Pos
		}
		if m := l.modules[pos.File]; m != nil {
			errs[i] = &SourceError{m.path, m.source, err}
		}
	}
	return out, errs
}
func (l *loader) load(path string, entry bool) (*module, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return nil, fmt.Errorf("load module: %w", err)
	}
	if l.visiting[path] {
		return nil, fmt.Errorf("cyclic import involving %s", path)
	}
	if m := l.modules[path]; m != nil {
		return m, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	prog, errs := parser.ParseFile(string(data), path)
	if len(errs) > 0 {
		return nil, &SourceError{path, string(data), errs[0]}
	}
	prefix := fmt.Sprintf("__gors_m%d_", len(l.modules))
	if entry {
		prefix = ""
	}
	m := &module{path: path, source: string(data), prefix: prefix, prog: prog, imports: map[string]*module{}, names: map[string]bool{}, public: map[string]bool{}}
	for _, d := range prog.Structs {
		m.names[d.Name] = true
		m.public[d.Name] = d.Public
	}
	for _, d := range prog.Funcs {
		if d.Recv == nil {
			m.names[d.Name] = true
			m.public[d.Name] = d.Public
		}
	}
	l.modules[path] = m
	l.visiting[path] = true
	for _, im := range prog.Imports {
		fail := func(msg string) error { return &SourceError{path, m.source, &parser.Error{Pos: im.At, Msg: msg}} }
		if _, dup := m.imports[im.Alias]; dup || m.names[im.Alias] || im.Alias == "_" {
			return nil, fail("duplicate or conflicting import alias " + im.Alias)
		}
		if filepath.IsAbs(im.Path) || filepath.Ext(im.Path) != ".gors" {
			return nil, fail("import path must be a relative .gors file")
		}
		child, err := l.load(filepath.Join(filepath.Dir(path), filepath.FromSlash(im.Path)), false)
		if err != nil {
			if _, source := err.(*SourceError); source {
				return nil, err
			}
			return nil, fail(fmt.Sprintf("import %q: %v", im.Path, err))
		}
		m.imports[im.Alias] = child
	}
	l.visiting[path] = false
	l.order = append(l.order, m)
	return m, nil
}

func (m *module) resolve() error {
	var problem error
	resolve := func(name string, pos token.Pos) string {
		if alias, member, ok := strings.Cut(name, "::"); ok {
			target := m.imports[alias]
			if target == nil {
				problem = &parser.Error{Pos: pos, Msg: "unknown import alias " + alias}
				return name
			}
			if !target.names[member] || !target.public[member] {
				problem = &parser.Error{Pos: pos, Msg: fmt.Sprintf("%s::%s is missing or private", alias, member)}
				return name
			}
			return target.prefix + member
		}
		if m.names[name] {
			return m.prefix + name
		}
		return name
	}
	var typ func(ast.TypeExpr)
	typ = func(t ast.TypeExpr) {
		switch t := t.(type) {
		case *ast.NamedType:
			t.Name = resolve(t.Name, t.At)
			for _, a := range t.Args {
				typ(a)
			}
		case *ast.PtrType:
			typ(t.Elem)
		case *ast.SliceType:
			typ(t.Elem)
		}
	}
	for _, d := range m.prog.Structs {
		for _, f := range d.Fields {
			typ(f.Type)
		}
		d.Name = m.prefix + d.Name
	}
	for _, fn := range m.prog.Funcs {
		if fn.Recv == nil {
			fn.Name = m.prefix + fn.Name
		} else {
			typ(fn.Recv.Type)
		}
		for _, p := range fn.Params {
			typ(p.Type)
		}
		if fn.Ret != nil {
			typ(fn.Ret)
		}
		scopes := []map[string]bool{{}}
		if fn.Recv != nil {
			scopes[0][fn.Recv.Name] = true
		}
		for _, p := range fn.Params {
			scopes[0][p.Name] = true
		}
		local := func(name string) bool {
			for i := len(scopes) - 1; i >= 0; i-- {
				if scopes[i][name] {
					return true
				}
			}
			return false
		}
		var expr func(ast.Expr)
		var stmt func(ast.Stmt)
		var block func(*ast.Block, string)
		expr = func(e ast.Expr) {
			switch e := e.(type) {
			case *ast.Ident:
				if !local(e.Name) {
					e.Name = resolve(e.Name, e.At)
				}
			case *ast.Unary:
				expr(e.X)
			case *ast.Binary:
				expr(e.X)
				expr(e.Y)
			case *ast.AddrOf:
				expr(e.X)
			case *ast.Field:
				expr(e.X)
			case *ast.Index:
				expr(e.X)
				expr(e.Idx)
			case *ast.TryExpr:
				expr(e.X)
			case *ast.Call:
				expr(e.Fn)
				for _, a := range e.Args {
					expr(a)
				}
			case *ast.StructLit:
				e.Type = resolve(e.Type, e.At)
				for _, f := range e.Fields {
					expr(f.Value)
				}
			case *ast.SliceLit:
				for _, a := range e.Elems {
					expr(a)
				}
			}
		}
		block = func(b *ast.Block, binding string) {
			scopes = append(scopes, map[string]bool{})
			if binding != "" {
				scopes[len(scopes)-1][binding] = true
			}
			for _, s := range b.Stmts {
				stmt(s)
			}
			scopes = scopes[:len(scopes)-1]
		}
		stmt = func(s ast.Stmt) {
			switch s := s.(type) {
			case *ast.LetStmt:
				if s.Type != nil {
					typ(s.Type)
				}
				expr(s.Value)
				scopes[len(scopes)-1][s.Name] = true
			case *ast.AssignStmt:
				expr(s.Target)
				expr(s.Value)
			case *ast.ExprStmt:
				expr(s.X)
			case *ast.ReturnStmt:
				if s.X != nil {
					expr(s.X)
				}
			case *ast.IfStmt:
				expr(s.Cond)
				block(s.Then, "")
				if s.Else != nil {
					stmt(s.Else)
				}
			case *ast.IfLetStmt:
				expr(s.Value)
				block(s.Then, s.Name)
				if s.Else != nil {
					stmt(s.Else)
				}
			case *ast.WhileStmt:
				expr(s.Cond)
				block(s.Body, "")
			case *ast.MatchStmt:
				expr(s.Value)
				for _, a := range s.Arms {
					block(a.Body, a.Name)
				}
			case *ast.Block:
				block(s, "")
			}
		}
		block(fn.Body, "")
	}
	return problem
}

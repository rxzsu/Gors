package sema

import "gors/internal/ast"

// References are local borrows or parameters in v0.1, never stored in aggregates
// or returned. This deliberately avoids claiming an unimplemented lifetime system.
func containsRef(t Type) bool {
	switch t := t.(type) {
	case *Ptr:
		return true
	case *Slice:
		return containsRef(t.Elem)
	case *OptionT:
		return containsRef(t.Elem)
	case *ResultT:
		return containsRef(t.Ok) || containsRef(t.Err)
	}
	return false
}
func nestedRef(t Type) bool {
	switch t := t.(type) {
	case *Ptr:
		return containsRef(t.Elem)
	case *Slice, *OptionT, *ResultT:
		return containsRef(t)
	}
	return false
}

func (c *checker) validateTypes(prog *ast.Program) {
	state := map[string]int{}
	var visit func(Type) bool
	visit = func(t Type) bool {
		switch t := t.(type) {
		case *StructT:
			if state[t.Name] == 1 {
				return false
			}
			if state[t.Name] == 2 {
				return true
			}
			state[t.Name] = 1
			for _, name := range t.Order {
				if !visit(t.Fields[name]) {
					return false
				}
			}
			state[t.Name] = 2
		case *Slice:
			return visit(t.Elem)
		case *OptionT:
			return visit(t.Elem)
		case *ResultT:
			return visit(t.Ok) && visit(t.Err)
		}
		return true
	}
	for _, d := range prog.Structs {
		st := c.info.Structs[d.Name]
		if !visit(st) {
			c.errf("E116", d.At, "recursive aggregate types are not supported in v0.1")
			return
		}
		for _, f := range d.Fields {
			if containsRef(st.Fields[f.Name]) {
				c.errf("E211", f.At, "references cannot be stored in struct fields")
			}
		}
	}
}

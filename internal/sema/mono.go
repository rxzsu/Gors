package sema

import "sort"

// MonoTypes returns every distinct Option/Result instantiation used in info.
// Generic types are monomorphised: Option<int> and Option<string> become two
// concrete structs in the generated code.
func MonoTypes(info *Info) []Type {
	seen := map[string]Type{}

	var add func(t Type)
	add = func(t Type) {
		switch tt := t.(type) {
		case *OptionT:
			add(tt.Elem)
			seen[MonoName(tt)] = tt
		case *ResultT:
			add(tt.Ok)
			add(tt.Err)
			seen[MonoName(tt)] = tt
		case *Ptr:
			add(tt.Elem)
		case *Slice:
			add(tt.Elem)
			seen[MonoName(tt)] = tt
		}
	}

	for _, t := range info.ExprTypes {
		add(t)
	}
	for _, t := range info.LetTypes {
		add(t)
	}
	for _, t := range info.IfLetTypes {
		add(t)
	}
	for _, s := range info.Funcs {
		for _, p := range s.Params {
			add(p)
		}
		add(s.Ret)
	}
	for _, ms := range info.Methods {
		for _, s := range ms {
			for _, p := range s.Params {
				add(p)
			}
			add(s.Ret)
		}
	}

	names := make([]string, 0, len(seen))
	for n := range seen {
		names = append(names, n)
	}
	sort.Strings(names)

	out := make([]Type, len(names))
	for i, n := range names {
		out[i] = seen[n]
	}
	return out
}

// MonoName returns a stable identifier for a type, used as the generated
// struct name for monomorphised Option/Result.
func MonoName(t Type) string {
	switch tt := t.(type) {
	case *Basic:
		switch tt.Name {
		case "int":
			return "int64"
		case "float":
			return "float64"
		case "string":
			return "str"
		case "bool":
			return "bool"
		}
		return tt.Name
	case *StructT:
		return tt.Name
	case *Ptr:
		return "ref_" + MonoName(tt.Elem)
	case *Slice:
		return "Slice_" + MonoName(tt.Elem)
	case *OptionT:
		return "Option_" + MonoName(tt.Elem)
	case *ResultT:
		return "Result_" + MonoName(tt.Ok) + "_" + MonoName(tt.Err)
	}
	return "unknown"
}

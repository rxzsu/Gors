package compiler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gors/internal/compiler"
)

func TestCompiles(t *testing.T) {
	cases := map[string]string{
		"hello": `fn main() { println("hi") }`,
		"arith": `fn main() {
			let a = 2
			let b = 3
			let c = a * b + 1
			println(c)
		}`,
		"nll": `fn main() {
			let mut x = 1
			let r = &x
			println(r)
			x = 2
			println(x)
		}`,
		"struct": `struct P { x: int, y: int }
			fn norm2(p: &P) -> int { return p.x * p.x + p.y * p.y }
			fn main() {
				let mut p = P { x: 3, y: 4 }
				println(norm2(&p))
				p.x = 1
				println(p.x)
			}`,
		"methods": `struct P { x: int }
			fn (p: &P) get() -> int { return p.x }
			fn (p: &mut P) set(v: int) { p.x = v }
			fn main() {
				let mut p = P { x: 1 }
				println(p.get())
				p.set(2)
				println(p.get())
			}`,
		"option": `fn find(x: int) -> Option<int> {
				if x > 0 { return Some(x * 2) }
				return None
			}
			fn main() {
				let a = find(5)
				if let Some(v) = a { println(v) } else { println("none") }
			}`,
		"result": `fn parse(s: string) -> Result<int, string> {
				if s == "" { return Err("empty") }
				return Ok(1)
			}
			fn twice(s: string) -> Result<int, string> {
				let v = parse(s)?
				return Ok(v * 2)
			}
			fn main() {
				let r = twice("x")
				if let Ok(v) = r { println(v) }
			}`,
		"while": `fn main() {
			let mut i = 0
			while i < 3 {
				println(i)
				i = i + 1
			}
		}`,
	}

	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			out, errs := compiler.Compile(src)
			if len(errs) > 0 {
				t.Fatalf("unexpected errors: %v", errs)
			}
			if !strings.Contains(out, "package main") {
				t.Fatalf("generated source looks wrong:\n%s", out)
			}
		})
	}
}

func TestRejects(t *testing.T) {
	cases := []struct {
		name string
		src  string
		code string
	}{
		{
			name: "use after move",
			src: `fn take(s: string) { println(s) }
				fn main() {
					let s = "x"
					take(s)
					println(s)
				}`,
			code: "E200",
		},
		{
			name: "move struct",
			src: `struct P { x: int }
				fn take(p: P) { println(p.x) }
				fn main() {
					let p = P { x: 1 }
					take(p)
					println(p.x)
				}`,
			code: "E200",
		},
		{
			name: "assign while borrowed",
			src: `fn main() {
					let mut x = 1
					let r = &x
					x = 2
					println(r)
				}`,
			code: "E206",
		},
		{
			name: "double mutable borrow",
			src: `fn main() {
					let mut x = 1
					let a = &mut x
					let b = &mut x
					println(a)
					println(b)
				}`,
			code: "E204",
		},
		{
			name: "mut borrow while shared",
			src: `fn main() {
					let mut x = 1
					let a = &x
					let b = &mut x
					println(a)
					println(b)
				}`,
			code: "E204",
		},
		{
			name: "assign immutable",
			src: `fn main() {
					let x = 1
					x = 2
				}`,
			code: "E207",
		},
		{
			name: "use while mutably borrowed",
			src: `fn main() {
					let mut x = 1
					let r = &mut x
					println(x)
					println(r)
				}`,
			code: "E202",
		},
		{
			name: "type mismatch",
			src: `fn main() {
					let x = 1
					let y = "s"
					println(x + y)
				}`,
			code: "E118",
		},
		{
			name: "undefined name",
			src:  `fn main() { println(nope) }`,
			code: "E115",
		},
		{
			name: "missing field",
			src: `struct P { x: int, y: int }
				fn main() { let p = P { x: 1 } }`,
			code: "E130",
		},
		{
			name: "method as value",
			src: `struct P { x: int }
				fn (p: &P) get() -> int { return p.x }
				fn main() {
					let p = P { x: 1 }
					println(p.get)
				}`,
			code: "E143",
		},
		{
			name: "mut method while shared",
			src: `struct P { x: int }
				fn (p: &mut P) bump() { p.x = p.x + 1 }
				fn main() {
					let mut p = P { x: 1 }
					let r = &p
					p.bump()
					println(r.x)
				}`,
			code: "E204",
		},
		{
			name: "shared method while mutably borrowed",
			src: `struct P { x: int }
				fn (p: &P) get() -> int { return p.x }
				fn main() {
					let mut p = P { x: 1 }
					let r = &mut p
					println(p.get())
					println(r.x)
				}`,
			code: "E205",
		},
		{
			name: "receiver must be a struct",
			src: `fn (n: int) bad() { }
				fn main() { }`,
			code: "E140",
		},
		{
			name: "None without annotation",
			src: `fn main() {
					let x = None
					println(1)
				}`,
			code: "E150",
		},
		{
			name: "? outside Result function",
			src: `fn f() -> Result<int, string> { return Ok(1) }
				fn main() {
					let v = f()?
					println(v)
				}`,
			code: "E161",
		},
		{
			name: "? error type mismatch",
			src: `fn f() -> Result<int, string> { return Ok(1) }
				fn g() -> Result<int, int> {
					let v = f()?
					return Ok(v)
				}
				fn main() { }`,
			code: "E162",
		},
		{
			name: "? on non-Result",
			src: `fn f() -> Option<int> { return Some(1) }
				fn g() -> Result<int, string> {
					let v = f()?
					return Ok(v)
				}
				fn main() { }`,
			code: "E160",
		},
		{
			name: "if let wrong type",
			src: `fn main() {
					let x = 1
					if let Some(v) = x { println(v) }
				}`,
			code: "E151",
		},
		{
			name: "option is moved",
			src: `fn f() -> Option<int> { return Some(1) }
				fn main() {
					let a = f()
					let b = a
					println(a)
				}`,
			code: "E200",
		},
		{
			name: "immutable &mut",
			src: `fn main() {
					let x = 1
					let r = &mut x
					println(r)
				}`,
			code: "E124",
		},
	}

	for _, tc := range cases {
		for _, be := range []compiler.Backend{compiler.BackendGo, compiler.BackendC} {
			t.Run(tc.name+"/"+string(be), func(t *testing.T) {
				_, errs := compiler.CompileWith(tc.src, be)
				if len(errs) == 0 {
					t.Fatalf("expected %s, got no errors", tc.code)
				}
				var joined []string
				for _, e := range errs {
					joined = append(joined, e.Error())
				}
				if !strings.Contains(strings.Join(joined, "\n"), tc.code) {
					t.Fatalf("expected %s, got:\n%s", tc.code, strings.Join(joined, "\n"))
				}
			})
		}
	}
}

func TestExamplesCompile(t *testing.T) {
	files, err := filepath.Glob("../../examples/*.gors")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no examples found")
	}
	for _, f := range files {
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, errs := compiler.Compile(string(src)); len(errs) > 0 {
			t.Errorf("%s: %v", filepath.Base(f), errs)
		}
	}
}

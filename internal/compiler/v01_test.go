package compiler_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gors/internal/compiler"
)

func TestV01Rejects(t *testing.T) {
	cases := []struct{ name, src, code string }{
		{"incomplete match", `fn main(){match Some(1){Some(x)=>{println(x)}}}`, "E174"},
		{"duplicate arm", `fn main(){match Some(1){Some(x)=>{} Some(y)=>{} None=>{}}}`, "E172"},
		{"wrong variant", `fn main(){match Some(1){Ok(x)=>{} None=>{}}}`, "E171"},
		{"wrong arity", `fn main(){match Some(1){Some=>{} None(x)=>{}}}`, "E173"},
		{"non variant", `fn main(){match 1 {Some(x)=>{} None=>{}}}`, "E170"},
		{"match move merge", `fn take(s:string){} fn main(){let s="x";match Some(1){Some(v)=>{take(s)} None=>{}};println(s)}`, "E200"},
		{"return local reference", `fn bad()->&int {let x=1;return &x} fn main(){}`, "E211"},
		{"reference field", `struct Bad {r:&int} fn main(){}`, "E211"},
		{"reference slice", `fn main(){let x=1;let refs=[&x]}`, "E211"},
		{"reference option", `fn main(){let x=1;println(Some(&x))}`, "E211"},
		{"mutable alias", `fn main(){let mut x=1;let a=&mut x;let b=a;println(b)}`, "E209"},
		{"shared alias lifetime", `fn main(){let mut x=1;let a=&x;let b=a;x=2;println(b)}`, "E206"},
		{"reference assignment", `fn main(){let x=1;let y=2;let mut a=&x;a=&y;println(a)}`, "E210"},
		{"two mutable args", `fn f(a:&mut int,b:&mut int){} fn main(){let mut x=1;f(&mut x,&mut x)}`, "E204"},
		{"shared and mutable args", `fn f(a:&int,b:&mut int){} fn main(){let mut x=1;f(&x,&mut x)}`, "E204"},
		{"reborrow args", `fn f(a:&mut int,b:&mut int){} fn main(){let mut x=1;let r=&mut x;f(r,r)}`, "E204"},
		{"loop move", `fn take(s:string){} fn main(){let s="x";let mut i=0;while i<2 {take(s);i=i+1}}`, "E212"},
		{"loop backedge borrow", `fn main(){let mut x=1;let r=&x;let mut i=0;while i<2 {println(r);x=2;i=i+1}}`, "E206"},
		{"if let merge", `fn take(s:string){} fn main(){let s="x";if let Some(x)=Some(1){take(s)} else {println(s)};println(s)}`, "E200"},
		{"missing return", `fn f(x:bool)->int{if x{return 1}} fn main(){}`, "E118"},
		{"recursive type", `struct Node {next:Node} fn main(){}`, "E116"},
		{"shadow", `fn main(){let x=1;{let x=2;println(x)}}`, "E107"},
		{"main signature", `fn main(x:int){}`, "E117"},
		{"alias call lifetime", `fn f(a:&int,b:&mut int){} fn main(){let mut x=1;let r=&x;f(r,&mut x)}`, "E204"},
		{"unreachable", `fn f()->int{return 1;println(2)} fn main(){}`, "E120"},
		{"break outside loop", `fn main(){break}`, "E180"},
		{"continue outside loop", `fn main(){continue}`, "E181"},
		{"unreachable after break", `fn main(){while true {break;println(1)}}`, "E120"},
		{"unreachable after continue", `fn main(){while true {continue;println(1)}}`, "E120"},
		{"for non slice", `fn main(){for x in 123{println(x)}}`, "E182"},
		{"for shadow", `fn main(){let x=1;for x in [1,2]{println(x)}}`, "E107"},
		{"slice non int low", `fn main(){let xs=[1];let _=xs[true:1]}`, "E131"},
		{"ambiguous return reference", `fn bad(a: &int, b: &int) -> &int { return a; } fn main() {}`, "E211"},
		{"return local struct ref", `struct Point { x: int } fn bad(p: &Point) -> &int { let local = Point { x: 1 }; return &local.x; } fn main() {}`, "E211"},
		{"mutate during returned ref loan", `struct Point { x: int } fn (p: &Point) get() -> &int { return &p.x; } fn main() { let mut pt = Point { x: 1 }; let r = pt.get(); pt.x = 2; println(*r); }`, "E206"},
		{"match expr type mismatch", `fn main() { let x = match Some(1) { Some(v) => v, None => "err" }; }`, "E175"},
		{"slice non int high", `fn main(){let xs=[1];let _=xs[0:true]}`, "E131"},
		{"slice non indexable", `fn main(){let x=123;let _=x[0:1]}`, "E132"},
		{"slice strings need deep copy", `fn main(){let xs=["one"];let ys=xs[:]}`, "E183"},
		{"slice borrowed strings need deep copy", `fn main(){let xs=["one"];let r=&xs;let ys=r[:]}`, "E183"},
		{"slice structs need deep copy", `struct Box{s:string} fn main(){let xs=[Box{s:"one"}];let ys=xs[:]}`, "E183"},
		{"slice nested slices need deep copy", `fn main(){let xs=[[1]];let ys=xs[:]}`, "E183"},
		{"slice bound mutates source", `fn change(xs:&mut []int)->int{push(xs,3);return 1} fn main(){let mut xs=[1,2];let ys=xs[:change(&mut xs)]}`, "E204"},
		{"slice bound moves source", `fn size(xs:[]int)->int{return len(xs)} fn main(){let xs=[1,2];let ys=xs[:size(xs)]}`, "E203"},
		{"slice bound mutates referenced source", `fn change(xs:&mut []int)->int{push(xs,3);return 1} fn main(){let mut xs=[1,2];let r=&xs;let ys=r[:change(&mut xs)]}`, "E204"},
		{"discard read", `fn main(){let _=1;println(_)}`, "E121"},
		{"immutable method receiver", `struct Box{x:int} fn(b:&mut Box)mutate(){b.x=2} fn main(){let b=Box{x:1};b.mutate()}`, "E124"},
		{"shared receiver mutation", `struct Box{x:int} fn(b:&mut Box)mutate(){b.x=2} fn bad(b:&Box){b.mutate()} fn main(){}`, "E124"},
		{"receiver nested move", `struct Box{s:string} fn make(s:string)->Box{return Box{s:s}} fn(b:&Box)show(){println(b.s)} fn main(){let s="x";make(s).show();println(s)}`, "E200"},
		{"assignment index move", `fn index(s:string)->int{return 0} fn main(){let mut xs=[1];let s="x";xs[index(s)]=2;println(s)}`, "E200"},
		{"print borrowed field", `struct Box{s:string} fn change(b:&mut Box)->string{b.s="new";return "changed"} fn main(){let mut b=Box{s:"old"};println(b.s,change(&mut b))}`, "E204"},
		{"concat borrowed field", `struct Box{s:string} fn change(b:&mut Box)->string{b.s="new";return "changed"} fn main(){let mut b=Box{s:"old"};println(b.s+change(&mut b))}`, "E204"},
		{"receiver alias lifetime", `struct Box{x:int} fn(b:&mut Box)f(x:int){} fn read(b:&Box)->int{return b.x} fn main(){let mut b=Box{x:1};let r=&mut b;r.f(read(&b))}`, "E205"},
		{"borrowed field alias lifetime", `struct Box{s:string} fn change(b:&mut Box)->string{b.s="new";return "changed"} fn main(){let mut b=Box{s:"old"};let r=&b;println(r.s,change(&mut b))}`, "E204"},
		{"reserved name", `fn main(){let _t1=1}`, "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, be := range []compiler.Backend{compiler.BackendC, compiler.BackendGo} {
				_, errs := compiler.CompileWith(tc.src, be)
				found := false
				for _, e := range errs {
					if strings.Contains(e.Error(), tc.code) {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s: expected %s, got %v", be, tc.code, errs)
				}
			}
		})
	}
}

func TestV01AcceptsBorrows(t *testing.T) {
	cases := []string{
		`fn main(){let mut x=1;let a=&x;let b=a;println(b);x=2;println(x)}`,
		`fn f(a:&int,b:&int){} fn main(){let x=1;f(&x,&x)}`,
		`fn f(a:&mut int){} fn main(){let mut x=1;f(&mut x);f(&mut x)}`,
		`fn take(s:string){} fn main(){let mut i=0;while i<2{let s="local";take(s);i=i+1}}`,
	}
	for i, src := range cases {
		for _, be := range []compiler.Backend{compiler.BackendC, compiler.BackendGo} {
			if _, errs := compiler.CompileWith(src, be); len(errs) > 0 {
				t.Fatalf("case %d/%s: %v", i, be, errs)
			}
		}
	}
}

func TestModuleErrors(t *testing.T) {
	cases := []struct{ name, main, dep, want string }{
		{"private function", `import lib "lib.gors" fn main(){lib::secret()}`, `fn secret(){}`, "private"},
		{"private method", `import lib "lib.gors" fn main(){let b=lib::make();b.secret()}`, `pub struct Box {x:int} pub fn make()->Box{return Box{x:1}} fn(b:&Box)secret(){}`, "private"},
		{"unknown alias", `fn main(){missing::call()}`, ``, "unknown import"},
		{"missing export", `import lib "lib.gors" fn main(){lib::missing()}`, `pub fn good(){}`, "missing or private"},
		{"duplicate alias", `import lib "lib.gors" import lib "lib.gors" fn main(){}`, ``, "alias"},
		{"cycle", `import lib "lib.gors" fn main(){}`, `import entry "main.gors"`, "cyclic"},
		{"missing file", `import lib "missing.gors" fn main(){}`, ``, "import"},
		{"bad dependency", `import lib "lib.gors" fn main(){lib::bad()}`, `pub fn bad(){println(unknown)}`, "undefined"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, src := range map[string]string{"main.gors": tc.main, "lib.gors": tc.dep} {
				if e := os.WriteFile(filepath.Join(dir, name), []byte(src), 0600); e != nil {
					t.Fatal(e)
				}
			}
			for _, be := range []compiler.Backend{compiler.BackendC, compiler.BackendGo} {
				_, errs := compiler.CompileFile(filepath.Join(dir, "main.gors"), be)
				if len(errs) == 0 || !strings.Contains(errs[0].Error(), tc.want) {
					t.Fatalf("%s: expected %q, got %v", be, tc.want, errs)
				}
				if tc.name == "bad dependency" {
					if e, ok := errs[0].(*compiler.SourceError); !ok || filepath.Base(e.Path) != "lib.gors" || e.Source != tc.dep {
						t.Fatalf("lost dependency source: %v", errs)
					}
				}
			}
		})
	}
}

package compiler_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"gors/internal/compiler"
)

// Each case is compiled and executed independently by both toolchains. Expected
// output prevents two equally broken backends from validating one another.
func TestBackendExecution(t *testing.T) {
	cases := []struct {
		name, src, want string
		fails           bool
	}{
		{"temporaries", `fn text() -> string { return "hello" }
 fn strings() -> []string { return ["a", "bb"] }
 fn main() { println(text()); println(len(text())); println(text() == "hello"); println(text() + text()); println(len(strings())); text(); strings() }`, "hello\n5\ntrue\nhellohello\n2\n", false},
		{"partial move", `struct Inner { a: string, b: string }
 struct Outer { inner: Inner, tail: string }
 fn take(s: string) { println(s) }
 fn main() { let v = Outer { inner: Inner { a: "a", b: "b" }, tail: "c" }; take(v.inner.a) }`, "a\n", false},
		{"replacement", `struct Box { value: string }
 fn main() { let mut s = "a"; s = s + "b"; s = s + "c"; println(s)
 let mut b = Box { value: "old" }; b.value = "new"; println(b.value)
 let mut xs = ["old"]; xs[0] = "new"; println(xs[0]) }`, "abc\nnew\nnew\n", false},
		{"conditional move", `fn take(s: string) { println(s) }
 fn main() { let s = "retained"; if false { take(s) }; let t = "moved"; if true { take(t) } }`, "moved\n", false},
		{"loop and short circuit", `fn yes() -> bool { println("called"); return true }
 fn main() { let mut i = 0; while i < len("abc" + "d") { i = i + 1 }; println(i)
 println(false && yes()); println(true || yes()); println(true && yes()) }`, "4\nfalse\ntrue\ncalled\ntrue\n", false},
		{"option and result", `fn maybe(x: bool) -> Option<string> { if x { return Some("yes") }; return None }
 fn parse(x: bool) -> Result<string, string> { if x { return Ok("ok") }; return Err("bad") }
 fn chain(x: bool) -> Result<string, string> { let local = "cleanup"; let s = parse(x)?; return Ok(s + "!") }
 fn main() { if let Some(s) = maybe(true) { println(s) }; if let Some(s) = maybe(false) { println(s) } else { println("none") }
 if let Ok(s) = chain(true) { println(s) }; if let Err(s) = chain(false) { println(s) } }`, "yes\nnone\nok!\nbad\n", false},
		{"early return", `fn size(x: bool) -> int { let s = "abc"; if x { return len(s) }; return 0 }
 fn main() { println(size(true)); println(size(false)) }`, "3\n0\n", false},
		{"early error in arguments", `fn fail() -> Result<string, string> { return Err("error") }
 fn combine(a: string, b: string) -> string { return a + b }
 fn work() -> Result<string, string> { return Ok(combine("first", fail()?)) }
 fn main() { if let Err(e) = work() { println(e) } }`, "error\n", false},
		{"owning receiver", `struct Box { value: string }
 fn make() -> Box { println("make"); return Box { value: "owned" } }
 fn (b: Box) consume() -> string { return b.value }
 fn main() { println(make().consume()) }`, "make\nowned\n", false},
		{"indexed move", `fn take(s: string) { println(s) }
 fn main() { let xs = ["first", "second"]; take(xs[0]) }`, "first\n", false},
		{"short circuit with try", `fn value() -> Result<bool, string> { println("unexpected"); return Err("bad") }
 fn work() -> Result<bool, string> { let b = false && value()?; return Ok(b) }
 fn main() { if let Ok(b) = work() { println(b) } }`, "false\n", false},
		{"try in loop", `fn cond(i: int) -> Result<bool, string> { return Ok(i < 3) }
 fn work() -> Result<int, string> { let mut i = 0; while cond(i)? { i = i + 1 }; return Ok(i) }
 fn main() { if let Ok(i) = work() { println(i) } }`, "3\n", false},
		{"different result payload", `fn inner() -> Result<int, string> { return Err("bad") }
 fn outer() -> Result<string, string> { let i = inner()?; println(i); return Ok("ok") }
 fn main() { if let Err(e) = outer() { println(e) } }`, "bad\n", false},
		{"evaluation order", `fn value(i: int) -> string { println(i); return "x" }
 fn inner() -> Result<string, string> { println(2); return Ok("y") }
 fn combine(a: string, b: string) -> string { return a + b }
 fn outer() -> Result<string, string> { return Ok(combine(value(1), inner()?)) }
 fn main() { if let Ok(s) = outer() { println(s) } }`, "1\n2\nxy\n", false},
		{"main return", `fn main() { let s = "cleanup"; println(s); return }`, "cleanup\n", false},
		{"break loop", `fn main() {
 let mut i = 0
 while true {
     let temp = "owned string " + "in loop"
     if i == 3 {
         break
     }
     i = i + 1
 }
 println(i)
}`, "3\n", false},
		{"continue loop", `fn main() {
 let mut i = 0
 let mut sum = 0
 while i < 5 {
     i = i + 1
     let s = "item"
     if i % 2 == 0 {
         continue
     }
     sum = sum + i
 }
 println(sum)
}`, "9\n", false},
		{"for slice", `fn main() {
 let xs = [1, 2, 3]
 let mut sum = 0
 for x in xs {
     sum = sum + x
 }
 println(sum)
}`, "6\n", false},
		{"for index and temp", `fn main() {
 for i, s in ["a", "b"] {
     println(i)
     println(s)
 }
}`, "0\na\n1\nb\n", false},
		{"for break continue", `fn main() {
 let xs = [10, 20, 30, 40]
 for i, x in xs {
     if i == 0 { continue }
     if x > 30 { break }
     println(x)
 }
}`, "20\n30\n", false},
		{"subslice copy", `fn main() {
 let xs = [1, 2, 3, 4, 5]
 let s = xs[1:4]
 println(len(s))
 println(s[0])
 println(s[2])
}`, "3\n2\n4\n", false},
		{"subslice independent storage", `fn main() {
 let mut xs = [1, 2, 3]
 let mut ys = xs[0:2]
 ys[0] = 9
 xs[1] = 8
 push(&mut ys, 7)
 println(xs[0]); println(ys[1]); println(xs[2]); println(len(ys))
}`, "1\n2\n3\n3\n", false},
		{"subslice reference and empty", `fn main() {
 let xs = [1, 2, 3]
 let r = &xs
 let ys = r[1:]
 println(ys[0]); println(len(xs[3:])); println(len(xs[:0]))
 let empty: []int = []
 println(len(empty[:]))
}`, "2\n0\n0\n0\n", false},
		{"string slice", `fn main() {
 let s = "abcdef"
 println(s[:3])
 println(s[3:])
 println(s[2:4])
}`, "abc\ndef\ncd\n", false},
		{"conditional try owner", `fn work() -> Result<string, string> {
 let result: Result<string, string> = Ok("retained"); if false { let value = result?; println(value) }; return Ok("done") }
 fn main() { if let Ok(s) = work() { println(s) } }`, "done\n", false},
		{"error in assignment index", `fn index() -> Result<int, string> { return Err("bad index") }
 fn work() -> Result<string, string> { let mut xs = ["old"]; xs[index()?] = "new"; return Ok("done") }
 fn main() { if let Err(e) = work() { println(e) } }`, "bad index\n", false},
		{"match option", `fn main() { let o = Some("yes"); match o { Some(s) => { println(s) }, None => { println("none") } }
 let n: Option<string> = None; match n { None => { println("empty") }, Some(s) => { println(s) } } }`, "yes\nempty\n", false},
		{"match result", `fn choose(ok: bool) -> Result<string,string> { if ok { return Ok("yes") }; return Err("no") }
 fn consume(ok: bool) -> string { match choose(ok) { Err(e) => { return e }, Ok(v) => { return v } } }
 fn main() { println(consume(true)); println(consume(false)) }`, "yes\nno\n", false},
		{"match wildcard binding", `fn main() { match Some("drop") { Some(_) => {}, None => {} }; let r: Result<string,string> = Err("drop"); match r { Ok(_) => {}, Err(_) => {} } }`, "", false},
		{"match as expression", `fn describe(o: Option<int>) -> string {
	return match o {
		Some(v) => "value",
		None => "empty",
	};
}
fn main() {
	let a: Option<int> = Some(10);
	let x = match a {
		Some(v) => v + 5,
		None => 0,
	};
	println(x);
	let b: Option<int> = None;
	let y = match b {
		Some(v) => v + 5,
		None => 42,
	};
	println(y);
	println(describe(Some(10)));
	println(describe(None));
	let r: Result<string, string> = Ok("success");
	let msg = match r {
		Ok(v) => { println("got ok"); v + "!" },
		Err(e) => e,
	};
	println(msg);
}`, "15\n42\nvalue\nempty\ngot ok\nsuccess!\n", false},
		{"lifetime elision references", `struct Point { x: int, y: int }
fn (p: &Point) get_x() -> &int {
	return &p.x;
}
fn (p: &mut Point) x_mut() -> &mut int {
	return &mut p.x;
}
fn first(xs: &[]int) -> &int {
	return &xs[0];
}
fn main() {
	let mut pt = Point { x: 10, y: 20 };
	println(*pt.get_x());
	let rx = pt.get_x();
	println(*rx);
	let rx_mut = pt.x_mut();
	*rx_mut = 99;
	println(pt.x);
	let list = [100, 200, 300];
	println(*first(&list));
}`, "10\n10\n99\n100\n", false},
		{"file modules", "@file:../../examples/modules/main.gors", "25\nhello\n", false},
		{"discard values", `fn main(){let _="a";let _="b";if let Some(_)=Some("c"){};match Some("d"){Some(_)=>{} None=>{}}}`, "", false},
		{"push reference", `fn add(xs:&mut []string){push(xs,"two")} fn main(){let mut xs=["one"];add(&mut xs);println(xs[1])}`, "two\n", false},
		{"replace by consuming", `fn append(s:string)->string{return s+"!"} fn main(){let mut s="hello";s=append(s);println(s)}`, "hello!\n", false},
		{"discard parameters", `fn consume(_:string,_:string){} fn main(){consume("a","b")}`, "", false},
		{"identifier namespaces", `struct Pair{x:int,X:int}
 fn printf()->int{return 1} fn printf_()->int{return 2}
 fn gors_str_new()->string{return "ok"}
 fn main(){let fmt=printf();let fmt_=printf_();let p=Pair{x:fmt,X:fmt_};println(p.x,p.X);println(gors_str_new())}`, "1 2\nok\n", false},
		{"monomorphic names", `struct int64{x:int} fn main(){let a=[1];let b=[int64{x:2}];println(a[0]);println(b[0].x)}`, "1\n2\n", false},
		{"method symbol names", `struct A_B{x:int} struct A{x:int}
 fn(a:&A_B)C()->int{return a.x} fn(a:&A)B_C()->int{return a.x}
 fn main(){let a=A_B{x:1};let b=A{x:2};println(a.C());println(b.B_C())}`, "1\n2\n", false},
		{"bounds", `fn main() { let xs = [1]; println(xs[2]) }`, "gors: index 2 out of bounds (len 1)\n", true},
		{"slice negative bound", `fn main() { let xs = [1, 2]; println(len(xs[-1:1])) }`, "gors: slice bounds out of range [-1:1] (len 2)\n", true},
		{"slice reversed bounds", `fn main() { let xs = [1, 2]; println(len(xs[2:1])) }`, "gors: slice bounds out of range [2:1] (len 2)\n", true},
		{"slice past length", `fn main() { let xs = [1, 2]; println(len(xs[:3])) }`, "gors: slice bounds out of range [0:3] (len 2)\n", true},
		{"string slice bounds", `fn main() { println("ab"[0:3]) }`, "gors: slice bounds out of range [0:3] (len 2)\n", true},
	}
	for _, ex := range []struct{ name, want string }{
		{"hello", "hello from GoRs\n"}, {"structs", "25\n4\n5\n"}, {"methods", "25\n4\n5\n"},
		{"option", "10\nnone\n"}, {"result", "2\nempty input\n"}, {"slices", "4\n10\n4\n40\n99\n"},
		{"memory", "43992\n"},
	} {
		src, err := os.ReadFile(filepath.Join("..", "..", "examples", ex.name+".gors"))
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, struct {
			name, src, want string
			fails           bool
		}{"example " + ex.name, string(src), ex.want, false})
	}

	modules := t.TempDir()
	for name, source := range map[string]string{
		"main.gors": `import a "a.gors" import b "b.gors" import again "a.gors"
      fn main(){let x=a::make();let y=b::make();println(x.label());println(y.label());let _=again::make()}`,
		"a.gors": `import common "common.gors" pub struct Box{s:string}
      pub fn make()->Box{return Box{s:common::text()}}
      pub fn(b:&Box)label()->string{return b.s+""}`,
		"b.gors": `import common "common.gors" pub struct Box{s:string}
      pub fn make()->Box{return Box{s:common::text()+"2"}}
      pub fn(b:&Box)label()->string{return b.s+""}`,
		"common.gors": `pub fn text()->string{return "shared"}`,
	} {
		if err := os.WriteFile(filepath.Join(modules, name), []byte(source), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cases = append(cases, struct {
		name, src, want string
		fails           bool
	}{"module namespaces and diamond imports", "@file:" + filepath.Join(modules, "main.gors"), "shared\nshared2\n", false})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, backend := range []compiler.Backend{compiler.BackendC, compiler.BackendGo} {
				t.Run(string(backend), func(t *testing.T) {
					tool, ext := "go", ".go"
					if backend == compiler.BackendC {
						tool, ext = "gcc", ".c"
					}
					if _, err := exec.LookPath(tool); err != nil {
						t.Fatalf("required backend tool %s: %v", tool, err)
					}
					var src string
					var errs []error
					if strings.HasPrefix(tc.src, "@file:") {
						src, errs = compiler.CompileFile(strings.TrimPrefix(tc.src, "@file:"), backend)
					} else {
						src, errs = compiler.CompileWith(tc.src, backend)
					}
					if len(errs) > 0 {
						t.Fatalf("compile: %v", errs)
					}
					dir := t.TempDir()
					file := filepath.Join(dir, "main"+ext)
					bin := filepath.Join(dir, "app")
					if runtime.GOOS == "windows" {
						bin += ".exe"
					}
					if err := os.WriteFile(file, []byte(src), 0600); err != nil {
						t.Fatal(err)
					}
					args := []string{"build", "-o", bin, file}
					if backend == compiler.BackendC {
						args = []string{"-std=c23", "-O1", "-o", bin, file}
					}
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					if out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput(); err != nil {
						t.Fatalf("build: %v\n%s\n%s", err, out, src)
					}
					ctxRun, cancelRun := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancelRun()
					cmd := exec.CommandContext(ctxRun, bin)
					// Fatal bounds errors terminate without unwinding in either backend.
					// Check leak freedom on successful executions only.
					leakcheck := "1"
					if tc.fails {
						leakcheck = "0"
					}
					cmd.Env = append(os.Environ(), "GORS_LEAKCHECK="+leakcheck)
					var stdout, stderr bytes.Buffer
					cmd.Stdout = &stdout
					cmd.Stderr = &stderr
					err := cmd.Run()
					if ctxRun.Err() != nil {
						t.Fatal(ctxRun.Err())
					}
					// C stdio uses CRLF on Windows; compare logical output lines.
					stdoutText := strings.ReplaceAll(stdout.String(), "\r\n", "\n")
					stderrText := strings.ReplaceAll(stderr.String(), "\r\n", "\n")
					if tc.fails {
						if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
							t.Fatalf("expected nonzero exit, got %v", err)
						}
						if stderrText != tc.want {
							t.Fatalf("unexpected failure: %s", stderr.String())
						}
					} else if err != nil {
						t.Fatalf("run: %v\n%s", err, stderr.String())
					}
					wantStdout := tc.want
					if tc.fails {
						wantStdout = ""
					}
					if stdoutText != wantStdout {
						t.Fatalf("stdout %q, want %q", stdout.String(), wantStdout)
					}
					if !tc.fails && stderr.Len() != 0 {
						t.Fatalf("unexpected stderr (including leak report): %s", stderr.String())
					}
				})
			}
		})
	}
}

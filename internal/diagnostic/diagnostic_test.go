package diagnostic_test

import (
	"errors"
	"strings"
	"testing"

	"gors/internal/compiler"
	"gors/internal/diagnostic"
	"gors/internal/parser"
	"gors/internal/sema"
	"gors/internal/token"
)

func TestOwnershipDiagnostics(t *testing.T) {
	cases := []struct {
		name, src, code, note string
		noteLine              int
	}{
		{"move", "fn take(s: string) {}\nfn main() {\n let text = \"x\"\n take(text)\n println(text)\n}", "E200", "value moved here", 4},
		{"branch move", "fn take(s: string) {}\nfn main() {\n let text = \"x\"\n if true { take(text) }\n println(text)\n}", "E200", "value moved here", 4},
		{"borrow", "fn main() {\n let mut x = 1\n let r = &x\n x = 2\n println(r)\n}", "E206", "borrow starts here", 3},
		{"mutable borrow", "fn main() {\n let mut x = 1\n let r = &mut x\n println(x)\n println(r)\n}", "E202", "borrow starts here", 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, errs := compiler.Compile(tc.src)
			for _, err := range errs {
				e, ok := err.(*sema.Error)
				if !ok || e.Code != tc.code {
					continue
				}
				if len(e.Notes) != 1 || e.Notes[0].Pos.Line != tc.noteLine || e.Notes[0].Msg != tc.note {
					t.Fatalf("wrong related location: %+v", e.Notes)
				}
				rendered := diagnostic.Format("test.gors", tc.src, e)
				for _, want := range []string{"test.gors:", "error[" + tc.code + "]", " | ", "^", tc.note, "help:"} {
					if !strings.Contains(rendered, want) {
						t.Fatalf("missing %q:\n%s", want, rendered)
					}
				}
				return
			}
			t.Fatalf("missing %s: %v", tc.code, errs)
		})
	}
}

func TestSourceExcerpt(t *testing.T) {
	src := "fn main() {\r\n\tlet имя = 1\r\n}\r\n"
	got := diagnostic.Format("unicode.gors", src, &parser.Error{Pos: token.Pos{Line: 2, Col: 6}, Msg: "example"})
	want := "unicode.gors:2:6: error: example\n 2 | \tlet имя = 1\n   | \t    ^^^\n"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestUnavailableSource(t *testing.T) {
	for _, pos := range []token.Pos{{}, {Line: 99, Col: 1}, {Line: 1, Col: 99}} {
		got := diagnostic.Format("test.gors", "x", &parser.Error{Pos: pos, Msg: "bad"})
		if !strings.Contains(got, "error: bad") {
			t.Fatal(got)
		}
	}
	if got := diagnostic.Format("test.gors", "", errors.New("backend failed")); got != "test.gors: backend failed\n" {
		t.Fatal(got)
	}
}

func TestOwnershipRejections(t *testing.T) {
	cases := []struct{ name, src, code string }{
		{"borrowed field", `struct Box { value: string }
 fn (b: &Box) steal() -> string { return b.value }
 fn main() {}`, "E208"},
		{"indexed move", `fn take(s: string) {} fn main() { let xs = ["a", "b"]; take(xs[0]); println(len(xs)) }`, "E200"},
		{"method move", `struct Box { value: string }
 fn take(s: string) {}
 fn (b: Box) consume() { take(b.value); println(b.value) }
 fn main() {}`, "E200"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, backend := range []compiler.Backend{compiler.BackendC, compiler.BackendGo} {
				_, errs := compiler.CompileWith(tc.src, backend)
				found := false
				for _, err := range errs {
					if e, ok := err.(*sema.Error); ok && e.Code == tc.code {
						found = true
					}
				}
				if !found {
					t.Fatalf("%s: expected %s, got %v", backend, tc.code, errs)
				}
			}
		})
	}
}

func TestMultiDigitGutter(t *testing.T) {
	src := strings.Repeat("x\n", 10)
	got := diagnostic.Format("test.gors", src, &parser.Error{Pos: token.Pos{Line: 10, Col: 1}, Msg: "bad"})
	if !strings.Contains(got, " 10 | x\n    | ^\n") {
		t.Fatalf("misaligned marker:\n%s", got)
	}
}

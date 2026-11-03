package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandDiagnostics(t *testing.T) {
	for _, tc := range []struct{ name, source, want string }{
		{"ownership", "fn take(s: string) {}\nfn main() {\n let text = \"x\"\n take(text)\n println(text)\n}", "note: value moved here"},
		{"syntax", "fn main() { let x = }", "error:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bad.gors")
			if err := os.WriteFile(path, []byte(tc.source), 0600); err != nil {
				t.Fatal(err)
			}
			for _, command := range []struct {
				name string
				run  func() int
			}{
				{"emit", func() int { return emitCmd([]string{path}) }},
				{"run", func() int { return runCmd([]string{path}, false) }},
				{"build", func() int { return runCmd([]string{path}, true) }},
			} {
				t.Run(command.name, func(t *testing.T) {
					r, w, err := os.Pipe()
					if err != nil {
						t.Fatal(err)
					}
					defer r.Close()
					old := os.Stderr
					os.Stderr = w
					defer func() { os.Stderr = old; w.Close() }()
					code := command.run()
					w.Close()
					os.Stderr = old
					out, err := io.ReadAll(r)
					if err != nil {
						t.Fatal(err)
					}
					if code != 1 {
						t.Fatalf("exit %d, want 1", code)
					}
					for _, want := range []string{path + ":", tc.want, " | ", "^"} {
						if !strings.Contains(string(out), want) {
							t.Fatalf("missing %q:\n%s", want, out)
						}
					}
				})
			}
		})
	}
}

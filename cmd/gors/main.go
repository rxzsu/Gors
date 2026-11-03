// Command gors is the GoRs compiler driver.
//
//	gors run   file.gors [--backend c|go]   compile and execute
//	gors build file.gors [-o out]           compile to a native binary
//	gors emit  file.gors                    print the generated source
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"gors/internal/compiler"
	"gors/internal/diagnostic"
	"gors/internal/version"
)

type options struct {
	out     string
	backend compiler.Backend
	opt     string
	rest    []string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "version", "--version", "-V":
		fmt.Println("GoRs " + version.Version)
	case "run":
		os.Exit(runCmd(args, false))
	case "build":
		os.Exit(runCmd(args, true))
	case "emit":
		os.Exit(emitCmd(args))
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `GoRs compiler

usage:
  gors run   <file.gors> [--backend c|go]   compile and run
  gors build <file.gors> [-o out]           compile to a native binary
  gors emit  <file.gors> [--backend c|go]   print generated source
  gors --version                         print compiler version

backends:
  c   native code via gcc/clang -O2, no runtime, no GC (default when found)
  go  Go source, fast compile, GC runtime
`)
}

func parseArgs(args []string) options {
	o := options{opt: "-O2"}
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case a == "-o" || a == "--out":
			o.out = next()
		case strings.HasPrefix(a, "-o="):
			o.out = strings.TrimPrefix(a, "-o=")
		case a == "--backend":
			o.backend = compiler.Backend(next())
		case strings.HasPrefix(a, "--backend="):
			o.backend = compiler.Backend(strings.TrimPrefix(a, "--backend="))
		case a == "--opt":
			o.opt = next()
		case strings.HasPrefix(a, "--opt="):
			o.opt = strings.TrimPrefix(a, "--opt=")
		default:
			o.rest = append(o.rest, a)
		}
	}
	if o.backend == "" {
		o.backend = defaultBackend()
	}
	return o
}

// defaultBackend prefers the native C backend when a C compiler is available.
func defaultBackend() compiler.Backend {
	if findCC() != "" {
		return compiler.BackendC
	}
	return compiler.BackendGo
}

func findCC() string {
	for _, name := range []string{"gcc", "clang", "cc"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	return ""
}

func emitCmd(args []string) int {
	o := parseArgs(args)
	if len(o.rest) < 1 {
		usage()
		return 2
	}
	src, err := os.ReadFile(o.rest[0])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	out, errs := compiler.CompileFile(o.rest[0], o.backend)
	if len(errs) > 0 {
		printErrors(o.rest[0], string(src), errs)
		return 1
	}
	fmt.Print(out)
	return 0
}

func runCmd(args []string, build bool) int {
	o := parseArgs(args)
	if len(o.rest) < 1 {
		usage()
		return 2
	}
	path := o.rest[0]
	src, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}

	out, errs := compiler.CompileFile(path, o.backend)
	if len(errs) > 0 {
		printErrors(path, string(src), errs)
		return 1
	}

	tmp, err := os.MkdirTemp("", "gors-*")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer os.RemoveAll(tmp)

	binName := o.out
	if binName == "" {
		base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
		if base == "" {
			base = "a"
		}
		binName = base
		if isWindows() {
			binName += ".exe"
		}
	}
	if !build {
		binName = filepath.Join(tmp, "gors-bin")
		if isWindows() {
			binName += ".exe"
		}
	}

	var cerr error
	if o.backend == compiler.BackendC {
		cerr = compileC(o, out, tmp, binName)
	} else {
		cerr = compileGo(out, tmp, binName)
	}
	if cerr != nil {
		return 1
	}

	if build {
		fmt.Fprintf(os.Stderr, "built %s\n", binName)
		return 0
	}

	cmd := exec.Command(binName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = os.Stdin
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func compileC(o options, src, dir, bin string) error {
	cc := findCC()
	if cc == "" {
		fmt.Fprintln(os.Stderr, "no C compiler found (install gcc or clang), or use --backend go")
		return fmt.Errorf("no C compiler")
	}
	cFile := filepath.Join(dir, "main.c")
	if err := os.WriteFile(cFile, []byte(src), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	cmd := exec.Command(cc, o.opt, "-std=c23", "-o", bin, cFile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func compileGo(src, dir, bin string) error {
	goFile := filepath.Join(dir, "main.go")
	if err := os.WriteFile(goFile, []byte(src), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return err
	}
	cmd := exec.Command("go", "build", "-o", bin, goFile)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func printErrors(path, source string, errs []error) {
	for _, e := range errs {
		fmt.Fprint(os.Stderr, diagnostic.Format(path, source, e))
	}
}

func isWindows() bool {
	return os.PathSeparator == '\\'
}

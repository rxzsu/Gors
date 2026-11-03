// Command package builds reproducible release archives using only Go tools.
// Run from the repository root: go run ./tools/package -out dist
package main

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"gors/internal/version"
)

func main() {
	out := flag.String("out", "dist", "output directory")
	flag.Parse()
	if err := build(*out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
func build(out string) error {
	if err := os.MkdirAll(out, 0755); err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "gors-release-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)
	var sums bytes.Buffer
	for _, target := range []string{"linux", "windows"} {
		binary := "gors"
		if target == "windows" {
			binary += ".exe"
		}
		path := filepath.Join(work, binary)
		cmd := exec.Command("go", "build", "-mod=readonly", "-trimpath", "-ldflags=-buildid=", "-o", path, "./cmd/gors")
		cmd.Env = append(os.Environ(), "GOOS="+target, "GOARCH=amd64", "CGO_ENABLED=0")
		if log, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("build %s: %w\n%s", target, err, log)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		files := []struct {
			name string
			data []byte
			mode int64
		}{{binary, data, 0755}}
		for _, name := range []string{"README.md", "CHANGELOG.md", "docs/language-v0.1.md"} {
			data, err := os.ReadFile(name)
			if err != nil {
				return err
			}
			files = append(files, struct {
				name string
				data []byte
				mode int64
			}{name, data, 0644})
		}
		var archive bytes.Buffer
		ext := ".tar.gz"
		if target == "linux" {
			gz := gzip.NewWriter(&archive)
			tw := tar.NewWriter(gz)
			for _, f := range files {
				if err := tw.WriteHeader(&tar.Header{Name: f.name, Size: int64(len(f.data)), Mode: f.mode, ModTime: time.Unix(0, 0)}); err != nil {
					return err
				}
				if _, err := tw.Write(f.data); err != nil {
					return err
				}
			}
			if err := tw.Close(); err != nil {
				return err
			}
			if err := gz.Close(); err != nil {
				return err
			}
		} else {
			ext = ".zip"
			zw := zip.NewWriter(&archive)
			for _, f := range files {
				h := &zip.FileHeader{Name: f.name, Method: zip.Deflate}
				h.SetMode(os.FileMode(f.mode))
				h.SetModTime(time.Date(1980, 1, 1, 0, 0, 0, 0, time.UTC))
				w, err := zw.CreateHeader(h)
				if err != nil {
					return err
				}
				if _, err = w.Write(f.data); err != nil {
					return err
				}
			}
			if err := zw.Close(); err != nil {
				return err
			}
		}
		name := fmt.Sprintf("gors-%s-%s-amd64%s", version.Version, target, ext)
		if err := os.WriteFile(filepath.Join(out, name), archive.Bytes(), 0644); err != nil {
			return err
		}
		fmt.Fprintf(&sums, "%x  %s\n", sha256.Sum256(archive.Bytes()), name)
		fmt.Println(filepath.Join(out, name))
	}
	return os.WriteFile(filepath.Join(out, "SHA256SUMS"), sums.Bytes(), 0644)
}

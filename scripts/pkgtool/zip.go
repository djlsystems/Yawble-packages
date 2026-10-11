package main

import (
	"archive/zip"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// zipTime is every entry's time. A fixed time (with sorted entries and fixed modes) makes the same
// folder content give the same zip bytes, whenever and wherever it is built.
var zipTime = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// writeZip zips the content of dir with dir itself as the root: dir/solution.json is the entry
// solution.json. Files with any execute bit are 0755, others 0644, folders 0755. A link anywhere is
// refused, as Yawble refuses a zip holding one.
func writeZip(dir, zipPath string) error {
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a link; a package holds files and folders only", p)
		}
		if p != dir {
			paths = append(paths, p)
		}
		return nil
	})
	if err != nil {
		return err
	}
	rel := func(p string) string {
		r, _ := filepath.Rel(dir, p)
		return filepath.ToSlash(r)
	}
	sort.Slice(paths, func(i, j int) bool { return rel(paths[i]) < rel(paths[j]) })

	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return err
	}
	out, err := os.Create(zipPath)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(out)
	for _, p := range paths {
		info, err := os.Lstat(p)
		if err != nil {
			return err
		}
		h := &zip.FileHeader{Name: rel(p), Modified: zipTime}
		if info.IsDir() {
			h.Name += "/"
			h.SetMode(fs.ModeDir | 0o755)
			if _, err := zw.CreateHeader(h); err != nil {
				return err
			}
			continue
		}
		h.Method = zip.Deflate
		if info.Mode()&0o111 != 0 {
			h.SetMode(0o755)
		} else {
			h.SetMode(0o644)
		}
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		f, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := zw.Close(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

// stage copies a solution package's content into out: everything but plugins/ (each plugin's
// build.sh lays out its built version there) and dot-files such as .gitignore.
func stage(src, out string) error {
	if _, err := readPackage(src); err != nil {
		return err
	}
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		r, _ := filepath.Rel(src, p)
		if r == "." {
			return os.MkdirAll(out, 0o755)
		}
		if strings.HasPrefix(d.Name(), ".") || filepath.ToSlash(r) == "plugins" {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("%s is a link; a package holds files and folders only", p)
		}
		target := filepath.Join(out, r)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if info.Mode()&0o111 != 0 {
			mode = 0o755
		}
		return os.WriteFile(target, data, mode)
	})
}

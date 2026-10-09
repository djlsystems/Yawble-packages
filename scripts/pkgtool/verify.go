package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// verifyCatalog checks <dist>/catalog.json against the zips beside it and the package folders under
// <packages>: it holds only the fields docs/catalog.md names, generatedAt is UTC to the second, the
// packages are ordered by id, every package is listed once, every zip is listed, each entry's
// download names the tag and carries its zip's sha256 and size, and every other field is what the
// zip's manifests say. It returns every problem it finds, not only the first.
func verifyCatalog(tag, dist, packages string) []string {
	var problems []string
	fail := func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) }

	if err := checkTag(tag); err != nil {
		return []string{err.Error()}
	}
	data, err := os.ReadFile(filepath.Join(dist, "catalog.json"))
	if err != nil {
		return []string{fmt.Sprintf("no catalog: %v", err)}
	}
	var c catalog
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return []string{fmt.Sprintf("catalog.json: %v", err)}
	}
	if c.Schema != schema {
		fail("catalog.json is schema %d; this build writes schema %d", c.Schema, schema)
	}
	if t, err := time.Parse(generatedAtLayout, c.GeneratedAt); err != nil || t.Format(generatedAtLayout) != c.GeneratedAt {
		fail("catalog.json: generatedAt %q is not UTC ISO 8601 to the second, such as 2026-10-09T08:00:00Z", c.GeneratedAt)
	}
	for i := 1; i < len(c.Packages); i++ {
		if c.Packages[i-1].ID >= c.Packages[i].ID {
			fail("catalog.json: packages are not ordered by id (%s before %s)", c.Packages[i-1].ID, c.Packages[i].ID)
		}
	}

	listed := map[string]bool{}
	for _, e := range c.Packages {
		name := e.ID + "-" + e.Version + ".zip"
		if listed[name] {
			fail("%s is listed twice", name)
			continue
		}
		listed[name] = true
		if want := repository + "/releases/download/" + tag + "/" + name; e.Download.URL != want {
			fail("%s: download url is %s; the release %s serves it at %s", name, e.Download.URL, tag, want)
		}
		zipPath := filepath.Join(dist, name)
		zipData, err := os.ReadFile(zipPath)
		if err != nil {
			fail("%s is listed but there is no such zip in %s", name, dist)
			continue
		}
		sum := sha256.Sum256(zipData)
		if got := hex.EncodeToString(sum[:]); e.Download.SHA256 != got {
			fail("%s: catalog sha256 %s, the zip's is %s", name, e.Download.SHA256, got)
		}
		if got := int64(len(zipData)); e.Download.Bytes != got {
			fail("%s: catalog bytes %d, the zip has %d", name, e.Download.Bytes, got)
		}
		// Everything else must be what the zip's own manifests say; the download and source fields
		// were compared above or follow from the id.
		want, err := readEntry(zipPath, tag)
		if err != nil {
			fail("%s: %v", name, err)
			continue
		}
		got := e
		got.Download, want.Download = download{}, download{}
		if !reflect.DeepEqual(got, want) {
			fail("%s: its catalog entry differs from what its manifests say (%s)", name, entryDiff(got, want))
		}
	}

	zips, err := filepath.Glob(filepath.Join(dist, "*.zip"))
	if err != nil {
		fail("%v", err)
	}
	for _, z := range zips {
		if !listed[filepath.Base(z)] {
			fail("%s is in %s but not in the catalog", filepath.Base(z), dist)
		}
	}

	dirs, err := os.ReadDir(packages)
	if err != nil {
		fail("%v", err)
	}
	for _, d := range dirs {
		if !d.IsDir() || strings.HasPrefix(d.Name(), ".") {
			continue
		}
		p, err := readPackage(filepath.Join(packages, d.Name()))
		if err != nil {
			fail("%v", err)
			continue
		}
		if name := p.ID + "-" + p.Version + ".zip"; !listed[name] {
			fail("package %s %s is not in the catalog (no %s)", p.ID, p.Version, name)
		}
	}
	return problems
}

// entryDiff names the top-level fields where two entries differ.
func entryDiff(a, b entry) string {
	var fields []string
	va, vb, t := reflect.ValueOf(a), reflect.ValueOf(b), reflect.TypeOf(a)
	for i := 0; i < t.NumField(); i++ {
		if !reflect.DeepEqual(va.Field(i).Interface(), vb.Field(i).Interface()) {
			fields = append(fields, strings.Split(t.Field(i).Tag.Get("json"), ",")[0])
		}
	}
	sort.Strings(fields)
	return strings.Join(fields, ", ")
}

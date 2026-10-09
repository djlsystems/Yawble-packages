package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// builtRepo lays out one plugin package and its zip and catalog, as scripts/build.sh leaves them.
func builtRepo(t *testing.T) (dist, packages string) {
	t.Helper()
	root := t.TempDir()
	dist, packages = filepath.Join(root, "dist"), filepath.Join(root, "packages")
	files := map[string]string{
		"plugin.json":            goPlugin,
		"bin/linux-x64/hello*":   "x64",
		"bin/linux-arm64/hello*": "arm64",
	}
	write(t, filepath.Join(packages, "hello"), map[string]string{"plugin.json": goPlugin, "build.sh": "#!/bin/sh\n"})
	if err := os.MkdirAll(dist, 0o755); err != nil {
		t.Fatal(err)
	}
	buildZip(t, dist, "hello-1.2.3.zip", files)
	if err := writeCatalog(tag, dist, at); err != nil {
		t.Fatal(err)
	}
	return dist, packages
}

func wantProblem(t *testing.T, problems []string, want string) {
	t.Helper()
	for _, p := range problems {
		if strings.Contains(p, want) {
			return
		}
	}
	t.Fatalf("want a problem containing %q, got %q", want, problems)
}

func TestACatalogTheBuildWroteMatchesItsZips(t *testing.T) {
	dist, packages := builtRepo(t)
	if problems := verifyCatalog(tag, dist, packages); len(problems) != 0 {
		t.Fatal(problems)
	}
}

func TestAChangedZipNoLongerMatchesTheCatalog(t *testing.T) {
	dist, packages := builtRepo(t)
	f, err := os.OpenFile(filepath.Join(dist, "hello-1.2.3.zip"), os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("x"))
	f.Close()
	problems := verifyCatalog(tag, dist, packages)
	wantProblem(t, problems, "catalog sha256")
	wantProblem(t, problems, "catalog bytes")
}

func TestACatalogMustListEveryPackageAndEveryZipAndOnlyZipsThatExist(t *testing.T) {
	dist, packages := builtRepo(t)
	write(t, filepath.Join(packages, "other"), map[string]string{
		"plugin.json": strings.NewReplacer(`"hello"`, `"other"`, `"1.2.3"`, `"0.1.0"`).Replace(goPlugin),
	})
	buildZip(t, dist, "stray-1.0.0.zip", map[string]string{"plugin.json": "{}"})
	problems := verifyCatalog(tag, dist, packages)
	wantProblem(t, problems, "package other 0.1.0 is not in the catalog")
	wantProblem(t, problems, "stray-1.0.0.zip is in")

	os.Remove(filepath.Join(dist, "hello-1.2.3.zip"))
	wantProblem(t, verifyCatalog(tag, dist, packages), "hello-1.2.3.zip is listed but there is no such zip")
}

func TestACatalogEntryMustSayWhatItsManifestsSayAndNameTheReleaseTag(t *testing.T) {
	dist, packages := builtRepo(t)
	wantProblem(t, verifyCatalog("catalog-2026.10.08.2", dist, packages), "the release catalog-2026.10.08.2 serves it at")

	c, _ := readCatalog(t, dist)
	c.Packages[0].Summary = "Something typed by hand."
	data, _ := json.Marshal(c)
	os.WriteFile(filepath.Join(dist, "catalog.json"), data, 0o644)
	wantProblem(t, verifyCatalog(tag, dist, packages), "differs from what its manifests say (summary)")
}

// unmask spells out a fixture: the scan reads this file too, so the addresses and domains it must
// find are written with {at} and {dot} here and made real only in the folder it scans.
var unmask = strings.NewReplacer("{at}", "@", "{dot}", ".")

func scanText(t *testing.T, people string, files map[string]string) []string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		write(t, root, map[string]string{name: unmask.Replace(body)})
	}
	hosts := filepath.Join(t.TempDir(), "hosts")
	os.WriteFile(hosts, []byte("graph.microsoft.com # an API\n"), 0o644)
	peopleFile := ""
	if people != "" {
		peopleFile = filepath.Join(t.TempDir(), "people")
		os.WriteFile(peopleFile, []byte(people), 0o644)
	}
	findings, err := scanTree(root, hosts, peopleFile)
	if err != nil {
		t.Fatal(err)
	}
	return findings
}

func TestTheScanPassesMadeUpPeopleReservedDomainsAndTheProjectsOwn(t *testing.T) {
	findings := scanText(t, "Pat Committer\npat@committer.test\ndjlsystems\nDJL Systems, Inc.\n", map[string]string{
		"a.md": "Write to person@example.com, ops@mail.example.org or a@x.test; see https://example.net/x.\n" +
			"Source: https://github.com/djlsystems/Yawble-packages and github.com/djlsystems.\n" +
			"Calls https://graph.microsoft.com/v1.0 and http://127.0.0.1:8080, from @example.com%3E.\n" +
			"Copyright (c) 2026 DJL Systems, Inc.\n" +
			"Fields such as info.Email, user.email and plugin.mail are code.\n",
		"go.mod":  "module example.com/m\n\nrequire github{dot}com/someone/lib v1.0.0\n",
		"main.go": "import \"github{dot}com/someone/lib/sub\"\n",
		"bin":     "\x00binary with person{at}real-company{dot}com\n",
	})
	if len(findings) != 0 {
		t.Fatal(findings)
	}
}

func TestTheScanFindsRealAddressesDomainsAuthorsAndThePeopleWhoWorkHere(t *testing.T) {
	findings := scanText(t, "Pat Committer\n", map[string]string{
		"notes.md": "Mail jane{at}realmail{dot}com.\n" +
			"Her site is jane-doe{dot}dev and https://shop{dot}realstore{dot}net/cart.\n" +
			"Also github{dot}com/somebody/thing and https://github{dot}com/somebody.\n" +
			"Auth" + "or: Jane Doe\n" +
			"Thanks to pat committer for this.\n" +
			"Look-alikes: notexample{dot}com, example.com{dot}evil{dot}co\n",
	})
	for _, want := range []string{
		"notes.md:1: email address jane{at}realmail{dot}com",
		"notes.md:2: domain jane-doe{dot}dev",
		"notes.md:2: domain shop{dot}realstore{dot}net",
		"notes.md:3: domain github{dot}com",
		"notes.md:4: authorship line (Auth" + "or:)",
		`notes.md:5: names "pat committer"`,
		"notes.md:6: domain notexample{dot}com",
		"notes.md:6: domain example.com{dot}evil{dot}co",
	} {
		wantProblem(t, findings, unmask.Replace(want))
	}
	n := 0
	for _, f := range findings {
		if strings.HasPrefix(f, unmask.Replace("notes.md:3: domain github{dot}com")) {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want both links to somebody else's account found, got %q", findings)
	}
}

func TestATagMustNameARealDateAndANumberFromOne(t *testing.T) {
	for _, good := range []string{"catalog-2026.10.09.1", "catalog-2028.02.29.1", "catalog-2026.12.31.10"} {
		if err := checkTag(good); err != nil {
			t.Errorf("checkTag(%q) = %v, want nil", good, err)
		}
	}
	for _, bad := range []string{
		"catalog-2026.13.45.0", "catalog-2026.02.30.1", "catalog-2027.02.29.1", "catalog-0000.00.00.1",
		"catalog-2026.00.09.1", "catalog-2026.10.00.1", "catalog-2026.10.09.0", "catalog-2026.10.09.01",
		"catalog-2026.10.9.1", "catalog-2026.10.09", "catalog-2026.10.09.1 ", "v1",
	} {
		if err := checkTag(bad); err == nil {
			t.Errorf("checkTag(%q) = nil, want a refusal", bad)
		}
	}
}

// editCatalog rewrites dist/catalog.json through its generic JSON form, so a test can add what the
// catalog type has no field for.
func editCatalog(t *testing.T, dist string, edit func(c map[string]any)) {
	t.Helper()
	_, raw := readCatalog(t, dist)
	edit(raw)
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dist, "catalog.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestACatalogMayHoldOnlyTheFieldsItsSchemaNames(t *testing.T) {
	dist, packages := builtRepo(t)
	editCatalog(t, dist, func(c map[string]any) {
		c["packages"].([]any)[0].(map[string]any)["pricing"] = "free"
	})
	wantProblem(t, verifyCatalog(tag, dist, packages), `unknown field "pricing"`)

	dist, packages = builtRepo(t)
	editCatalog(t, dist, func(c map[string]any) {
		c["packages"].([]any)[0].(map[string]any)["download"].(map[string]any)["mirror"] = "none"
	})
	wantProblem(t, verifyCatalog(tag, dist, packages), `unknown field "mirror"`)
}

func TestGeneratedAtIsUTCToTheSecond(t *testing.T) {
	for _, bad := range []string{"yesterday", "2026-10-09T10:00:00+02:00", "2026-10-09T08:00:00.5Z", "2026-10-09 08:00:00Z", "2026-10-09", "2026-13-09T08:00:00Z", ""} {
		dist, packages := builtRepo(t)
		editCatalog(t, dist, func(c map[string]any) { c["generatedAt"] = bad })
		wantProblem(t, verifyCatalog(tag, dist, packages), "generatedAt")
	}
}

func TestTheCatalogListsPackagesOrderedById(t *testing.T) {
	dist, packages := builtRepo(t)
	write(t, filepath.Join(packages, "board"), map[string]string{"plugin.json": scriptPlugin, "build.sh": "#!/bin/sh\n"})
	buildZip(t, dist, "board-0.2.0.zip", map[string]string{"plugin.json": scriptPlugin, "board*": ""})
	if err := writeCatalog(tag, dist, at); err != nil {
		t.Fatal(err)
	}
	if problems := verifyCatalog(tag, dist, packages); len(problems) != 0 {
		t.Fatal(problems)
	}
	editCatalog(t, dist, func(c map[string]any) {
		p := c["packages"].([]any)
		p[0], p[1] = p[1], p[0]
	})
	wantProblem(t, verifyCatalog(tag, dist, packages), "not ordered by id")
}

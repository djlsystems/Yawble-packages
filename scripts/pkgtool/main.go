// pkgtool does what scripts/build.sh cannot do in sh alone: read the packages' JSON manifests, copy
// a solution package without its plugin sources, write reproducible zips and write the catalog.
//
//	pkgtool tag <tag>                        refuses a tag that is not catalog-<yyyy.mm.dd>.<n>
//	pkgtool info <package folder>            prints "<kind> <id> <version>"
//	pkgtool stage <package folder> <out>     copies a solution package into <out>, leaving out
//	                                         plugins/ (built separately) and dot-files
//	pkgtool zip <folder> <zip file>          zips the folder's content, the folder itself as the root
//	pkgtool catalog <tag> <dist folder>      writes <dist>/catalog.json from every <dist>/*.zip
//
// It uses the standard library only, so a clean clone needs nothing but Go and sh.
package main

import (
	"fmt"
	"os"
	"time"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pkgtool:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return usage()
	}
	switch cmd, rest := args[0], args[1:]; {
	case cmd == "tag" && len(rest) == 1:
		return checkTag(rest[0])
	case cmd == "info" && len(rest) == 1:
		p, err := readPackage(rest[0])
		if err != nil {
			return err
		}
		fmt.Printf("%s %s %s\n", p.Kind, p.ID, p.Version)
		return nil
	case cmd == "stage" && len(rest) == 2:
		return stage(rest[0], rest[1])
	case cmd == "zip" && len(rest) == 2:
		return writeZip(rest[0], rest[1])
	case cmd == "catalog" && len(rest) == 2:
		at, err := generatedAt()
		if err != nil {
			return err
		}
		return writeCatalog(rest[0], rest[1], at)
	}
	return usage()
}

func usage() error {
	return fmt.Errorf("usage: pkgtool tag <tag> | info <package> | stage <package> <out> | zip <folder> <zip> | catalog <tag> <dist>")
}

// generatedAt is now, or SOURCE_DATE_EPOCH when it is set, so a rebuild can be made byte-identical.
func generatedAt() (time.Time, error) {
	if s := os.Getenv("SOURCE_DATE_EPOCH"); s != "" {
		var sec int64
		if _, err := fmt.Sscanf(s, "%d", &sec); err != nil {
			return time.Time{}, fmt.Errorf("SOURCE_DATE_EPOCH %q is not a number of seconds", s)
		}
		return time.Unix(sec, 0).UTC(), nil
	}
	return time.Now().UTC(), nil
}

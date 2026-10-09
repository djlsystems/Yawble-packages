package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
)

var (
	idPattern      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)
	versionPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)
)

// pkg is a package folder as the build sees it: a solution (solution.json at its root) or a plugin
// (plugin.json at its root).
type pkg struct {
	Kind, ID, Version string
}

type solutionManifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Members     []struct {
		Name     string          `json:"name"`
		PluginID string          `json:"pluginId"`
		Secrets  json.RawMessage `json:"secrets"`
	} `json:"members"`
	Inputs struct {
		Settings []struct {
			Member      string `json:"member"`
			Setting     string `json:"setting"`
			Description string `json:"description"`
			Required    bool   `json:"required"`
		} `json:"settings"`
		Connections []struct {
			Member      string `json:"member"`
			Slot        string `json:"slot"`
			Description string `json:"description"`
			Required    bool   `json:"required"`
		} `json:"connections"`
		Documents []struct {
			Folder      string `json:"folder"`
			Description string `json:"description"`
			Required    bool   `json:"required"`
		} `json:"documents"`
	} `json:"inputs"`
}

type pluginManifest struct {
	ID          string                `json:"id"`
	Name        string                `json:"name"`
	Version     string                `json:"version"`
	Description string                `json:"description"`
	Executable  struct{ Path string } `json:"executable"`
	Platforms   json.RawMessage       `json:"platforms"`
	Requires    []string              `json:"requires"`
	Config      json.RawMessage       `json:"config"`
	Secrets     json.RawMessage       `json:"secrets"`
	Connections json.RawMessage       `json:"connections"`
}

type configField struct {
	SetBy       string `json:"setBy"`
	Required    bool   `json:"required"`
	Description string `json:"description"`
}

type secretField struct {
	Description string `json:"description"`
}

type connectionSlot struct {
	Description string   `json:"description"`
	Providers   []string `json:"providers"`
	Required    bool     `json:"required"`
}

// readPackage reads the manifest at a package folder's root and checks that the folder is named
// after its id, so packages/<id>/ and the zip <id>-<version>.zip can never disagree.
func readPackage(dir string) (pkg, error) {
	sol, err := os.ReadFile(filepath.Join(dir, "solution.json"))
	if err == nil {
		var m solutionManifest
		if err := json.Unmarshal(sol, &m); err != nil {
			return pkg{}, fmt.Errorf("%s/solution.json: %v", dir, err)
		}
		return checkPackage(dir, pkg{"solution", m.ID, m.Version})
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return pkg{}, err
	}
	plug, err := os.ReadFile(filepath.Join(dir, "plugin.json"))
	if errors.Is(err, fs.ErrNotExist) {
		return pkg{}, fmt.Errorf("%s holds neither solution.json nor plugin.json", dir)
	}
	if err != nil {
		return pkg{}, err
	}
	var m pluginManifest
	if err := json.Unmarshal(plug, &m); err != nil {
		return pkg{}, fmt.Errorf("%s/plugin.json: %v", dir, err)
	}
	return checkPackage(dir, pkg{"plugin", m.ID, m.Version})
}

func checkPackage(dir string, p pkg) (pkg, error) {
	if !idPattern.MatchString(p.ID) {
		return pkg{}, fmt.Errorf("%s: id %q is not a slug (lowercase letters, digits and hyphens, at most 48)", dir, p.ID)
	}
	if !versionPattern.MatchString(p.Version) {
		return pkg{}, fmt.Errorf("%s: version %q is not a plain version such as 1.0.0", dir, p.Version)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return pkg{}, err
	}
	if base := filepath.Base(abs); base != p.ID {
		return pkg{}, fmt.Errorf("%s: the folder is named %q but its manifest's id is %q; name the folder after the id", dir, base, p.ID)
	}
	return p, nil
}

// objectKeys answers a JSON object's keys in the order written, so the catalog lists slots,
// secrets and settings in the order their manifest does. null or absent is no keys.
func objectKeys(raw json.RawMessage) ([]string, error) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, fmt.Errorf("expected an object")
	}
	var keys []string
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		keys = append(keys, t.(string))
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

// orderedObject decodes a JSON object into its values and its keys in the order written.
func orderedObject[T any](raw json.RawMessage) ([]string, map[string]T, error) {
	keys, err := objectKeys(raw)
	if err != nil || keys == nil {
		return nil, map[string]T{}, err
	}
	values := map[string]T{}
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, nil, err
	}
	return keys, values, nil
}

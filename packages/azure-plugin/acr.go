package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// Container registries: create one in an owned resource group, and build an image in one the team
// owns or one in sharedContainerRegistries. A build uploads the folder (less what .dockerignore
// leaves out) to the address the registry hands out for it, runs the registry's own build, and
// answers the image's digest and the end of the build's log. No registry password or key is ever
// asked for.

const (
	acrRunsAPIVersion = "2019-06-01-preview"
	maxBuildBytes     = 512 << 20
	logTail           = 40
)

var (
	imageForm = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`)
	acrSKUs   = []string{"Basic", "Standard", "Premium"}
)

// registryHost is a registry's address.
func registryHost(name string) string { return strings.ToLower(name) + ".azurecr.io" }

func init() {
	register(
		command{words: []string{"acr", "create"}, usage: "<resource group> <registry> [Basic|Standard|Premium]", run: (*plugin).acrCreate},
		command{words: []string{"acr", "build"}, usage: "<registry> <image:tag> <folder>", run: (*plugin).acrBuild},
	)
}

func (p *plugin) acrCreate(args []string) (string, error) {
	if err := argsExactly(args, 2, 3, "acr create", "<resource group> <registry> [Basic|Standard|Premium]"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	rg, name, sku := args[0], strings.ToLower(args[1]), "Basic"
	if !acrName.MatchString(name) {
		return "", refusef("Refused: %q is not a registry name: 5 to 50 letters and digits, unique in all of Azure.", args[1])
	}
	if len(args) == 3 {
		found := false
		for _, s := range acrSKUs {
			if strings.EqualFold(s, args[2]) {
				sku, found = s, true
			}
		}
		if !found {
			return "", refusef("Refused: %s is not a registry sku; use Basic, Standard or Premium.", args[2])
		}
	}
	id := rgPath(p.cfg.Subscription, rg) + "/providers/Microsoft.ContainerRegistry/registries/" + name
	if err := p.mustOwn(change{id: id, kind: changeCreate}); err != nil {
		return "", err
	}
	what := fmt.Sprintf("create container registry %s (%s) in %s with its admin user off, tagged for this team", name, sku, rg)
	if !p.cfg.apply() {
		return p.whatif("acr create", id, what, map[string]int{"create": 1}), nil
	}
	location, err := p.rgLocation(rg)
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"location": location, "sku": map[string]any{"name": sku}, "tags": p.ownTags(),
		"properties": map[string]any{"adminUserEnabled": false},
	}
	if _, _, err := p.put(id, body, "the registry "+name); err != nil {
		return "", err
	}
	p.applied("acr create", id, what, []string{id})
	return fmt.Sprintf("Container registry %s is ready in %s, at %s. Build into it with acr build; an app pulls from it with its own managed identity.", name, rg, registryHost(name)), nil
}

func (p *plugin) acrBuild(args []string) (string, error) {
	if err := argsExactly(args, 3, 3, "acr build", "<registry> <image:tag> <folder>"); err != nil {
		return "", err
	}
	if err := p.needSubscription(); err != nil {
		return "", err
	}
	regName, _ := lastName(args[0], acrID, ".azurecr.io")
	image := args[1]
	if !imageForm.MatchString(image) {
		return "", refusef("Refused: %q is not an image name such as my-app:1.0 (lower-case name, then a tag).", image)
	}
	if !strings.Contains(image[strings.LastIndex(image, "/")+1:], ":") {
		image += ":latest"
	}
	dir, err := p.allowedPath(args[2], "builds only folders from those")
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return "", refusef("Refused: %s is not a folder.", args[2])
	}
	if info, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil || !info.Mode().IsRegular() {
		return "", refusef("Refused: %s has no Dockerfile.", args[2])
	}
	registry, err := p.findByName("Microsoft.ContainerRegistry/registries", regName, "container registry")
	if err != nil {
		return "", err
	}
	if why, err := p.guard(change{id: registry.ID, kind: changePush}); err != nil {
		return "", err
	} else if why != "" {
		return "", refusef("Refused: %s. This member builds only into registries its team owns and those in sharedContainerRegistries. Nothing was changed.", why)
	}
	source, files, err := packFolder(dir)
	if err != nil {
		return "", err
	}
	server := registryHost(registry.Name)
	what := fmt.Sprintf("upload %s (%d files, %d KB packed, after .dockerignore) and build %s/%s in the registry", filepath.Base(dir), files, (len(source)+1023)/1024, server, image)
	if !p.cfg.apply() {
		return p.whatif("acr build", registry.ID, what, map[string]int{"modify": 1}), nil
	}

	// THE UPLOAD: to the address the registry hands out for this one source.
	p.progress(fmt.Sprintf("azure: uploading %d files to %s", files, server))
	var upload struct {
		UploadURL    string `json:"uploadUrl"`
		RelativePath string `json:"relativePath"`
	}
	if _, err := p.az.arm(http.MethodPost, registry.ID+"/listBuildSourceUploadUrl?api-version="+acrRunsAPIVersion, nil, &upload); err != nil {
		return "", err
	}
	p.hide(upload.UploadURL)
	if upload.UploadURL == "" || upload.RelativePath == "" {
		return "", fmt.Errorf("the registry %s gave no address to upload the source to", registry.Name)
	}
	if _, err := anonymous(http.MethodPut, upload.UploadURL, source, map[string]string{"x-ms-blob-type": "BlockBlob", "x-ms-version": blobVersion}, 10*time.Minute); err != nil {
		return "", fmt.Errorf("the source could not be uploaded: %w", err)
	}

	// THE BUILD.
	run := map[string]any{
		"type": "DockerBuildRequest", "imageNames": []string{image}, "isPushEnabled": true,
		"sourceLocation": upload.RelativePath, "dockerFilePath": "Dockerfile",
		"platform": map[string]any{"os": "Linux", "architecture": "amd64"}, "timeout": 3600,
	}
	var scheduled struct {
		Properties struct {
			RunID string `json:"runId"`
		} `json:"properties"`
	}
	if _, err := p.az.arm(http.MethodPost, registry.ID+"/scheduleRun?api-version="+acrRunsAPIVersion, run, &scheduled); err != nil {
		return "", err
	}
	runID := scheduled.Properties.RunID
	if runID == "" {
		return "", fmt.Errorf("the registry %s did not start the build", registry.Name)
	}
	type runState struct {
		Properties struct {
			Status       string `json:"status"`
			OutputImages []struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				Digest     string `json:"digest"`
			} `json:"outputImages"`
		} `json:"properties"`
	}
	var state runState
	deadline := time.Now().Add(pollFor)
	for {
		if _, err := p.az.arm(http.MethodGet, registry.ID+"/runs/"+runID+"?api-version="+acrRunsAPIVersion, nil, &state); err != nil {
			return "", err
		}
		status := state.Properties.Status
		if s := strings.ToLower(status); s == "succeeded" || s == "failed" || s == "canceled" || s == "error" || s == "timeout" {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the build %s did not finish within %d minutes; it may still finish in the registry", runID, int(pollFor.Minutes()))
		}
		p.progress(fmt.Sprintf("azure: build %s of %s is %s", runID, image, strings.ToLower(status)))
		sleep(pollEvery)
	}
	tail := p.buildLog(registry.ID, runID)
	if !strings.EqualFold(state.Properties.Status, "Succeeded") {
		return "", fmt.Errorf("the build %s of %s ended %s. The end of its log:\n%s", runID, image, strings.ToLower(state.Properties.Status), tail)
	}
	digest := ""
	for _, o := range state.Properties.OutputImages {
		if o.Digest != "" {
			digest = o.Digest
			break
		}
	}
	ref := server + "/" + image
	if digest != "" {
		ref = server + "/" + image[:strings.LastIndex(image, ":")] + "@" + digest
	}
	p.applied("acr build", registry.ID, fmt.Sprintf("built and pushed %s/%s (%s)", server, image, digest), []string{registry.ID})
	return fmt.Sprintf("Built and pushed %s/%s in run %s.\nDigest: %s\nPin it as %s.\nThe end of the build's log:\n%s", server, image, runID, digest, ref, tail), nil
}

// buildLog answers the last lines of a build's log, from the address the registry hands out for it.
func (p *plugin) buildLog(registryID, runID string) string {
	var link struct {
		LogLink string `json:"logLink"`
	}
	if _, err := p.az.arm(http.MethodPost, registryID+"/runs/"+runID+"/listLogSasUrl?api-version="+acrRunsAPIVersion, nil, &link); err != nil || link.LogLink == "" {
		return "(the log could not be read)"
	}
	p.hide(link.LogLink)
	ans, err := anonymous(http.MethodGet, link.LogLink, nil, nil, 2*time.Minute)
	if err != nil {
		return "(the log could not be read)"
	}
	lines := strings.Split(strings.TrimRight(strings.ReplaceAll(string(ans.body), "\r\n", "\n"), "\n"), "\n")
	if len(lines) > logTail {
		lines = lines[len(lines)-logTail:]
	}
	return strings.Join(lines, "\n")
}

// packFolder packs a build folder as a gzipped tar, leaving out .git and whatever its .dockerignore
// leaves out; the Dockerfile and .dockerignore always go, as Docker sends them. Links are packed as
// links, never followed.
func packFolder(dir string) ([]byte, int, error) {
	ignore := readDockerignore(dir)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files, total := 0, int64(0)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if rel == ".git" && d.IsDir() {
			return filepath.SkipDir
		}
		if rel != "Dockerfile" && rel != ".dockerignore" && ignore.excludes(rel) {
			if d.IsDir() && !ignore.mayInclude(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(path)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil // a socket or device: not part of a build
		}
		// PORTABLE MODES, OWNED BY ROOT: each entry as git checks it out and docker build sees it,
		// never the folder's own modes, which on a volume with umask 0007 would leave a non-root
		// image user unable to read its own files.
		hdr.Name, hdr.Uname, hdr.Gname, hdr.Uid, hdr.Gid = rel, "", "", 0, 0
		switch {
		case d.IsDir():
			hdr.Name += "/"
			hdr.Mode = 0o755
		case info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0:
			hdr.Mode = 0o755
		case info.Mode().IsRegular():
			hdr.Mode = 0o644
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
			if total > maxBuildBytes {
				return refusef("Refused: the folder holds more than %d MB after .dockerignore; leave large files out with .dockerignore.", maxBuildBytes>>20)
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			_, err = io.Copy(tw, f)
			f.Close()
			if err != nil {
				return err
			}
			files++
		}
		return nil
	})
	if r, ok := err.(refusal); ok {
		return nil, 0, r
	}
	if err != nil {
		return nil, 0, fmt.Errorf("the folder could not be packed: %v", err)
	}
	if err := tw.Close(); err != nil {
		return nil, 0, err
	}
	if err := gz.Close(); err != nil {
		return nil, 0, err
	}
	return buf.Bytes(), files, nil
}

// dockerignore is a folder's .dockerignore: patterns read in order, the last that matches deciding,
// a ! pattern putting a path back.
type dockerignore []ignoreRule

type ignoreRule struct {
	re     *regexp.Regexp
	negate bool
	raw    string
}

func readDockerignore(dir string) dockerignore {
	raw, err := os.ReadFile(filepath.Join(dir, ".dockerignore"))
	if err != nil {
		return nil
	}
	var rules dockerignore
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		negate := strings.HasPrefix(line, "!")
		line = strings.TrimPrefix(line, "!")
		line = strings.Trim(filepath.ToSlash(filepath.Clean(line)), "/")
		if line == "." || line == "" {
			continue
		}
		if re, err := regexp.Compile("^" + globToRegexp(line) + "(/.*)?$"); err == nil {
			rules = append(rules, ignoreRule{re: re, negate: negate, raw: line})
		}
	}
	return rules
}

// globToRegexp turns a .dockerignore pattern into a regular expression: ** is any number of folders,
// * any run of characters but /, ? one such character.
func globToRegexp(glob string) string {
	var b strings.Builder
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case c == '*' && i+1 < len(glob) && glob[i+1] == '*':
			i++
			if i+1 < len(glob) && glob[i+1] == '/' {
				i++
				b.WriteString("(.*/)?")
			} else {
				b.WriteString(".*")
			}
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '\\' && i+1 < len(glob):
			i++
			b.WriteString(regexp.QuoteMeta(string(glob[i])))
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	return b.String()
}

func (d dockerignore) excludes(rel string) bool {
	out := false
	for _, r := range d {
		if r.re.MatchString(rel) {
			out = !r.negate
		}
	}
	return out
}

// mayInclude reports whether a ! pattern could put back something inside an excluded folder, so
// the folder must still be walked.
func (d dockerignore) mayInclude(dir string) bool {
	for _, r := range d {
		if r.negate && (strings.HasPrefix(r.raw, dir+"/") || strings.Contains(r.raw, "*")) {
			return true
		}
	}
	return false
}

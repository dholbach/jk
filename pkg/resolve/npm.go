package resolve

import (
	"encoding/json"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Note on path/filepath: for portability, we should use `path`, since
// import specifiers always use forward slashes; however, `path` is
// not quite as convenient as filepath when opening files, so for now,
// I'm using filepath.

/* ## Module resolution for npm packages

[npm](https://docs.npmjs.com/about-npm/) is a convenient way to
distribute JavaScript modules. By and large these are for Node.JS (and
use CommonJS modules) or for browsers (and have whatever unholy
packaging was considered best practice at the time); but, it's
possible to publish ES2015 modules as well, either consisting of just
ES6 code, or hybrid packages that can be used with more than one
module system.

Here's how npm packages work:

 1. When you `npm install foo`, npm creates a directory
 `./node_modules/foo` with the contents of the package in it.

 2. In that directory, there will be a file `package.json`, which has
 various metadata, and some fields describing how `foo` is to be used;

 3. If the `package.json` has entries under `.dependencies`, these
 will also be downloaded by npm. It may put them inside the foo
 directory (i.e., under `./node_modules/foo/node_modules`), or flatten
 them out into the directory that `foo` is in (i.e., under
 `./node_modules`). Newer npm releases tend to try and flatten
 dependencies.

 4. If the `package.json` file has an field `.main`, that points to
 the file to be used when the package is referred to by its directory,
 e.g., if you import `foo`.

The algorithm for resolving a module path is described in
<https://nodejs.org/api/modules.html#modules_all_together>. This file
adapts that algorithm to account for only supporting ES2015 modules,
by

 - using the file extensions `.js`, `.mjs`
 - using the field `module` in `package.json`, instead of `main`
 - not looking for node_modules directories above the directory given
   as the top-level.

*/

// NodeModulesImporter is an implementation of Importer that uses a
// resolution algorithm adapted from Node.JS's, in order to support
// modules installed with npm.
type NodeModulesImporter struct {
	// ModulesPath is the top-level directory at which to stop looking
	// for "module paths" (those that don't start with `/`, `./`, or
	// `../`).
	ModuleBase string
}

// Import is the entry point into the module resolution algorithm.
func (n *NodeModulesImporter) Import(basePath, specifier, referrer string) ([]byte, string, []Candidate) {
	if filepath.IsAbs(specifier) {
		log.Fatalf("absolute import path %q not allowed in %q", specifier, referrer)
	}
	if strings.HasPrefix(specifier, "./") || strings.HasPrefix(specifier, "../") {
		return loadAsPath(filepath.Join(basePath, specifier))
	}
	return loadAsModule(specifier, basePath)
}

var moduleExtensions = []string{".mjs", ".js"}

// loadAsFile tries to load a path as though it referred to a file. No
// bytes returned means failure.
func loadAsFile(path string) ([]byte, string, []Candidate) {
	candidates := []Candidate{{path, verbatimRule}}
	bytes, err := ioutil.ReadFile(path)
	if err == nil {
		return bytes, path, candidates
	}

	bytes, path, extCandidates := loadGuessedFile(path)
	return bytes, path, append(candidates, extCandidates...)
}

func loadGuessedFile(path string) ([]byte, string, []Candidate) {
	var candidates []Candidate
	for _, ext := range moduleExtensions {
		p := path + ext
		candidates = append(candidates, Candidate{p, extensionRulePrefix + ext})
		bytes, err := ioutil.ReadFile(p)
		if err == nil {
			return bytes, p, candidates
		}
	}
	return nil, "", candidates
}

// loadAsPath attempts to load a path when it's unknown whether it
// refers to a file or a directory.
func loadAsPath(path string) ([]byte, string, []Candidate) {
	candidates := []Candidate{{path, verbatimRule}}
	info, err := os.Stat(path)
	switch {
	default: // file exists and is not a directory
		bytes, err := ioutil.ReadFile(path)
		if err != nil {
			log.Fatal(err)
		}
		return bytes, path, candidates

	case os.IsNotExist(err):
		bytes, path, guessedCandidates := loadGuessedFile(path)
		return bytes, path, append(candidates, guessedCandidates...)

	case err != nil:
		log.Fatal(err)

	case info.IsDir():
		bytes, path, dirCandidates := loadAsDir(path)
		return bytes, path, append(candidates, dirCandidates...)
	}
	return nil, "", candidates
}

// loadIndex tries to load the default index files, assuming the path
// is a directory.
func loadIndex(path string) ([]byte, string, []Candidate) {
	var candidates []Candidate
	for _, ext := range moduleExtensions {
		p := filepath.Join(path, "index"+ext)
		candidates = append(candidates, Candidate{p, indexRulePrefix + ext})
		bytes, err := ioutil.ReadFile(p)
		if err == nil {
			return bytes, p, candidates
		}
	}
	return nil, "", candidates
}

// loadAsDir attempts to load a path which is known to be a directory.
func loadAsDir(path string) ([]byte, string, []Candidate) {
	var candidates []Candidate

	packageJSONPath := filepath.Join(path, "package.json")
	packageJSON, _ := ioutil.ReadFile(packageJSONPath)
	if packageJSON != nil {
		var pkg struct{ Module string }
		if err := json.Unmarshal(packageJSON, &pkg); err == nil && pkg.Module != "" {
			module := filepath.Join(path, pkg.Module)
			// .module is treated as through it were a file (but not a directory)
			bytes, path, pkgCandidates := loadAsFile(module)
			// TODO(michael) consider transformating these candidates
			// (and below) to reflect the indirection through
			// package.json
			qualifyCandidates(pkgCandidates, "via .module in "+packageJSONPath)
			candidates = append(candidates, pkgCandidates...)
			if bytes != nil {
				return bytes, path, candidates
			}
			// .. or a directory with an index (but not another package.json)
			bytes, path, modIndexCandidates := loadIndex(module)
			qualifyCandidates(modIndexCandidates, "via .module in "+packageJSONPath)
			candidates = append(candidates, modIndexCandidates...)
			if bytes != nil {
				return bytes, path, candidates
			}
		}
	}
	bytes, path, indexCandidates := loadIndex(path)
	return bytes, path, append(candidates, indexCandidates...)
}

// loadAsModule attempts to load a specifier as though it referred to
// a package in (potentially nested) node_modules directories.
func loadAsModule(specifier, base string) (_ []byte, _ string, candidates []Candidate) {
	defer func() {
		qualifyCandidates(candidates, "via NPM resolution")
	}()

	bits := strings.Split(base, "/")
	for i := len(bits) - 1; i >= 0; i-- {
		if bits[i] == "node_modules" {
			continue
		}
		path := strings.Join(append(bits[:i+1], "node_modules", specifier), "/")
		bytes, path, pathCandidates := loadAsPath(path)
		candidates = append(candidates, pathCandidates...)
		if bytes != nil {
			return bytes, path, candidates
		}
	}
	return nil, "", candidates
}

func qualifyCandidates(cs []Candidate, extra string) {
	for i := range cs {
		cs[i].Rule = cs[i].Rule + ", " + extra
	}
}

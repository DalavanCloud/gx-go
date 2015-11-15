package main

import (
	"errors"
	"fmt"
	"go/build"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	rw "github.com/whyrusleeping/gx-go/rewrite"
	gx "github.com/whyrusleeping/gx/gxutil"
	. "github.com/whyrusleeping/stump"
)

func doUpdate(oldimp, newimp string) error {
	curpath, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("error getting working dir: ", err)
	}

	rwf := func(in string) string {
		return strings.Replace(in, oldimp+"/", newimp+"/", 1)
	}

	filter := func(in string) bool {
		return strings.HasSuffix(in, ".go")
	}

	return rw.RewriteImports(curpath, rwf, filter)
}

func pathIsNotStdlib(path string) bool {
	first := strings.Split(path, "/")[0]

	if len(strings.Split(first, ".")) > 1 {
		return true
	}
	return false
}

type Importer struct {
	pkgs    map[string]*gx.Dependency
	gopath  string
	pm      *gx.PM
	rewrite bool
	yesall  bool
}

func NewImporter(rw bool) (*Importer, error) {
	gp, err := getGoPath()
	if err != nil {
		return nil, err
	}

	cfg, err := gx.LoadConfig()
	if err != nil {
		return nil, err
	}

	pm, err := gx.NewPM(cfg)
	if err != nil {
		return nil, err
	}

	return &Importer{
		pkgs:    make(map[string]*gx.Dependency),
		gopath:  gp,
		pm:      pm,
		rewrite: rw,
	}, nil
}

func getGoPath() (string, error) {
	gopath := os.Getenv("GOPATH")
	if gopath == "" {
		return "", errors.New("gopath not set")
	}
	return gopath, nil
}

// this function is an attempt to keep subdirectories of a package as part of
// the same logical gx package. It has a special case for golang.org/x/ packages
func getBaseDVCS(path string) string {
	parts := strings.Split(path, "/")
	depth := 3
	if parts[0] == "golang.org" && parts[1] == "x" {
		depth = 4
	}

	if len(parts) > depth {
		return strings.Join(parts[:3], "/")
	}
	return path
}

func (i *Importer) GxPublishGoPackage(imppath string) (*gx.Dependency, error) {
	imppath = getBaseDVCS(imppath)
	if d, ok := i.pkgs[imppath]; ok {
		return d, nil
	}

	// make sure its local
	err := GoGet(imppath)
	if err != nil {
		if !strings.Contains(err.Error(), "no buildable Go source files") {
			return nil, err
		}
	}

	pkgpath := path.Join(i.gopath, "src", imppath)
	pkgFilePath := path.Join(pkgpath, gx.PkgFileName)
	pkg, err := LoadPackageFile(pkgFilePath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, err
		}

		// init as gx package
		parts := strings.Split(imppath, "/")
		pkgname := parts[len(parts)-1]
		if !i.yesall {
			p := fmt.Sprintf("enter name for import '%s'", imppath)
			nname, err := prompt(p, pkgname)
			if err != nil {
				return nil, err
			}

			pkgname = nname
		}

		err = i.pm.InitPkg(pkgpath, pkgname, "go")
		if err != nil {
			return nil, err
		}

		pkg, err = LoadPackageFile(pkgFilePath)
		if err != nil {
			return nil, err
		}
	}

	// recurse!
	depsToVendor, err := i.depsToVendorForPackage(imppath)
	if err != nil {
		return nil, err
	}

	for n, child := range depsToVendor {
		Log("- processing dep %s for %s [%d / %d]", child, imppath, n+1, len(depsToVendor))
		if strings.HasPrefix(child, imppath) {
			continue
		}
		childdep, err := i.GxPublishGoPackage(child)
		if err != nil {
			return nil, err
		}

		pkg.Dependencies = append(pkg.Dependencies, childdep)
	}

	err = gx.SavePackageFile(pkg, pkgFilePath)
	if err != nil {
		return nil, err
	}

	if i.rewrite {
		fullpkgpath, err := filepath.Abs(pkgpath)
		if err != nil {
			return nil, err
		}

		err = i.rewriteImports(fullpkgpath)
		if err != nil {
			return nil, err
		}
	}

	hash, err := i.pm.PublishPackage(pkgpath, &pkg.PackageBase)
	if err != nil {
		return nil, err
	}

	Log("published %s as %s", imppath, hash)

	dep := &gx.Dependency{
		Hash:    hash,
		Name:    pkg.Name,
		Version: pkg.Version,
	}
	i.pkgs[imppath] = dep
	return dep, nil
}

func (i *Importer) depsToVendorForPackage(path string) ([]string, error) {
	rdeps := make(map[string]struct{})
	gopkg, err := build.Import(path, "", 0)
	if err != nil {
		_, ok := err.(*build.NoGoError)
		if !ok {
			return nil, err
		}
		// if theres no go code here, there still might be some in lower directories
	} else {
		// if the package existed and has go code in it
		for _, child := range gopkg.Imports {
			if pathIsNotStdlib(child) && !strings.HasPrefix(child, path) {
				rdeps[child] = struct{}{}
			}
		}
	}

	dirents, err := ioutil.ReadDir(filepath.Join(i.gopath, "src", path))
	if err != nil {
		return nil, err
	}

	for _, e := range dirents {
		if !e.IsDir() || skipDir(e.Name()) {
			continue
		}

		out, err := i.depsToVendorForPackage(filepath.Join(path, e.Name()))
		if err != nil {
			return nil, err
		}

		for _, o := range out {
			rdeps[o] = struct{}{}
		}
	}

	var depsToVendor []string
	for d, _ := range rdeps {
		depsToVendor = append(depsToVendor, d)
	}

	return depsToVendor, nil
}

func skipDir(name string) bool {
	switch name {
	case "Godeps", "vendor", ".git":
		return true
	default:
		return false
	}
}

func (i *Importer) rewriteImports(pkgpath string) error {

	filter := func(p string) bool {
		return !strings.HasPrefix(p, "vendor") &&
			!strings.HasPrefix(p, ".git") &&
			strings.HasSuffix(p, ".go")
	}

	rwf := func(in string) string {
		dep, ok := i.pkgs[in]
		if !ok {
			return in
		}

		return dep.Hash + "/" + dep.Name
	}

	return rw.RewriteImports(pkgpath, rwf, filter)
}

// TODO: take an option to grab packages from local GOPATH
func GoGet(path string) error {
	out, err := exec.Command("go", "get", path).CombinedOutput()
	if err != nil {
		return fmt.Errorf("go get failed: %s - %s", string(out), err)
	}
	return nil
}
// Package monoimporter provides a monorepo-compatible types.Importer for Go
// packages.
package monoimporter

import (
	"archive/zip"
	"fmt"
	"go/ast"
	"go/build"
	"go/token"
	"go/types"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/u-root/gobusybox/src/pkg/bb/bbinternal"
	"golang.org/x/tools/go/gcexportdata"
	"golang.org/x/tools/go/packages"
)

type finder interface {
	findAndOpen(pkg string) io.ReadCloser
}

func find(finders []finder, pkg string) io.ReadCloser {
	for _, f := range finders {
		if f == nil {
			continue
		}
		if file := f.findAndOpen(pkg); file != nil {
			return file
		}
	}
	return nil
}

// zipReader exists only for the internal blaze Go toolchain, which is packaged
// in a big zip.
type zipReader struct {
	ctxt   build.Context
	stdlib *zip.Reader
	files  map[string]*zip.File
}

func newZipReader(stdlib *zip.Reader, ctxt build.Context) *zipReader {
	z := &zipReader{
		stdlib: stdlib,
		files:  make(map[string]*zip.File),
		ctxt:   ctxt,
	}
	for _, file := range z.stdlib.File {
		z.files[file.Name] = file
	}
	return z
}

// goEnvDir is the Go build context directory name used by
// blaze/bazel/buck/Go.
//
// GOOS_GOARCH[_InstallSuffix], e.g. linux_amd64 or linux_amd64_pure.
func goEnvDir(ctxt build.Context) string {
	var suffix string
	if len(ctxt.InstallSuffix) > 0 {
		suffix = fmt.Sprintf("_%s", ctxt.InstallSuffix)
	}
	return fmt.Sprintf("%s_%s%s", ctxt.GOOS, ctxt.GOARCH, suffix)
}

func (z *zipReader) findAndOpen(pkg string) io.ReadCloser {
	pkg = strings.TrimPrefix(pkg, "google3/")
	name := fmt.Sprintf("%s/%s.x", goEnvDir(z.ctxt), pkg)
	f, ok := z.files[name]
	if !ok {
		return nil
	}
	rc, err := f.Open()
	if err != nil {
		return nil
	}
	return rc
}

// stdlibArchives is for bazel Go standard library archive files.
type stdlibArchives struct {
	ctxt build.Context

	// archs is a list of either directory containing Go .a archives, or .a
	// archive file paths.
	//
	// When target==default GOARCH/GOOS/cgo config, bazel will pass a list
	// of stdlib .a archive file paths.
	//
	// When target is not the default Go env, bazel will likely pass just a
	// stdlib directory path.
	//
	// (This is just from observation. I'm not 100% sure.)
	archs []string
}

// bazel Starlark rules pass stdlib .a files one of two ways: either as a list
// of individual files (e.g. linux_amd64/math/rand.a), or as just a directory
// name.
func (a stdlibArchives) findAndOpen(pkg string) io.ReadCloser {
	// bazel stdlib archives should be found using this method.
	//
	// bazel prefers .a files.
	name := fmt.Sprintf("/%s/%s.a", goEnvDir(a.ctxt), pkg)

	for _, filename := range a.archs {
		// If bazel passed just a directory name, this should find the
		// file.
		if fi, err := os.Stat(filename); err == nil && fi.IsDir() {
			f, err := os.Open(filepath.Join(filename, name))
			if err == nil {
				return f
			}
		}
		// If bazel passed individual file names, check the suffix.
		if strings.HasSuffix(filename, name) {
			ar, err := os.Open(filename)
			if err == nil {
				return ar
			}
		}
	}
	return nil
}

// unmappedArchives is for blaze non-stdlib dependencies, whose file path lets
// us derive their Go import path.
type unmappedArchives struct {
	// archs is a list .a archive file paths, where import path == package
	// path.
	archs []string
}

func (a unmappedArchives) findAndOpen(pkg string) io.ReadCloser {
	pkg = strings.TrimPrefix(pkg, "google3/")

	// in blaze, pkg path == file path, and we prefer .x
	//
	// I honestly do not know why .a files work in bazel, but not
	// in blaze. I don't even know the difference.
	name := fmt.Sprintf("/%s.x", pkg)

	for _, filename := range a.archs {
		if strings.HasSuffix(filename, name) {
			ar, err := os.Open(filename)
			if err == nil {
				return ar
			}
		}
	}
	return nil
}

// mappedArchives is for bazel non-stdlib dependencies.
//
// bazel Starlark rules pass a list of goImportPath:goArchiveFile.
type mappedArchives struct {
	// pkgs is a map of Go import path -> archive file.
	//
	// While in blaze, importPath == file path, in bazel each package gets
	// to define its own import path.
	pkgs map[string]string
}

// bazel Starlark rules pass a list of Go import path -> archive file path.
func (a mappedArchives) findAndOpen(pkg string) io.ReadCloser {
	// In bazel, non-stdlib dependencies should be found through this,
	// because we pass an explicit map of import path -> archive path from
	// the Starlark rules.
	if filename, ok := a.pkgs[pkg]; ok {
		f, err := os.Open(filename)
		if err == nil {
			return f
		}
	}
	return nil
}

// Importer implements a go/types.Importer for bazel-like monorepo build
// systems for Go packages.
//
// While open source Go depends on GOPATH and GOROOT to find packages,
// bazel-like build systems such as blaze or buck rely on a monorepo-style
// package search instead of using GOPATH and standard library packages are
// found in a zipped archive instead of GOROOT.
type Importer struct {
	fset *token.FileSet

	// imports is a cache of imported packages.
	imports map[string]*types.Package

	mapped   *mappedArchives
	unmapped *unmappedArchives
	stdlib   *stdlibArchives

	// stdlibZip is an archive reader for standard library package object
	// files.
	stdlibZip *zipReader
}

// NewFromZips returns a new monorepo importer, using the build context to pick
// the desired standard library zip archive.
//
// zips refers to zip file paths with Go standard library object files.
//
// archives refers to directories in which to find compiled Go package object files.
func NewFromZips(ctxt build.Context, unmappedArchs, mappedArchs, stdlibArchs, stdlibZips []string) (*Importer, error) {
	// Some architectures have extra stuff after the GOARCH in the stdlib filename.
	ctxtWithWildcard := ctxt
	ctxtWithWildcard.GOARCH += "*"

	var stdlib *zip.Reader
	// blaze may pass more than one zip file, each for different environments.
	wantPattern := fmt.Sprintf("%s.x.zip", goEnvDir(ctxtWithWildcard))
	for _, dir := range stdlibZips {
		if matched, err := filepath.Match(wantPattern, filepath.Base(dir)); err != nil {
			log.Printf("Error with pattern %q: %v", wantPattern, err)
		} else if matched {
			stdlibZ, err := zip.OpenReader(dir)
			if err != nil {
				return nil, err
			}
			stdlib = &stdlibZ.Reader
			break
		}
	}

	ma := &mappedArchives{
		pkgs: make(map[string]string),
	}
	for _, archive := range mappedArchs {
		nameAndFile := strings.Split(archive, ":")
		if len(nameAndFile) != 2 {
			return nil, fmt.Errorf("archive %q is not goImportPath:goArchiveFilePath", nameAndFile)
		}
		ma.pkgs[nameAndFile[0]] = nameAndFile[1]
	}
	sa := &stdlibArchives{
		ctxt:  ctxt,
		archs: stdlibArchs,
	}
	ua := &unmappedArchives{archs: unmappedArchs}

	return New(ctxt, ua, ma, sa, stdlib), nil
}

// New returns a new monorepo importer.
func New(ctxt build.Context, ua *unmappedArchives, ma *mappedArchives, sa *stdlibArchives, stdlibZip *zip.Reader) *Importer {
	i := &Importer{
		imports: map[string]*types.Package{
			"unsafe": types.Unsafe,
		},
		fset:     token.NewFileSet(),
		mapped:   ma,
		stdlib:   sa,
		unmapped: ua,
	}
	if stdlibZip != nil {
		i.stdlibZip = newZipReader(stdlibZip, ctxt)
	}
	return i
}

// Import implements types.Importer.Import.
func (i *Importer) Import(importPath string) (*types.Package, error) {
	if pkg, ok := i.imports[importPath]; ok && pkg.Complete() {
		return pkg, nil
	}

	var finders []finder
	// stdlibZip and mapped do exact file matching based on
	// importPath, they never return the wrong package.
	if i.stdlibZip != nil {
		finders = append(finders, i.stdlibZip)
	}
	if i.mapped != nil {
		finders = append(finders, i.mapped)
	}
	// stdlib and unmapped match based on file path suffix, so they
	// may return the wrong package. Match to stdlib first, because
	// stdlib contains shorter paths. (E.g. "errors" can match to
	// either "errors" in stdlib or "golang.org/x/crypto/errors"
	// but it should match to stdlib)
	if i.stdlib != nil {
		finders = append(finders, i.stdlib)
	}
	if i.unmapped != nil {
		finders = append(finders, i.unmapped)
	}
	file := find(finders, importPath)
	if file == nil {
		return nil, fmt.Errorf("package %q not found", importPath)
	}
	defer file.Close()

	r, err := gcexportdata.NewReader(file)
	if err != nil {
		return nil, err
	}
	return gcexportdata.Read(r, i.fset, i.imports, importPath)
}

// Load loads a google3 package.
func Load(pkgPath string, filepaths []string, importer types.Importer) (*packages.Package, error) {
	p := &packages.Package{
		PkgPath: pkgPath,
	}

	// If go_binary, bla, if go_library, bla
	fset, astFiles, parsedFileNames, err := bbinternal.ParseAST("main", filepaths)
	if err != nil {
		return nil, err
	}

	p.Fset = fset
	p.Syntax = astFiles
	p.CompiledGoFiles = parsedFileNames
	p.GoFiles = filepaths

	// Type-check the package before we continue. We need types to rewrite
	// some statements.
	conf := types.Config{
		Importer: importer,

		// We only need global declarations' types.
		IgnoreFuncBodies: true,
	}

	p.TypesInfo = &types.Info{
		// If you don't make these maps before passing TypesInfo to
		// Check, they won't be filled in.
		Types:  make(map[ast.Expr]types.TypeAndValue),
		Scopes: make(map[ast.Node]*types.Scope),
	}
	// It's important that p.Syntax be in the same order every time for
	// p.TypesInfo to be stable.
	tpkg, err := conf.Check(pkgPath, p.Fset, p.Syntax, p.TypesInfo)
	if err != nil {
		return nil, fmt.Errorf("type checking failed: %v", err)
	}
	p.Types = tpkg
	return p, nil
}

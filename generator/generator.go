package generator

import (
	"bytes"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"go/ast"
	"go/token"
	"io"
	"text/template"

	"github.com/pkg/errors"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/imports"

	"github.com/hexdigest/gowrap/pkg"
	"github.com/hexdigest/gowrap/printer"
)

// Generator generates decorators for the interface types
type Generator struct {
	Options

	headerTemplate *template.Template
	bodyTemplate   *template.Template
	srcPackage     *packages.Package
	dstPackage     *packages.Package
	methods        methodsList
	interfaceType  string
	genericsTypes  string
	genericsParams string
	localPrefix    string
}

// TemplateInputs information passed to template for generation
type TemplateInputs struct {
	// Interface information for template
	Interface TemplateInputInterface
	// Vars additional vars to pass to the template, see Options.Vars
	Vars    map[string]interface{}
	Imports []string
}

// Import generates an import statement using a list of imports from the source file
// along with the ones from the template itself
func (t TemplateInputs) Import(imports ...string) string {
	allImports := make(map[string]struct{}, len(imports)+len(t.Imports))

	for _, i := range t.Imports {
		allImports[strings.TrimSpace(i)] = struct{}{}
	}

	for _, i := range imports {
		if len(i) == 0 {
			continue
		}

		i = strings.TrimSpace(i)

		if i[len(i)-1] != '"' {
			i += `"`
		}

		if i[0] != '"' {
			i = `"` + i
		}

		allImports[i] = struct{}{}
	}

	out := make([]string, 0, len(allImports))

	for i := range allImports {
		out = append(out, i)
	}

	sort.Strings(out)

	return "import (\n" + strings.Join(out, "\n") + ")\n"
}

// TemplateInputInterface subset of interface information used for template generation
type TemplateInputInterface struct {
	Name string
	// Type of the interface, with package name qualifier (e.g. sort.Interface)
	Type string
	// Generics of the interface when using generics
	Generics TemplateInputGenerics
	// Methods name keyed map of method information
	Methods map[string]Method
}

type methodsList map[string]Method

// Options of the NewGenerator constructor
type Options struct {
	//InterfaceName is a name of interface type
	InterfaceName string

	//Imports from the file with interface definition
	Imports []string

	//SourcePackage is an import path or a relative path of the package that contains the source interface
	SourcePackage string

	//SourcePackageAlias is an import selector defauls is source package name
	SourcePackageAlias string

	//OutputFile name which is used to detect destination package name and also to fix imports in the resulting source
	OutputFile string

	//HeaderTemplate is used to generate package clause and comment over the generated source
	HeaderTemplate string

	//BodyTemplate generates import section, decorator constructor and methods
	BodyTemplate string

	//Vars additional vars that are passed to the templates from the command line
	Vars map[string]interface{}

	//HeaderVars header specific variables
	HeaderVars map[string]interface{}

	//Funcs is a map of helper functions that can be used within a template
	Funcs template.FuncMap

	//LocalPrefix is a comma-separated string of import path prefixes, which, if set, instructs Process to sort the import
	//paths with the given prefixes into another group after 3rd-party packages.
	LocalPrefix string
}

var errEmptyInterface = errors.New("interface has no methods")
var errUnexportedMethod = errors.New("unexported method")

// NewGenerator returns Generator initialized with options
func NewGenerator(options Options) (*Generator, error) {
	if options.Funcs == nil {
		options.Funcs = make(template.FuncMap)
	}

	headerTemplate, err := template.New("header").Funcs(options.Funcs).Parse(options.HeaderTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse header template")
	}

	bodyTemplate, err := template.New("body").Funcs(options.Funcs).Parse(options.BodyTemplate)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse body template")
	}

	if options.Vars == nil {
		options.Vars = make(map[string]interface{})
	}

	fs := token.NewFileSet()

	srcPackage, err := pkg.Load(options.SourcePackage)
	if err != nil {
		return nil, errors.Wrap(err, "failed to load source package")
	}

	dstPackagePath := filepath.Dir(options.OutputFile)
	if !strings.HasPrefix(dstPackagePath, "/") && !strings.HasPrefix(dstPackagePath, "./") {
		dstPackagePath = "./" + dstPackagePath
	}

	dstPackage, err := loadDestinationPackage(dstPackagePath)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to load destination package: %s", dstPackagePath)
	}

	srcPackageAST, err := pkg.AST(fs, srcPackage)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse source package")
	}

	interfaceType := srcPackage.Name + "." + options.InterfaceName
	if srcPackage.PkgPath == dstPackage.PkgPath {
		interfaceType = options.InterfaceName
		srcPackageAST.Name = ""
	} else {
		if options.SourcePackageAlias != "" {
			srcPackageAST.Name = options.SourcePackageAlias
		}

		options.Imports = append(options.Imports, `"`+srcPackage.PkgPath+`"`)
	}

	types, methods, imports, err := findInterface(fs, srcPackage, srcPackageAST, options.InterfaceName, nil)
	if err != nil {
		return nil, errors.Wrap(err, "failed to parse interface declaration")
	}
	genericsTypes, genericsParams := types.buildVars()

	if len(methods) == 0 {
		return nil, errEmptyInterface
	}

	for _, m := range methods {
		if srcPackageAST.Name != "" && []rune(m.Name)[0] == []rune(strings.ToLower(m.Name))[0] {
			return nil, errors.Wrap(errUnexportedMethod, m.Name)
		}
	}

	options.Imports = append(options.Imports, makeImports(imports)...)

	return &Generator{
		Options:        options,
		headerTemplate: headerTemplate,
		bodyTemplate:   bodyTemplate,
		srcPackage:     srcPackage,
		dstPackage:     dstPackage,
		interfaceType:  interfaceType,
		genericsTypes:  genericsTypes,
		genericsParams: genericsParams,
		methods:        methods,
		localPrefix:    options.LocalPrefix,
	}, nil
}

func makeImports(imports []*ast.ImportSpec) []string {
	result := make([]string, len(imports))
	for _, i := range imports {
		var name string
		if i.Name != nil {
			name = i.Name.Name
		}
		result = append(result, name+" "+i.Path.Value)
	}

	return result
}

func loadDestinationPackage(path string) (*packages.Package, error) {
	dstPackage, err := pkg.Load(path)
	if err != nil {
		//using directory name as a package name
		dstPackage, err = makePackage(path)
	}

	return dstPackage, err
}

var errNoPackageName = errors.New("failed to determine the destination package name")

func makePackage(path string) (*packages.Package, error) {
	name := filepath.Base(path)
	if name == string(filepath.Separator) || name == "." {
		return nil, errNoPackageName
	}

	return &packages.Package{
		Name: name,
	}, nil
}

// Generate generates code using header and body templates
func (g Generator) Generate(w io.Writer) error {
	buf := bytes.NewBuffer([]byte{})

	err := g.headerTemplate.Execute(buf, map[string]interface{}{
		"SourcePackage": g.srcPackage,
		"Package":       g.dstPackage,
		"Vars":          g.Options.Vars,
		"Options":       g.Options,
	})
	if err != nil {
		return err
	}

	err = g.bodyTemplate.Execute(buf, TemplateInputs{
		Interface: TemplateInputInterface{
			Name: g.Options.InterfaceName,
			Generics: TemplateInputGenerics{
				Types:  g.genericsTypes,
				Params: g.genericsParams,
			},
			Type:    g.interfaceType,
			Methods: g.methods,
		},
		Imports: g.Options.Imports,
		Vars:    g.Options.Vars,
	})
	if err != nil {
		return err
	}

	imports.LocalPrefix = g.localPrefix
	processedSource, err := imports.Process(g.Options.OutputFile, buf.Bytes(), nil)
	if err != nil {
		return errors.Wrapf(err, "failed to format generated code:\n%s", buf)
	}

	_, err = w.Write(processedSource)
	return err
}

var errInterfaceNotFound = errors.New("interface type declaration not found")

// findInterface looks for the interface declaration in the given directory
// and returns the generic params if exists, a list of the interface's methods, and a list of imports from the file
// where interface type declaration was found
func findInterface(fs *token.FileSet, currentPackage *packages.Package, p *ast.Package, interfaceName string, genericParams genericParams) (genericTypes genericTypes, methods methodsList, imports []*ast.ImportSpec, err error) {
	//looking for the source interface declaration in all files in the dir
	//while doing this we also store all found type declarations to check if some of the
	//interface methods use unexported types
	ts, imports, types := iterateFiles(p, interfaceName)
	if ts == nil {
		return nil, nil, nil, errors.Wrap(errInterfaceNotFound, interfaceName)
	}

	genericTypes = genericTypesBuild(ts)

	if it, ok := ts.Type.(*ast.InterfaceType); ok {
		methods, err = processInterface(fs, currentPackage, it, types, p.Name, imports, genericTypes, genericParams)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	return genericTypes, methods, imports, err
}

func iterateFiles(p *ast.Package, name string) (selectedType *ast.TypeSpec, imports []*ast.ImportSpec, types []*ast.TypeSpec) {
	for _, f := range p.Files {
		if f != nil {
			for _, ts := range typeSpecs(f) {
				types = append(types, ts)
				if ts.Name.Name == name {
					selectedType = ts
					imports = f.Imports
					return
				}
			}
		}
	}
	return
}

func typeSpecs(f *ast.File) []*ast.TypeSpec {
	result := []*ast.TypeSpec{}

	for _, decl := range f.Decls {
		if gd, ok := decl.(*ast.GenDecl); ok && gd.Tok == token.TYPE {
			for _, spec := range gd.Specs {
				if ts, ok := spec.(*ast.TypeSpec); ok {
					result = append(result, ts)
				}
			}
		}
	}

	return result
}

func getEmbeddedMethods(t ast.Expr, fs *token.FileSet, currentPackage *packages.Package, types []*ast.TypeSpec, pr typePrinter, typesPrefix string, imports []*ast.ImportSpec, params genericParams) (param genericParam, methods methodsList, err error) {
	switch v := t.(type) {
	case *ast.SelectorExpr:
		if x, ok := v.X.(*ast.Ident); ok && x != nil {
			param.Name, err = pr.PrintType(x)
			if err != nil {
				return
			}
		}

		methods, err = processSelector(fs, currentPackage, v, imports, params)
		return

	case *ast.Ident:
		param.Name, err = pr.PrintType(v)
		if err != nil {
			return
		}
		methods, err = processIdent(fs, currentPackage, v, types, typesPrefix, imports, params)
		return
	}
	return
}

func processEmbedded(t ast.Expr, fs *token.FileSet, currentPackage *packages.Package, types []*ast.TypeSpec, pr typePrinter, typesPrefix string, imports []*ast.ImportSpec, genericParams genericParams) (genericParam genericParam, embeddedMethods methodsList, err error) {
	var x ast.Expr
	var hasGenericsParams bool

	switch v := t.(type) {
	case *ast.IndexExpr:
		x = v.X
		hasGenericsParams = true

		genericParam, _, err = processEmbedded(v.Index, fs, currentPackage, types, pr, typesPrefix, imports, genericParams)
		if err != nil {
			return
		}
		if genericParam.Name != "" {
			genericParams = append(genericParams, genericParam)
		}

	case *ast.IndexListExpr:
		x = v.X
		hasGenericsParams = true

		if v.Indices != nil {
			for _, index := range v.Indices {
				genericParam, _, err = processEmbedded(index, fs, currentPackage, types, pr, typesPrefix, imports, genericParams)
				if err != nil {
					return
				}
				if genericParam.Name != "" {
					genericParams = append(genericParams, genericParam)
				}
			}
		}
	default:
		x = v
	}

	genericParam, embeddedMethods, err = getEmbeddedMethods(x, fs, currentPackage, types, pr, typesPrefix, imports, genericParam.Params)
	if err != nil {
		return
	}

	if hasGenericsParams {
		genericParam.Params = genericParam.Params
	}
	return
}

func processInterface(fs *token.FileSet, currentPackage *packages.Package, it *ast.InterfaceType, types []*ast.TypeSpec, typesPrefix string, imports []*ast.ImportSpec, genericsTypes genericTypes, genericParams genericParams) (methods methodsList, err error) {
	if it.Methods == nil {
		return nil, nil
	}

	methods = make(methodsList, len(it.Methods.List))

	pr := printer.New(fs, types, typesPrefix)

	for _, field := range it.Methods.List {
		var embeddedMethods methodsList
		var err error

		switch v := field.Type.(type) {
		case *ast.FuncType:
			var method *Method

			method, err = NewMethod(field.Names[0].Name, field, pr, genericsTypes, genericParams)
			if err == nil {
				methods[field.Names[0].Name] = *method
				continue
			}

		default:
			_, embeddedMethods, err = processEmbedded(v, fs, currentPackage, types, pr, typesPrefix, imports, genericParams)
		}

		if err != nil {
			return nil, err
		}

		methods, err = mergeMethods(methods, embeddedMethods)
		if err != nil {
			return nil, err
		}
	}

	return methods, nil
}

func processSelector(fs *token.FileSet, currentPackage *packages.Package, se *ast.SelectorExpr, imports []*ast.ImportSpec, genericParams genericParams) (methodsList, error) {
	selectedName := se.Sel.Name
	packageSelector := se.X.(*ast.Ident).Name

	importPath, err := findImportPathForName(packageSelector, imports, currentPackage)
	if err != nil {
		return nil, errors.Wrapf(err, "unable to find package %s", packageSelector)
	}

	p, ok := currentPackage.Imports[importPath]
	if !ok {
		return nil, fmt.Errorf("unable to find package %s", packageSelector)
	}

	astPkg, err := pkg.AST(fs, p)
	if err != nil {
		return nil, errors.Wrap(err, "failed to import package")
	}

	_, methods, _, err := findInterface(fs, p, astPkg, selectedName, genericParams)

	return methods, err
}

// mergeMethods merges two methods list. Retains overlapping methods from the
// parent list
func mergeMethods(methods, embeddedMethods methodsList) (methodsList, error) {
	if methods == nil || embeddedMethods == nil {
		return methods, nil
	}

	result := make(methodsList, len(methods)+len(embeddedMethods))
	for name, signature := range embeddedMethods {
		result[name] = signature
	}

	for name, signature := range methods {
		result[name] = signature
	}

	return result, nil
}

var errNotAnInterface = errors.New("embedded type is not an interface")

func processIdent(fs *token.FileSet, currentPackage *packages.Package, i *ast.Ident, types []*ast.TypeSpec, typesPrefix string, imports []*ast.ImportSpec, genericParams genericParams) (methodsList, error) {
	var embeddedInterface *ast.InterfaceType
	var genericsTypes genericTypes
	for _, t := range types {
		if t.Name.Name == i.Name {
			var ok bool
			embeddedInterface, ok = t.Type.(*ast.InterfaceType)
			if !ok {
				return nil, errors.Wrap(errNotAnInterface, t.Name.Name)
			}

			genericsTypes = genericTypesBuild(t)
			break
		}
	}

	if embeddedInterface == nil {
		return nil, nil
	}

	return processInterface(fs, currentPackage, embeddedInterface, types, typesPrefix, imports, genericsTypes, genericParams)
}

var errUnknownSelector = errors.New("unknown selector")

func findImportPathForName(name string, imports []*ast.ImportSpec, currentPackage *packages.Package) (string, error) {
	for _, i := range imports {
		if i.Name != nil && i.Name.Name == name {
			return unquote(i.Path.Value), nil
		}
	}

	for path, pkg := range currentPackage.Imports {
		if pkg.Name == name {
			return path, nil
		}
	}

	return "", errors.Wrapf(errUnknownSelector, name)
}

func unquote(s string) string {
	if s[0] == '"' {
		s = s[1:]
	}

	if s[len(s)-1] == '"' {
		s = s[0 : len(s)-1]
	}

	return s
}

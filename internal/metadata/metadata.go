package metadata

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"maps"
	"sort"
	"strings"
)

const MainFunc = "main"

// CallIdentifierType represents different types of identifiers used in the call graph
type CallIdentifierType int

const (
	// BaseID - Function/method name with package, no position or generics
	BaseID CallIdentifierType = iota
	// GenericID - Includes generic type parameters but no position
	GenericID
	// InstanceID - Includes position and generic type parameters for specific call instances
	InstanceID
)

// CallIdentifier manages different identifier formats for calls
type CallIdentifier struct {
	pkg      string
	name     string
	recvType string
	position string
	generics map[string]string
}

func NewCallIdentifier(pkg, name, recvType, position string, generics map[string]string) *CallIdentifier {
	return &CallIdentifier{
		pkg:      pkg,
		name:     name,
		recvType: recvType,
		position: position,
		generics: generics,
	}
}

// ID returns the identifier based on the specified type
func (ci *CallIdentifier) ID(idType CallIdentifierType) string {
	var base string

	// Build base identifier
	if ci.recvType != "" {
		if strings.HasPrefix(ci.recvType, "*") {
			base = fmt.Sprintf("%s.%s.%s", ci.pkg, ci.recvType[1:], ci.name)
		} else {
			base = fmt.Sprintf("%s.%s.%s", ci.pkg, ci.recvType, ci.name)
		}
	} else {
		base = fmt.Sprintf("%s.%s", ci.pkg, ci.name)
	}
	base = strings.TrimPrefix(base, "*")

	switch idType {
	case BaseID:
		return base
	case GenericID:
		// Include generics but no position
		if len(ci.generics) > 0 {
			var genericParts []string
			for param, concrete := range ci.generics {
				genericParts = append(genericParts, fmt.Sprintf("%s=%s", param, concrete))
			}
			sort.Slice(genericParts, func(i, j int) bool { return genericParts[i] < genericParts[j] })
			return fmt.Sprintf("%s[%s]", base, strings.Join(genericParts, ","))
		}
		return base
	case InstanceID:
		// Include generics and position for instance identification
		var parts []string
		parts = append(parts, base)

		if len(ci.generics) > 0 {
			var genericParts []string
			for param, concrete := range ci.generics {
				genericParts = append(genericParts, fmt.Sprintf("%s=%s", param, concrete))
			}
			sort.Slice(genericParts, func(i, j int) bool { return genericParts[i] < genericParts[j] })
			parts = append(parts, fmt.Sprintf("[%s]", strings.Join(genericParts, ",")))
		}

		if ci.position != "" {
			parts = append(parts, fmt.Sprintf("@%s", ci.position))
		}

		id := strings.Join(parts, "")
		id = strings.TrimPrefix(id, "*")

		return id
	default:
		return base
	}
}

// Helper function to strip ID to base format
func stripToBase(id string) string {
	// Remove position (@...)
	if idx := strings.Index(id, "@"); idx >= 0 {
		id = id[:idx]
	}
	// Remove generics ([...])
	if idx := strings.Index(id, "["); idx >= 0 {
		id = id[:idx]
	}
	return id
}

var assignmentCount int
var processAssignmentCount int

// GenerateMetadata extracts all metadata and call graph info
func GenerateMetadata(pkgs map[string]map[string]*ast.File, fileToInfo map[*ast.File]*types.Info, importPaths map[string]string, fset *token.FileSet) *Metadata {
	funcMap := BuildFuncMap(pkgs)

	fmt.Println("funcMap Count:", len(funcMap))

	metadata := &Metadata{
		StringPool: NewStringPool(),
		Packages:   make(map[string]*Package),
		CallGraph:  make([]CallGraphEdge, 0),
	}

	for pkgName, files := range pkgs {
		pkg := &Package{
			Files: make(map[string]*File),
		}

		// Collect methods for types
		allTypeMethods := make(map[string][]Method)
		allTypes := make(map[string]*Type)

		// First pass: collect all methods
		for fileName, file := range files {
			info := fileToInfo[file]
			for _, decl := range file.Decls {
				fn, ok := decl.(*ast.FuncDecl)
				if !ok || fn.Recv == nil || len(fn.Recv.List) == 0 {
					continue
				}
				recvType := getTypeName(fn.Recv.List[0].Type)

				// Extract type parameter names for generics
				typeParams := []string{}
				if fn.Type != nil && fn.Type.TypeParams != nil {
					for _, tparam := range fn.Type.TypeParams.List {
						for _, name := range tparam.Names {
							typeParams = append(typeParams, name.Name)
						}
					}
				}

				// Extract return value origins
				var returnVars []CallArgument
				var maxReturnCount int

				if fn.Body != nil {
					ast.Inspect(fn.Body, func(n ast.Node) bool {
						ret, ok := n.(*ast.ReturnStmt)
						if !ok {
							return true
						}

						// Track the maximum number of return values seen
						if len(ret.Results) > maxReturnCount {
							maxReturnCount = len(ret.Results)
							returnVars = nil // Clear and rebuild with the most complete return
							for _, expr := range ret.Results {
								returnVars = append(returnVars, *ExprToCallArgument(expr, info, pkgName, fset, metadata))
							}
						}

						return true // Continue traversal to see all returns
					})
				}

				// Use funcMap to get callee function declaration
				var assignmentsInFunc = make(map[string][]Assignment)

				ast.Inspect(fn, func(nd ast.Node) bool {
					switch expr := nd.(type) {
					case *ast.AssignStmt:
						assignments := processAssignment(expr, file, info, pkgName, fset, fileToInfo, funcMap, metadata)
						processAssignmentCount++
						for _, assign := range assignments {
							varName := metadata.StringPool.GetString(assign.VariableName)
							assignmentsInFunc[varName] = append(assignmentsInFunc[varName], assign)
						}
					}
					return true
				})

				m := Method{
					Name:          metadata.StringPool.Get(fn.Name.Name),
					Receiver:      metadata.StringPool.Get(recvType),
					Signature:     *ExprToCallArgument(fn.Type, info, pkgName, fset, metadata),
					Position:      metadata.StringPool.Get(getFuncPosition(fn, fset)),
					Scope:         metadata.StringPool.Get(getScope(fn.Name.Name)),
					AssignmentMap: assignmentsInFunc,
					TypeParams:    typeParams,
					ReturnVars:    returnVars,
					Filename:      metadata.StringPool.Get(fileName),
				}
				m.SignatureStr = metadata.StringPool.Get(CallArgToString(m.Signature))
				allTypeMethods[recvType] = append(allTypeMethods[recvType], m)
			}
		}

		// Second pass: process each file
		for fileName, file := range files {
			info := fileToInfo[file]
			fullPath := buildFullPath(importPaths[pkgName], fileName)

			f := &File{
				Types:           make(map[string]*Type),
				Functions:       make(map[string]*Function),
				Variables:       make(map[string]*Variable),
				StructInstances: make([]StructInstance, 0),
				Imports:         make(map[int]int),
			}

			// Collect constants for this file
			constMap := collectConstants(file, info, pkgName, fset, metadata)

			// Process types
			processTypes(file, info, pkgName, fset, f, allTypeMethods, allTypes, metadata)

			// Process functions
			processFunctions(file, info, pkgName, fset, f, fileToInfo, funcMap, metadata)

			// Process variables and constants
			processVariables(file, info, pkgName, fset, f, metadata)

			// Process struct instances and assignments
			processStructInstances(file, info, pkgName, fset, f, constMap, metadata)

			// Process imports
			processImports(file, metadata, f)

			pkg.Types = allTypes
			pkg.Files[fullPath] = f
		}

		metadata.Packages[pkgName] = pkg
	}

	// Analyze interface implementations
	analyzeInterfaceImplementations(metadata.Packages, metadata.StringPool)

	for pkgName, files := range pkgs {
		// Build call graph
		buildCallGraph(files, pkgs, pkgName, fileToInfo, fset, funcMap, metadata)
	}

	metadata.BuildCallGraphMaps()

	roots := metadata.CallGraphRoots()
	for _, edge := range roots {
		metadata.TraverseCallerChildren(edge, func(parent, child *CallGraphEdge) {
			if len(parent.TypeParamMap) > 0 && len(child.TypeParamMap) > 0 {
				newChild := *child
				newChild.TypeParamMap = map[string]string{}

				maps.Copy(newChild.TypeParamMap, child.TypeParamMap)
				// Add parent types
				maps.Copy(newChild.TypeParamMap, parent.TypeParamMap)

				// Reset id
				newChild.Caller.identifier = nil
				newChild.Caller.Edge = &newChild
				newChild.Caller.buildIdentifier()

				newChild.Callee.identifier = nil
				newChild.Callee.Edge = &newChild
				newChild.Callee.buildIdentifier()

				metadata.CallGraph = append(metadata.CallGraph, newChild)
				metadata.Callers[newChild.Caller.identifier.ID(BaseID)] = append(metadata.Callers[newChild.Caller.identifier.ID(BaseID)], &newChild)
			}
		})
	}

	// Process function return types to fill ResolvedType
	metadata.ProcessFunctionReturnTypes()

	// Finalize string pool
	metadata.StringPool.Finalize()

	fmt.Println("process assignment Count:", processAssignmentCount)
	fmt.Println("assignment Count:", assignmentCount)

	return metadata
}

// NEW: Enhanced metadata methods for tracker tree simplification

// GetArgumentProcessor returns the argument processor instance
func (m *Metadata) GetArgumentProcessor() *ArgumentProcessor {
	if m.argumentProcessor == nil {
		m.argumentProcessor = &ArgumentProcessor{
			argTypeCache:        make(map[string]ArgumentType),
			variableOriginCache: make(map[string]VariableOrigin),
			assignmentLinkCache: make(map[string][]AssignmentLink),
		}
	}
	return m.argumentProcessor
}

// GetGenericTypeResolver returns the generic type resolver instance
func (m *Metadata) GetGenericTypeResolver() *GenericTypeResolver {
	if m.genericResolver == nil {
		m.genericResolver = &GenericTypeResolver{
			typeParamCache:     make(map[string]map[string]string),
			compatibilityCache: make(map[string]bool),
		}
	}
	return m.genericResolver
}

// ClassifyArgument determines the type of an argument for enhanced processing
func (m *Metadata) ClassifyArgument(arg CallArgument) ArgumentType {
	switch arg.GetKind() {
	case KindCall:
		return ArgTypeFunctionCall
	case KindIdent:
		if strings.HasPrefix(arg.GetType(), "func(") {
			return ArgTypeFunctionCall
		}
		return ArgTypeVariable
	case KindLiteral:
		return ArgTypeLiteral
	case KindSelector:
		return ArgTypeSelector
	case KindUnary:
		return ArgTypeUnary
	case KindBinary:
		return ArgTypeBinary
	case KindIndex:
		return ArgTypeIndex
	case KindCompositeLit:
		return ArgTypeComposite
	case KindTypeAssert:
		return ArgTypeTypeAssert
	default:
		return ArgTypeComplex
	}
}

// ProcessArguments processes arguments with enhanced classification and tracking
func (m *Metadata) ProcessArguments(edge *CallGraphEdge, limits TrackerLimits) []*ProcessedArgument {
	var processed []*ProcessedArgument
	argCount := 0

	for i, arg := range edge.Args {
		if argCount >= limits.MaxArgsPerFunction {
			break
		}

		// Skip certain arguments
		if edge.Caller.ID() == stripToBase(arg.ID()) ||
			edge.Callee.ID() == arg.ID() ||
			arg.GetName() == "nil" ||
			arg.ID() == "" {
			continue
		}

		processedArg := &ProcessedArgument{
			Argument: &arg,
			Edge:     edge,
			ArgType:  m.ClassifyArgument(arg),
			ArgIndex: i,
			ArgContext: fmt.Sprintf("%s.%s",
				m.StringPool.GetString(edge.Caller.Name),
				m.StringPool.GetString(edge.Callee.Name)),
		}

		processed = append(processed, processedArg)
		argCount++
	}

	return processed
}

// BuildAssignmentRelationships builds assignment relationships for all call graph edges
func (m *Metadata) BuildAssignmentRelationships() map[AssignmentKey]*AssignmentLink {
	relationships := make(map[AssignmentKey]*AssignmentLink)

	for i := range m.CallGraph {
		edge := &m.CallGraph[i]

		callerName := m.StringPool.GetString(edge.Caller.Name)
		calleeName := m.StringPool.GetString(edge.Callee.Name)
		callerPkg := m.StringPool.GetString(edge.Caller.Pkg)

		// TODO: remove this
		_ = calleeName

		// Get root assignments
		if pkg, ok := m.Packages[callerPkg]; ok {
			for _, file := range pkg.Files {
				if fn, ok := file.Functions[callerName]; ok && callerName == MainFunc {
					for recvVarName, assigns := range fn.AssignmentMap {
						assignment := assigns[len(assigns)-1]

						if edge.CalleeRecvVarName != recvVarName {
							continue
						}

						akey := AssignmentKey{
							Name:      recvVarName,
							Pkg:       callerPkg,
							Type:      m.StringPool.GetString(assignment.ConcreteType),
							Container: callerName,
						}

						relationships[akey] = &AssignmentLink{
							AssignmentKey: akey,
							Assignment:    &assignment,
							Edge:          edge,
						}
					}
				}
			}
		}

		// Process assignments for this edge
		for recvVarName, assigns := range edge.AssignmentMap {
			assignment := assigns[len(assigns)-1] // Latest assignment

			akey := AssignmentKey{
				Name:      recvVarName,
				Pkg:       m.StringPool.GetString(assignment.Pkg),
				Type:      m.StringPool.GetString(assignment.ConcreteType),
				Container: m.StringPool.GetString(assignment.Func),
			}

			var assignmentEdge *CallGraphEdge = edge

			// Get nested edges to link to the assignment
			if callers, exists := m.Callers[edge.Callee.BaseID()]; exists {
				for _, nestedEdge := range callers {
					if nestedEdge.CalleeRecvVarName == recvVarName {
						assignmentEdge = nestedEdge
						break
					}
				}
			}

			relationships[akey] = &AssignmentLink{
				AssignmentKey: akey,
				Assignment:    &assignment,
				Edge:          assignmentEdge,
			}
		}
	}

	return relationships
}

// BuildVariableRelationships builds variable relationships for all call graph edges
func (m *Metadata) BuildVariableRelationships() map[ParamKey]*VariableLink {
	relationships := make(map[ParamKey]*VariableLink)

	for i := range m.CallGraph {
		edge := &m.CallGraph[i]

		for param, arg := range edge.ParamArgMap {
			originVar, originPkg, _, originFunc := TraceVariableOrigin(
				param,
				m.StringPool.GetString(edge.Callee.Name),
				m.StringPool.GetString(edge.Callee.Pkg),
				m,
			)

			if originVar == "" {
				continue
			}

			pkey := ParamKey{
				Name:      param,
				Pkg:       m.StringPool.GetString(edge.Callee.Pkg),
				Container: m.StringPool.GetString(edge.Callee.Name),
			}

			relationships[pkey] = &VariableLink{
				ParamKey:   pkey,
				OriginVar:  originVar,
				OriginPkg:  originPkg,
				OriginFunc: originFunc,
				Edge:       edge,
				Argument:   &arg,
			}
		}
	}

	return relationships
}

// GetAssignmentRelationships returns the cached assignment relationships
func (m *Metadata) GetAssignmentRelationships() map[AssignmentKey]*AssignmentLink {
	if m.assignmentRelationships == nil {
		m.assignmentRelationships = m.BuildAssignmentRelationships()
	}
	return m.assignmentRelationships
}

// GetVariableRelationships returns the cached variable relationships
func (m *Metadata) GetVariableRelationships() map[ParamKey]*VariableLink {
	if m.variableRelationships == nil {
		m.variableRelationships = m.BuildVariableRelationships()
	}
	return m.variableRelationships
}

// FindRelatedAssignments finds assignments related to a variable
func (m *Metadata) FindRelatedAssignments(varName, pkg, container string) []*AssignmentLink {
	akey := AssignmentKey{
		Name:      varName,
		Pkg:       pkg,
		Container: container,
	}

	if link, exists := m.GetAssignmentRelationships()[akey]; exists {
		return []*AssignmentLink{link}
	}

	// Find partial matches
	var matches []*AssignmentLink
	for key, link := range m.GetAssignmentRelationships() {
		if key.Name == varName && key.Pkg == pkg {
			matches = append(matches, link)
		}
	}

	return matches
}

// FindRelatedVariables finds variables related to a parameter
func (m *Metadata) FindRelatedVariables(varName, pkg, container string) []*VariableLink {
	pkey := ParamKey{
		Name:      varName,
		Pkg:       pkg,
		Container: container,
	}

	if link, exists := m.GetVariableRelationships()[pkey]; exists {
		return []*VariableLink{link}
	}

	// Find partial matches
	var matches []*VariableLink
	for key, link := range m.GetVariableRelationships() {
		if key.Name == varName && key.Pkg == pkg {
			matches = append(matches, link)
		}
	}

	return matches
}

// TraverseCallGraph traverses the call graph with a visitor function
func (m *Metadata) TraverseCallGraph(startFrom string, visitor func(*CallGraphEdge, int) bool) {
	visited := make(map[string]bool)
	m.traverseCallGraphHelper(startFrom, 0, visitor, visited)
}

// traverseCallGraphHelper is the internal implementation with cycle detection
func (m *Metadata) traverseCallGraphHelper(current string, depth int, visitor func(*CallGraphEdge, int) bool, visited map[string]bool) {
	if visited[current] {
		return
	}
	visited[current] = true

	if edges, exists := m.Callers[current]; exists {
		for _, edge := range edges {
			if !visitor(edge, depth) {
				return
			}
			m.traverseCallGraphHelper(edge.Callee.BaseID(), depth+1, visitor, visited)
		}
	}
}

// GetCallDepth returns the call depth for a function
func (m *Metadata) GetCallDepth(funcID string) int {
	if depth, exists := m.callDepth[funcID]; exists {
		return depth
	}

	// Calculate depth by traversing up the call graph
	depth := 0
	current := funcID

	for {
		if callers, exists := m.Callees[current]; exists && len(callers) > 0 {
			depth++
			current = callers[0].Caller.BaseID()
		} else {
			break
		}
	}

	m.callDepth[funcID] = depth
	return depth
}

// GetFunctionsAtDepth returns all functions at a specific call depth
func (m *Metadata) GetFunctionsAtDepth(targetDepth int) []*CallGraphEdge {
	var result []*CallGraphEdge

	for funcID := range m.Callers {
		if m.GetCallDepth(funcID) == targetDepth {
			if edges, exists := m.Callers[funcID]; exists {
				result = append(result, edges...)
			}
		}
	}

	return result
}

// IsReachableFrom checks if a function is reachable from another function
func (m *Metadata) IsReachableFrom(fromFunc, toFunc string) bool {
	visited := make(map[string]bool)

	var dfs func(current string) bool
	dfs = func(current string) bool {
		if current == toFunc {
			return true
		}
		if visited[current] {
			return false
		}
		visited[current] = true

		if edges, exists := m.Callers[current]; exists {
			for _, edge := range edges {
				if dfs(edge.Callee.BaseID()) {
					return true
				}
			}
		}
		return false
	}

	return dfs(fromFunc)
}

// GetCallPath returns the call path from one function to another
func (m *Metadata) GetCallPath(fromFunc, toFunc string) []*CallGraphEdge {
	visited := make(map[string]bool)
	var path []*CallGraphEdge

	var dfs func(current string) bool
	dfs = func(current string) bool {
		if current == toFunc {
			return true
		}
		if visited[current] {
			return false
		}
		visited[current] = true

		if edges, exists := m.Callers[current]; exists {
			for _, edge := range edges {
				path = append(path, edge)
				if dfs(edge.Callee.BaseID()) {
					return true
				}
				path = path[:len(path)-1] // backtrack
			}
		}
		return false
	}

	if dfs(fromFunc) {
		return path
	}
	return nil
}

// Generic Type Resolver Methods

// ResolveTypeParameters resolves type parameters for a call graph edge
func (m *Metadata) ResolveTypeParameters(edge *CallGraphEdge) map[string]string {
	resolver := m.GetGenericTypeResolver()
	return resolver.ResolveTypeParameters(edge)
}

// IsGenericTypeCompatible checks if generic types are compatible
func (m *Metadata) IsGenericTypeCompatible(callerTypes, calleeTypes []string) bool {
	resolver := m.GetGenericTypeResolver()
	return resolver.IsCompatible(callerTypes, calleeTypes)
}

// ResolveTypeParameters resolves type parameters for a call graph edge
func (r *GenericTypeResolver) ResolveTypeParameters(edge *CallGraphEdge) map[string]string {
	cacheKey := edge.Caller.ID() + "->" + edge.Callee.ID()

	if cached, exists := r.typeParamCache[cacheKey]; exists {
		return cached
	}

	// Extract and resolve type parameters
	resolved := r.extractTypeParameters(edge)
	r.typeParamCache[cacheKey] = resolved

	return resolved
}

// extractTypeParameters extracts type parameters from a call graph edge
func (r *GenericTypeResolver) extractTypeParameters(edge *CallGraphEdge) map[string]string {
	resolved := make(map[string]string)

	// Copy existing type parameter map
	if edge.TypeParamMap != nil {
		for k, v := range edge.TypeParamMap {
			resolved[k] = v
		}
	}

	return resolved
}

// IsCompatible checks if caller types are compatible with callee types
func (r *GenericTypeResolver) IsCompatible(callerTypes, calleeTypes []string) bool {
	cacheKey := strings.Join(callerTypes, ",") + "->" + strings.Join(calleeTypes, ",")

	if cached, exists := r.compatibilityCache[cacheKey]; exists {
		return cached
	}

	compatible := IsSubset(callerTypes, calleeTypes)
	r.compatibilityCache[cacheKey] = compatible

	return compatible
}

// BuildFuncMap creates a map of function names to their declarations.
func BuildFuncMap(pkgs map[string]map[string]*ast.File) map[string]*ast.FuncDecl {
	funcMap := make(map[string]*ast.FuncDecl)
	for pkgPath, files := range pkgs {
		for _, file := range files {
			pkgName := ""
			if file.Name != nil {
				pkgName = file.Name.Name
			}
			ast.Inspect(file, func(n ast.Node) bool {
				// Handle regular functions
				if fn, isFn := n.(*ast.FuncDecl); isFn {
					var key string
					if fn.Recv == nil || len(fn.Recv.List) == 0 {
						// Only for top-level functions, use package prefix if not main
						if pkgName != "" {
							key = pkgPath + "." + fn.Name.Name
						} else {
							key = fn.Name.Name
						}
						funcMap[key] = fn
					} else {
						// For methods, always use TypeName.MethodName (no package prefix)
						var typeName string
						recvType := fn.Recv.List[0].Type
						if starExpr, ok := recvType.(*ast.StarExpr); ok {
							if ident, ok := starExpr.X.(*ast.Ident); ok {
								typeName = ident.Name
							}
						} else if ident, ok := recvType.(*ast.Ident); ok {
							typeName = ident.Name
						}
						if typeName != "" {
							methodKey := pkgPath + "." + typeName + "." + fn.Name.Name
							funcMap[methodKey] = fn
						}
					}
				}
				return true
			})
		}
	}
	return funcMap
}

// buildFullPath creates the full path for a file
func buildFullPath(importPath, fileName string) string {
	if importPath != "" {
		return importPath + "/" + fileName
	}
	return fileName
}

// collectConstants collects all constants from a file
func collectConstants(file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, meta *Metadata) map[string]string {
	constMap := make(map[string]string)

	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.CONST {
			continue
		}

		for _, spec := range genDecl.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			for i, name := range vspec.Names {
				if len(vspec.Values) > i {
					value := CallArgToString(*ExprToCallArgument(vspec.Values[i], info, pkgName, fset, meta))
					constMap[name.Name] = value
				}
			}
		}
	}

	return constMap
}

// processTypes processes all type declarations in a file
func processTypes(file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, f *File, allTypeMethods map[string][]Method, allTypes map[string]*Type, metadata *Metadata) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || genDecl.Tok != token.TYPE {
			continue
		}

		for _, spec := range genDecl.Specs {
			tspec, ok := spec.(*ast.TypeSpec)
			if !ok {
				continue
			}

			t := &Type{
				Name:  metadata.StringPool.Get(tspec.Name.Name),
				Scope: metadata.StringPool.Get(getScope(tspec.Name.Name)),
			}

			// Extract comments
			t.Comments = metadata.StringPool.Get(getComments(tspec))

			// Process type kind
			processTypeKind(tspec, info, pkgName, fset, t, allTypes, metadata)

			// Add methods for non-interface types
			if t.Kind != metadata.StringPool.Get("interface") {
				specName := getTypeName(tspec)
				t.Methods = allTypeMethods[specName]
				t.Methods = append(t.Methods, allTypeMethods["*"+specName]...)
			}

			f.Types[tspec.Name.Name] = t
		}
	}
}

// processTypeKind determines the kind of type and processes it accordingly
func processTypeKind(tspec *ast.TypeSpec, info *types.Info, pkgName string, fset *token.FileSet, t *Type, allTypes map[string]*Type, metadata *Metadata) {
	switch ut := tspec.Type.(type) {
	case *ast.StructType:
		t.Kind = metadata.StringPool.Get("struct")
		processStructFields(ut, metadata, t)
		allTypes[tspec.Name.Name] = t

	case *ast.InterfaceType:
		t.Kind = metadata.StringPool.Get("interface")
		processInterfaceMethods(ut, info, pkgName, fset, t, metadata)
		allTypes[tspec.Name.Name] = t

	case *ast.Ident:
		t.Kind = metadata.StringPool.Get("alias")
		t.Target = metadata.StringPool.Get(ut.Name)
		allTypes[tspec.Name.Name] = t

	default:
		t.Kind = metadata.StringPool.Get("other")
		allTypes[tspec.Name.Name] = t
	}
}

// processStructFields processes fields of a struct type
func processStructFields(structType *ast.StructType, metadata *Metadata, t *Type) {
	for _, field := range structType.Fields.List {
		fieldType := getTypeName(field.Type)
		tag := getFieldTag(field)
		comments := getComments(field)

		if len(field.Names) == 0 {
			// Embedded (anonymous) field
			t.Embeds = append(t.Embeds, metadata.StringPool.Get(fieldType))
			continue
		}

		for _, name := range field.Names {
			scope := getScope(name.Name)
			f := Field{
				Name:     metadata.StringPool.Get(name.Name),
				Type:     metadata.StringPool.Get(fieldType),
				Tag:      metadata.StringPool.Get(tag),
				Scope:    metadata.StringPool.Get(scope),
				Comments: metadata.StringPool.Get(comments),
			}

			// Check if this field has a nested struct type
			if structTypeExpr, ok := field.Type.(*ast.StructType); ok {
				// Create a nested type for this field
				nestedType := &Type{
					Name:     metadata.StringPool.Get(name.Name + "_nested"),
					Kind:     metadata.StringPool.Get("struct"),
					Scope:    metadata.StringPool.Get(getScope(name.Name)),
					Comments: metadata.StringPool.Get(comments),
				}
				processStructFields(structTypeExpr, metadata, nestedType)
				f.NestedType = nestedType
			}

			t.Fields = append(t.Fields, f)
		}
	}
}

// processInterfaceMethods processes methods of an interface type
func processInterfaceMethods(interfaceType *ast.InterfaceType, info *types.Info, pkgName string, fset *token.FileSet, t *Type, metadata *Metadata) {
	for _, method := range interfaceType.Methods.List {
		if len(method.Names) > 0 {
			m := Method{
				Name:      metadata.StringPool.Get(method.Names[0].Name),
				Signature: *ExprToCallArgument(method.Type.(*ast.FuncType), info, pkgName, fset, metadata),
				Scope:     metadata.StringPool.Get(getScope(method.Names[0].Name)),
			}
			m.SignatureStr = metadata.StringPool.Get(CallArgToString(m.Signature))
			m.Comments = metadata.StringPool.Get(getComments(method))
			t.Methods = append(t.Methods, m)
		}
	}
}

// processFunctions processes all function declarations in a file
func processFunctions(file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, f *File, fileToInfo map[*ast.File]*types.Info, funcMap map[string]*ast.FuncDecl, metadata *Metadata) {
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Recv != nil {
			continue
		}

		comments := getComments(fn)

		// Extract type parameter names for generics
		typeParams := []string{}
		if fn.Type != nil && fn.Type.TypeParams != nil {
			for _, tparam := range fn.Type.TypeParams.List {
				for _, name := range tparam.Names {
					typeParams = append(typeParams, name.Name)
				}
			}
		}

		// Extract return value origins
		var returnVars []CallArgument
		var maxReturnCount int

		if fn.Body != nil {
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				ret, ok := n.(*ast.ReturnStmt)
				if !ok {
					return true
				}

				// Track the maximum number of return values seen
				if len(ret.Results) > maxReturnCount {
					maxReturnCount = len(ret.Results)
					returnVars = nil // Clear and rebuild with the most complete return
					for _, expr := range ret.Results {
						returnVars = append(returnVars, *ExprToCallArgument(expr, info, pkgName, fset, metadata))
					}
				}

				return true // Continue traversal to see all returns
			})
		}

		// Use funcMap to get callee function declaration
		var assignmentsInFunc = make(map[string][]Assignment)

		ast.Inspect(fn, func(nd ast.Node) bool {
			switch expr := nd.(type) {
			case *ast.AssignStmt:
				assignments := processAssignment(expr, file, info, pkgName, fset, fileToInfo, funcMap, metadata)
				for _, assign := range assignments {
					varName := metadata.StringPool.GetString(assign.VariableName)
					assignmentsInFunc[varName] = append(assignmentsInFunc[varName], assign)
				}
			}
			return true
		})

		f.Functions[fn.Name.Name] = &Function{
			Name:          metadata.StringPool.Get(fn.Name.Name),
			Signature:     *ExprToCallArgument(fn.Type, info, pkgName, fset, metadata),
			Position:      metadata.StringPool.Get(getFuncPosition(fn, fset)),
			Scope:         metadata.StringPool.Get(getScope(fn.Name.Name)),
			Comments:      metadata.StringPool.Get(comments),
			TypeParams:    typeParams,
			ReturnVars:    returnVars,
			AssignmentMap: assignmentsInFunc,
		}
	}
}

// processVariables processes all variable and constant declarations in a file
func processVariables(file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, f *File, metadata *Metadata) {
	for _, decl := range file.Decls {
		genDecl, ok := decl.(*ast.GenDecl)
		if !ok || (genDecl.Tok != token.VAR && genDecl.Tok != token.CONST) {
			continue
		}

		var tok string
		// genDecl is *ast.GenDecl
		switch genDecl.Tok {
		case token.CONST:
			tok = "const"
		case token.VAR:
			tok = "var"
		}

		for _, spec := range genDecl.Specs {
			vspec, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}

			comments := getComments(vspec)
			for i, name := range vspec.Names {
				v := &Variable{
					Name:     metadata.StringPool.Get(name.Name),
					Tok:      metadata.StringPool.Get(tok),
					Type:     metadata.StringPool.Get(getTypeName(vspec.Type)),
					Position: metadata.StringPool.Get(getVarPosition(name, fset)),
					Comments: metadata.StringPool.Get(comments),
				}

				if len(vspec.Values) > i {
					v.Value = metadata.StringPool.Get(CallArgToString(*ExprToCallArgument(vspec.Values[i], info, pkgName, fset, metadata)))
				}

				f.Variables[name.Name] = v
			}
		}
	}
}

// processStructInstances processes struct literals and assignments
func processStructInstances(file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, f *File, constMap map[string]string, metadata *Metadata) {
	ast.Inspect(file, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.CompositeLit:
			processStructInstance(x, info, pkgName, fset, f, constMap, metadata)
		}
		return true
	})
}

// processStructInstance processes a struct literal
func processStructInstance(cl *ast.CompositeLit, info *types.Info, pkgName string, fset *token.FileSet, f *File, constMap map[string]string, metadata *Metadata) {
	typeName := getTypeName(cl.Type)
	if typeName == "" {
		return
	}

	fields := map[int]int{}
	for _, elt := range cl.Elts {
		if kv, ok := elt.(*ast.KeyValueExpr); ok {
			key := CallArgToString(*ExprToCallArgument(kv.Key, info, pkgName, fset, metadata))
			val := CallArgToString(*ExprToCallArgument(kv.Value, info, pkgName, fset, metadata))

			// Use constant value if available
			if ident, ok := kv.Value.(*ast.Ident); ok {
				if cval, exists := constMap[ident.Name]; exists {
					val = cval
				}
			}

			fields[metadata.StringPool.Get(key)] = metadata.StringPool.Get(val)
		}
	}

	f.StructInstances = append(f.StructInstances, StructInstance{
		Type:     metadata.StringPool.Get(typeName),
		Position: metadata.StringPool.Get(getPosition(cl.Pos(), fset)),
		Fields:   fields,
	})
}

// processAssignment processes a variable assignment
func processAssignment(assign *ast.AssignStmt, file *ast.File, info *types.Info, pkgName string, fset *token.FileSet, fileToInfo map[*ast.File]*types.Info, funcMap map[string]*ast.FuncDecl, metadata *Metadata) []Assignment {
	var assignments []Assignment

	lhsLen := len(assign.Lhs)
	rhsLen := len(assign.Rhs)
	maxLen := lhsLen
	if rhsLen > maxLen {
		maxLen = rhsLen
	}
	for i := 0; i < maxLen; i++ {
		var lhsExpr ast.Expr
		var rhsExpr ast.Expr
		if i < lhsLen {
			lhsExpr = assign.Lhs[i]
		}
		if i < rhsLen {
			rhsExpr = assign.Rhs[i]
		}

		// Find the enclosing function name for this assignment
		funcName, _ := getEnclosingFunctionName(file, assign.Pos())

		// Handle identifier assignments (var = ...)
		switch expr := lhsExpr.(type) {
		case *ast.Ident:
			if expr.Name == "_" {
				// Skip blank identifier
				continue
			}
			if rhsExpr != nil {
				val := *ExprToCallArgument(rhsExpr, info, pkgName, fset, metadata)
				_, concreteTypeArg := analyzeAssignmentValue(rhsExpr, info, funcName, pkgName, metadata, fset)
				concreteType := ""
				if concreteTypeArg != nil {
					concreteType = concreteTypeArg.GetType()
				}

				if funcName == "" {
					continue
				}

				// if concreteType != "" {
				assignment := Assignment{
					VariableName: metadata.StringPool.Get(expr.Name),
					Pkg:          metadata.StringPool.Get(pkgName),
					ConcreteType: metadata.StringPool.Get(concreteType),
					Position:     metadata.StringPool.Get(getPosition(assign.Pos(), fset)),
					Scope:        metadata.StringPool.Get(getScope(expr.Name)),
					Value:        val,
					Lhs:          *ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata),
					Func:         metadata.StringPool.Get(funcName),
				}
				// If RHS is a function call, record callee info
				if callExpr, ok := rhsExpr.(*ast.CallExpr); ok {
					calleeFunc, calleePkg, _ := getCalleeFunctionNameAndPackage(callExpr.Fun, file, pkgName, fileToInfo, funcMap, fset)
					assignment.CalleeFunc = calleeFunc
					assignment.CalleePkg = calleePkg
					assignment.ReturnIndex = 0 // For now, always first return value
				}
				assignments = append(assignments, assignment)
				assignmentCount++
			}
			// }
		// Handle selector assignments (obj.Field = ...)
		case *ast.SelectorExpr:
			if rhsExpr != nil {
				lhsArg := *ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata)
				assignments = append(assignments, Assignment{
					VariableName: metadata.StringPool.Get(CallArgToString(lhsArg)),
					Pkg:          metadata.StringPool.Get(pkgName),
					ConcreteType: lhsArg.Type,
					Position:     metadata.StringPool.Get(getPosition(assign.Pos(), fset)),
					Scope:        metadata.StringPool.Get("selector"),
					Value:        *ExprToCallArgument(rhsExpr, info, pkgName, fset, metadata),
					Lhs:          *ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata),
				})
			}
		// Handle index assignments (arr[i] = ...)
		case *ast.IndexExpr, *ast.IndexListExpr:
			if rhsExpr != nil {
				assignments = append(assignments, Assignment{
					VariableName: metadata.StringPool.Get(CallArgToString(*ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata))),
					Pkg:          metadata.StringPool.Get(pkgName),
					ConcreteType: metadata.StringPool.Get("index"),
					Position:     metadata.StringPool.Get(getPosition(assign.Pos(), fset)),
					Scope:        metadata.StringPool.Get("index"),
					Value:        *ExprToCallArgument(rhsExpr, info, pkgName, fset, metadata),
					Lhs:          *ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata),
				})
			}
		// Fallback: record any other LHS as a raw assignment
		default:
			if lhsExpr != nil && rhsExpr != nil {
				assignments = append(assignments, Assignment{
					VariableName: metadata.StringPool.Get(CallArgToString(*ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata))),
					Pkg:          metadata.StringPool.Get(pkgName),
					ConcreteType: metadata.StringPool.Get("raw"),
					Position:     metadata.StringPool.Get(getPosition(assign.Pos(), fset)),
					Scope:        metadata.StringPool.Get("raw"),
					Value:        *ExprToCallArgument(rhsExpr, info, pkgName, fset, metadata),
					Lhs:          *ExprToCallArgument(lhsExpr, info, pkgName, fset, metadata),
				})
			}
		}
	}

	return assignments
}

// processImports processes import statements
func processImports(file *ast.File, metadata *Metadata, f *File) {
	for _, imp := range file.Imports {
		importPath := getImportPath(imp)
		alias := getImportAlias(imp)
		if alias == "" {
			alias = importPath
		}
		f.Imports[metadata.StringPool.Get(alias)] = metadata.StringPool.Get(importPath)
	}
}

// buildCallGraph builds the call graph for all files in a package
func buildCallGraph(files map[string]*ast.File, pkgs map[string]map[string]*ast.File, pkgName string, fileToInfo map[*ast.File]*types.Info, fset *token.FileSet, funcMap map[string]*ast.FuncDecl, metadata *Metadata) {
	for _, file := range files {
		var argMap = map[string]*CallArgument{}
		var calleeMap = map[string]*CallGraphEdge{}

		info := fileToInfo[file]

		var assignStmt *ast.AssignStmt

		ast.Inspect(file, func(n ast.Node) bool {
			if n == nil {
				return true
			}

			if call, ok := n.(*ast.CallExpr); ok {
				processCallExpression(call, file, pkgs, pkgName, assignStmt, fileToInfo, funcMap, fset, metadata, info, calleeMap, argMap)
				assignStmt = nil
			} else if assign, ok := n.(*ast.AssignStmt); ok {
				// Find which variable this call is assigned to
				assignStmt = assign
			}

			return true
		})

		for argID, arg := range argMap {
			if edge, ok := calleeMap[argID]; ok {
				arg.Edge = edge
			}
		}
	}
}

// processCallExpression processes a function call expression
func processCallExpression(call *ast.CallExpr, file *ast.File, pkgs map[string]map[string]*ast.File, pkgName string, parentAssign *ast.AssignStmt, fileToInfo map[*ast.File]*types.Info, funcMap map[string]*ast.FuncDecl, fset *token.FileSet, metadata *Metadata, info *types.Info, calleeMap map[string]*CallGraphEdge, argMap map[string]*CallArgument) {
	callerFunc, callerParts := getEnclosingFunctionName(file, call.Pos())
	calleeFunc, calleePkg, calleeParts := getCalleeFunctionNameAndPackage(call.Fun, file, pkgName, fileToInfo, funcMap, fset)

	if callerFunc != "" && calleeFunc != "" {
		// Collect arguments
		args := make([]CallArgument, len(call.Args))
		for i, arg := range call.Args {
			args[i] = *ExprToCallArgument(arg, info, pkgName, fset, metadata)
			argMap[args[i].ID()] = &args[i]
		}

		// Build parameter-to-argument mapping
		paramArgMap := make(map[string]CallArgument)
		typeParamMap := make(map[string]string)

		// Get the *types.Object for the function being called
		// This is crucial for getting the *declared* generic type parameters
		extractParamsAndTypeParams(call, info, args, paramArgMap, typeParamMap)

		// Use funcMap to get callee function declaration
		var assignmentsInFunc = make(map[string][]Assignment)

		calleeAstFile := astFileFromFn(calleePkg, calleeFunc, pkgs, metadata)

		if calleeAstFile != nil {
			fnInfo := fileToInfo[calleeAstFile]
			var funcName string

			if calleeParts == "" {
				funcName = calleePkg + "." + calleeFunc
			} else {
				calleeParts = strings.TrimPrefix(calleeParts, "*")

				funcName = calleePkg + "." + calleeParts + "." + calleeFunc
			}

			if fn, ok := funcMap[funcName]; ok {
				ast.Inspect(fn, func(nd ast.Node) bool {
					if nd == nil {
						return true
					}

					switch expr := nd.(type) {
					case *ast.AssignStmt:
						// IMPORTANT: The `file` argument in processAssignment should be the file of the *callee*,
						// not the caller. Otherwise, info.ObjectOf might return nil for objects not in the caller's file.
						// We need to find the correct `*ast.File` object for the callee's declaration.
						// This lookup is more complex than just using `pos.Filename` because `pkgs` is keyed by package path,
						// and `fileToInfo` maps `*ast.File` pointers.
						assignments := processAssignment(expr, calleeAstFile, fnInfo, calleePkg, fset, fileToInfo, funcMap, metadata)
						for _, assign := range assignments {
							varName := CallArgToString(assign.Lhs)
							assignmentsInFunc[varName] = append(assignmentsInFunc[varName], assign)
						}
					}
					return true
				})
			}
		}

		var assignVarName string
		// If this call's result is assigned to a variable in the caller, record that mapping as an assignment entry
		if parentAssign != nil {
			assignments := processAssignment(parentAssign, file, info, pkgName, fset, fileToInfo, funcMap, metadata)
			for _, assign := range assignments {
				varName := CallArgToString(assign.Lhs)
				assignVarName = varName
				if callerFunc == MainFunc {
					assignmentsInFunc[varName] = append(assignmentsInFunc[varName], assign)
				}
			}
		}

		// Create the call graph edge
		var calleeVarName string
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Obj != nil {
				// This identifies the variable name of the receiver for method calls (e.g., "myStruct.Method()")
				calleeVarName = ident.Name
			}
		}

		cgEdge := CallGraphEdge{
			Position:          metadata.StringPool.Get(getPosition(call.Pos(), fset)),
			Args:              args,
			AssignmentMap:     assignmentsInFunc,
			ParamArgMap:       paramArgMap,
			TypeParamMap:      typeParamMap,
			CalleeVarName:     calleeVarName,
			CalleeRecvVarName: assignVarName,
			meta:              metadata,
		}

		cgEdge.Caller = *cgEdge.NewCall(
			metadata.StringPool.Get(callerFunc),
			metadata.StringPool.Get(pkgName),
			-1, // No position for caller
			metadata.StringPool.Get(callerParts),
		)

		cgEdge.Callee = *cgEdge.NewCall(
			metadata.StringPool.Get(calleeFunc),
			metadata.StringPool.Get(calleePkg),
			metadata.StringPool.Get(getPosition(call.Pos(), fset)),
			metadata.StringPool.Get(calleeParts),
		)

		// Apply type parameter resolution
		applyTypeParameterResolution(&cgEdge)

		// Use instance ID for calleeMap indexing to avoid conflicts
		calleeInstance := cgEdge.Callee.InstanceID()
		calleeMap[calleeInstance] = &cgEdge

		metadata.CallGraph = append(metadata.CallGraph, cgEdge)
	}
}

func astFileFromFn(pkgName, fnName string, pkgs map[string]map[string]*ast.File, metadata *Metadata) *ast.File {
	var astFile *ast.File

	if pkg, pkgExists := metadata.Packages[pkgName]; pkgExists {
		for fileName, f := range pkg.Files {
			if _, ok := f.Functions[fnName]; ok {
				astFile = pkgs[pkgName][fileName]
				break
			}

			for _, t := range f.Types {
				for _, method := range t.Methods {
					methodName := metadata.StringPool.GetString(method.Name)
					if methodName == fnName {
						astFile = pkgs[pkgName][metadata.StringPool.GetString(method.Filename)]
						return astFile
					}
				}
			}

		}
	}

	return astFile
}

func extractParamsAndTypeParams(call *ast.CallExpr, info *types.Info, args []CallArgument, paramArgMap map[string]CallArgument, typeParamMap map[string]string) {
	var funcObj types.Object
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		funcObj = info.ObjectOf(fun)
	case *ast.SelectorExpr:
		funcObj = info.ObjectOf(fun.Sel)
	case *ast.IndexExpr: // For calls like `Func[T]()`
		if ident, ok := fun.X.(*ast.Ident); ok {
			funcObj = info.ObjectOf(ident)
		} else if sel, ok := fun.X.(*ast.SelectorExpr); ok {
			funcObj = info.ObjectOf(sel.Sel)
		}
	case *ast.IndexListExpr: // For calls like `Func[T1, T2]()`
		if ident, ok := fun.X.(*ast.Ident); ok {
			funcObj = info.ObjectOf(ident)
		} else if sel, ok := fun.X.(*ast.SelectorExpr); ok {
			funcObj = info.ObjectOf(sel.Sel)
		}
	}

	if funcObj != nil {
		if fobj, isFunc := funcObj.(*types.Func); isFunc {
			if sig, isSig := fobj.Type().(*types.Signature); isSig {
				// Handle generic type parameters from the *declared* function signature
				if sig.TypeParams() != nil {
					// Attempt to extract explicit type arguments from the call expression syntax
					var explicitTypeArgExprs []ast.Expr
					switch fun := call.Fun.(type) {
					case *ast.IndexExpr:
						explicitTypeArgExprs = []ast.Expr{fun.Index}
					case *ast.IndexListExpr:
						explicitTypeArgExprs = fun.Indices
					case *ast.SelectorExpr:
						// For cases like pkg.Func[T] or receiver.Method[T]
						switch selX := fun.X.(type) {
						case *ast.IndexExpr:
							explicitTypeArgExprs = []ast.Expr{selX.Index}
						case *ast.IndexListExpr:
							explicitTypeArgExprs = selX.Indices
						}
					case *ast.Ident, *ast.ParenExpr: // Handle cases where type arguments are inferred
						// If it's an Ident (e.g., HandleRequest(handler)) or wrapped in Parens,
						// type arguments are inferred, not explicitly in call.Fun syntax.
						// We will use info.Instances below to get inferred types.
						explicitTypeArgExprs = nil // Ensure it's nil or empty
					default:
						explicitTypeArgExprs = nil // Default case, no explicit type arguments
					}

					// If explicit type arguments are provided, use them
					if len(explicitTypeArgExprs) > 0 {
						for i := 0; i < sig.TypeParams().Len(); i++ {
							tparam := sig.TypeParams().At(i)
							name := tparam.Obj().Name()

							if i < len(explicitTypeArgExprs) {
								typeArgExpr := explicitTypeArgExprs[i]
								if typeOfTypeArg := info.TypeOf(typeArgExpr); typeOfTypeArg != nil {
									typeParamMap[name] = typeOfTypeArg.String()
								} else {
									typeParamMap[name] = getTypeName(typeArgExpr)
								}
							}
						}
					} else {
						// No explicit type arguments in the call syntax.
						// This means type inference is happening.
						// We need to get the instantiated types from the *call expression itself*.

						// Handle type inference for different call expression types
						var instance types.Instance
						var found bool

						switch fun := call.Fun.(type) {
						case *ast.Ident:
							instance, found = info.Instances[fun]
						case *ast.SelectorExpr:
							// For selector expressions like pkg.Func, try to get the instance
							instance, found = info.Instances[fun.Sel]
						case *ast.ParenExpr:
							// For parenthesized expressions like (Func), unwrap and try again
							if ident, ok := fun.X.(*ast.Ident); ok {
								instance, found = info.Instances[ident]
							}
						}

						if found && instance.TypeArgs != nil {
							for i := 0; i < sig.TypeParams().Len(); i++ {
								tparam := sig.TypeParams().At(i)
								name := tparam.Obj().Name()
								if i < instance.TypeArgs.Len() {
									inferredType := instance.TypeArgs.At(i)
									typeParamMap[name] = inferredType.String()
								}
							}
						} else {
							// Try to infer types from function arguments
							// This is crucial for cases like HandleRequest(handleSendEmail)
							// where the type parameters are inferred from the argument types
							if len(args) > 0 {
								// Look at the first argument to infer type parameters
								firstArg := args[0]
								if firstArg.GetKind() == KindIdent {
									// Try to get the type of the argument
									if argType := info.TypeOf(call.Args[0]); argType != nil {
										// For function arguments, try to extract parameter types
										if sig, isSig := argType.(*types.Signature); isSig {
											// Check if this is a function type that can help infer generic parameters
											if sig.Params().Len() > 0 {
												// The first parameter type of the argument function
												// should correspond to the first type parameter of the generic function
												firstParamType := sig.Params().At(0).Type()
												if sig.TypeParams().Len() > 0 {
													// This is a generic function argument
													// Try to map its type parameters to the callee's type parameters
													for i := 0; i < sig.TypeParams().Len(); i++ {
														tparam := sig.TypeParams().At(i)
														calleeTParam := sig.TypeParams().At(i)
														if i < sig.TypeParams().Len() {
															// Map the argument's type parameter to the callee's type parameter
															typeParamMap[calleeTParam.Obj().Name()] = tparam.Obj().Name()
														}
													}
												} else {
													// Non-generic function argument
													// The first parameter type should map to the first type parameter
													if sig.TypeParams().Len() > 0 {
														firstTParam := sig.TypeParams().At(0)
														typeParamMap[firstTParam.Obj().Name()] = firstParamType.String()
													}
												}
											}
										}
									}
								}
							}
						}
					}
				}

				// Handle regular parameters
				tup := sig.Params()
				for i := 0; i < tup.Len(); i++ {
					field := tup.At(i)
					if i < len(args) {
						if args[i].TypeParamMap == nil {
							args[i].TypeParamMap = map[string]string{}
						}

						// Propagate type mapping to args
						maps.Copy(args[i].TypeParamMap, typeParamMap)

						paramArgMap[field.Name()] = args[i]
					}
				}
			}
		}
	}
}

// analyzeInterfaceImplementations analyzes which structs implement which interfaces
func analyzeInterfaceImplementations(pkgs map[string]*Package, pool *StringPool) {
	for pkgName, pkg := range pkgs {
		for structName, stct := range pkg.Types {
			if stct.Kind != pool.Get("struct") {
				continue
			}

			structMethods := make(map[int]int) // name -> signature string
			for _, method := range stct.Methods {
				structMethods[method.Name] = method.SignatureStr
			}

			for interfacePkgName, interfacePkg := range pkgs {
				for interfaceName, intrf := range interfacePkg.Types {
					if intrf.Kind != pool.Get("interface") {
						continue
					}

					if implementsInterface(structMethods, intrf) {
						stct.Implements = append(stct.Implements, pool.Get(interfacePkgName+"."+interfaceName))
						intrf.ImplementedBy = append(intrf.ImplementedBy, pool.Get(pkgName+"."+structName))
					}
				}
			}
		}
	}
}

// applyTypeParameterResolution applies ParamArgMap and TypeParamMap to CallArgument structures
// to fill them with correct resolved type information
func applyTypeParameterResolution(edge *CallGraphEdge) {
	if edge == nil {
		return
	}

	// Apply type parameter resolution to all arguments
	for i := range edge.Args {
		arg := &edge.Args[i]
		applyTypeParameterResolutionToArgument(arg, edge.ParamArgMap, arg.TypeParamMap)
	}

	// Apply type parameter resolution to ParamArgMap values
	for paramName, arg := range edge.ParamArgMap {
		resolvedArg := arg
		applyTypeParameterResolutionToArgument(&resolvedArg, edge.ParamArgMap, edge.TypeParamMap)
		edge.ParamArgMap[paramName] = resolvedArg
	}
}

// applyTypeParameterResolutionToArgument applies type parameter resolution to a single CallArgument
func applyTypeParameterResolutionToArgument(arg *CallArgument, paramArgMap map[string]CallArgument, typeParamMap map[string]string) {
	if arg == nil {
		return
	}

	// Check if this argument represents a generic type parameter
	if arg.Type != -1 {
		// Check if the type is a generic type parameter (e.g., "TRequest", "TData")
		if concreteType, exists := typeParamMap[arg.GetType()]; exists || len(arg.TypeParamMap) > 0 {
			arg.ResolvedType = arg.Meta.StringPool.Get(concreteType)
			arg.IsGenericType = true
			arg.GenericTypeName = arg.Type
		}
	}

	// Recursively apply to nested arguments
	if arg.X != nil {
		applyTypeParameterResolutionToArgument(arg.X, paramArgMap, arg.X.TypeParamMap)
	}
	if arg.Fun != nil {
		applyTypeParameterResolutionToArgument(arg.Fun, paramArgMap, arg.Fun.TypeParamMap)
	}
	for i := range arg.Args {
		applyTypeParameterResolutionToArgument(&arg.Args[i], paramArgMap, arg.Args[i].TypeParamMap)
	}
}

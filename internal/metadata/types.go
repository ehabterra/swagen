package metadata

import (
	"fmt"
	"maps"
	"regexp"
	"sort"
	"strings"
)

const (
	KindIdent           = "ident"
	KindLiteral         = "literal"
	KindSelector        = "selector"
	KindCall            = "call"
	KindRaw             = "raw"
	KindString          = "string"
	KindInt             = "int"
	KindFloat64         = "float64"
	KindRune            = "rune"
	KindComplex128      = "complex128"
	KindFuncLit         = "func_lit"
	KindUnary           = "unary"
	KindBinary          = "binary"
	KindIndex           = "index"
	KindIndexList       = "index_list"
	KindStar            = "star"
	KindParen           = "paren"
	KindArrayType       = "array_type"
	KindSlice           = "slice"
	KindCompositeLit    = "composite_lit"
	KindKeyValue        = "key_value"
	KindTypeAssert      = "type_assert"
	KindChanType        = "chan_type"
	KindMapType         = "map_type"
	KindStructType      = "struct_type"
	KindInterfaceType   = "interface_type"
	KindInterfaceMethod = "interface_method"
	KindEmbed           = "embed"
	KindField           = "field"
	KindEllipsis        = "ellipsis"
	KindFuncType        = "func_type"
	KindFuncResults     = "func_results"
)

// StringPool for deduplicating strings across metadata
type StringPool struct {
	strings map[string]int
	values  []string
}

func NewStringPool() *StringPool {
	return &StringPool{
		strings: make(map[string]int),
		values:  make([]string, 0),
	}
}

func (sp *StringPool) Get(s string) int {
	if s == "" {
		return -1
	}

	if idx, exists := sp.strings[s]; exists {
		return idx
	}

	if sp.strings == nil {
		return -1
	}

	idx := len(sp.values)
	sp.strings[s] = idx
	sp.values = append(sp.values, s)
	return idx
}

func (sp *StringPool) GetString(idx int) string {
	if idx >= 0 && idx < len(sp.values) {
		return sp.values[idx]
	}
	return ""
}

// GetSize returns the number of unique strings in the pool
func (sp *StringPool) GetSize() int {
	return len(sp.values)
}

// MarshalYAML implements yaml.Marshaler interface
func (sp *StringPool) MarshalYAML() (interface{}, error) {
	return sp.values, nil
}

// UnmarshalYAML implements yaml.Unmarshaler interface
func (sp *StringPool) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var values []string
	if err := unmarshal(&values); err != nil {
		return err
	}

	sp.values = values
	sp.strings = make(map[string]int)
	for i, s := range values {
		sp.strings[s] = i
	}
	return nil
}

// Finalize cleans up the string pool by removing the lookup map
func (sp *StringPool) Finalize() {
	// sp.strings = nil
}

// Metadata represents the complete metadata for a Go codebase
type Metadata struct {
	StringPool *StringPool         `yaml:"string_pool,omitempty"`
	Packages   map[string]*Package `yaml:"packages,omitempty"`
	CallGraph  []CallGraphEdge     `yaml:"call_graph,omitempty"`

	Callers map[string][]*CallGraphEdge `yaml:"-"`
	Callees map[string][]*CallGraphEdge `yaml:"-"`
	Args    map[string][]*CallGraphEdge `yaml:"-"`

	roots []*CallGraphEdge `yaml:"-"`

	callDepth map[string]int `yaml:"_"`

	// NEW: Enhanced fields for tracker tree simplification
	assignmentRelationships map[AssignmentKey]*AssignmentLink `yaml:"-"`
	variableRelationships   map[ParamKey]*VariableLink        `yaml:"-"`
	argumentProcessor       *ArgumentProcessor                `yaml:"-"`
	genericResolver         *GenericTypeResolver              `yaml:"-"`
}

// BuildCallGraphMaps builds the various lookup maps
func (m *Metadata) BuildCallGraphMaps() {
	m.Callers = make(map[string][]*CallGraphEdge)
	m.Callees = make(map[string][]*CallGraphEdge)
	m.Args = make(map[string][]*CallGraphEdge)
	m.callDepth = map[string]int{}

	for i := range m.CallGraph {
		edge := &m.CallGraph[i]

		callerBase := edge.Caller.BaseID()
		calleeBase := edge.Callee.BaseID()

		m.Callers[callerBase] = append(m.Callers[callerBase], edge)
		m.Callees[calleeBase] = append(m.Callees[calleeBase], edge)

		// Index arguments by their base IDs
		for _, arg := range edge.Args {
			argBase := stripToBase(arg.ID())
			m.Args[argBase] = append(m.Args[argBase], edge)
		}
	}
}

// GetCallersOfFunction returns all edges where the given function is the caller
func (m *Metadata) GetCallersOfFunction(pkg, funcName string) []*CallGraphEdge {
	baseID := fmt.Sprintf("%s.%s", pkg, funcName)
	return m.Callers[baseID]
}

// GetCalleesOfFunction returns all edges where the given function is called
func (m *Metadata) GetCalleesOfFunction(pkg, funcName string) []*CallGraphEdge {
	baseID := fmt.Sprintf("%s.%s", pkg, funcName)
	return m.Callees[baseID]
}

// GetCallersOfMethod returns all edges where the given method is the caller
func (m *Metadata) GetCallersOfMethod(pkg, recvType, methodName string) []*CallGraphEdge {
	baseID := fmt.Sprintf("%s.%s.%s", pkg, recvType, methodName)
	return m.Callers[baseID]
}

// GetCalleesOfMethod returns all edges where the given method is called
func (m *Metadata) GetCalleesOfMethod(pkg, recvType, methodName string) []*CallGraphEdge {
	baseID := fmt.Sprintf("%s.%s.%s", pkg, recvType, methodName)
	return m.Callees[baseID]
}

// IsSubset checks if array 'a' is a subset of array 'b'
// Returns true if all elements in 'a' exist in 'b'
func IsSubset(a, b []string) bool {
	// Create a map for O(1) lookups
	bMap := make(map[string]bool)
	for _, item := range b {
		bMap[item] = true
	}

	// Check if all items in 'a' exist in 'b'
	for _, item := range a {
		if !bMap[item] {
			return false
		}
	}

	return true
}

// ExtractGenericTypes extracts the values from generic type parameters in a string
// Supports two formats:
// 1. "path.Function[TParam1=Value1,TParam2=Value2]" -> extracts values after '='
// 2. "path.Function[Type1,Type2,Type3]" -> extracts all comma-separated types
// Returns: []string containing the extracted types
func ExtractGenericTypes(input string) []string {
	// Find the content between square brackets
	re := regexp.MustCompile(`\[([^\]]+)\]`)
	matches := re.FindStringSubmatch(input)

	if len(matches) < 2 {
		return []string{}
	}

	// Extract the parameters string (everything between [ and ])
	params := matches[1]

	// Split by comma to get individual items
	items := strings.Split(params, ",")

	var result []string
	for _, item := range items {
		item = strings.TrimSpace(item)

		// Check if this is a key=value format
		if strings.Contains(item, "=") {
			parts := strings.Split(item, "=")
			if len(parts) == 2 {
				value := strings.TrimSpace(parts[1])
				result = append(result, value)
			}
		} else {
			// This is just a type name (comma-separated format)
			result = append(result, item)
		}
	}

	return result
}

func TypeEdges(id string, callerEdges []*CallGraphEdge) []*CallGraphEdge {
	edges := []*CallGraphEdge{}
	idTypes := ExtractGenericTypes(id)

	if len(idTypes) > 0 {
		for i := range callerEdges {
			CallerEdgeID := callerEdges[i].Caller.ID()
			CallerEdgeTypes := ExtractGenericTypes(CallerEdgeID)

			if IsSubset(idTypes, CallerEdgeTypes) {
				edges = append(edges, callerEdges[i])
			}
		}
	} else {
		edges = callerEdges
	}
	return edges
}

const MaxSelfCallingDepth = 50

// TraverseCallerChildren traverses the call graph using base IDs
func (m *Metadata) TraverseCallerChildren(edge *CallGraphEdge, action func(parent, child *CallGraphEdge)) {
	m.traverseCallerChildrenHelper(edge, action, make(map[string]bool))
}

// traverseCallerChildrenHelper is the internal implementation with cycle detection
func (m *Metadata) traverseCallerChildrenHelper(edge *CallGraphEdge, action func(parent, child *CallGraphEdge), visited map[string]bool) {
	calleeBase := edge.Callee.BaseID()

	// Check for cycles
	if visited[calleeBase] {
		return
	}
	visited[calleeBase] = true
	defer delete(visited, calleeBase)

	if children, ok := m.Callers[calleeBase]; ok {
		for _, child := range children {
			if calleeBase == child.Callee.BaseID() { // Limit self calling
				if m.callDepth[calleeBase] >= MaxSelfCallingDepth {
					continue
				}
				m.callDepth[calleeBase]++
			}
			action(edge, child)
			m.traverseCallerChildrenHelper(child, action, visited)
		}
	}
}

// CallGraphRoots finds root functions using base IDs
func (m *Metadata) CallGraphRoots() []*CallGraphEdge {
	if len(m.roots) > 0 {
		return m.roots
	}

	// Search for root functions using base IDs
	for i := range m.CallGraph {
		edge := &m.CallGraph[i]
		callerBase := edge.Caller.BaseID()

		var isRoot = true

		// Always consider main function as a root
		callerName := m.StringPool.GetString(edge.Caller.Name)
		if callerName == "main" {
			isRoot = true
		} else {
			// Check if this function is called by anyone (using base ID)
			if _, exists := m.Callees[callerBase]; exists {
				isRoot = false
			}

			// Check if this function appears as an argument (using base ID)
			if _, exists := m.Args[callerBase]; exists {
				isRoot = false
			}
		}

		if isRoot {
			m.roots = append(m.roots, edge)
		}
	}

	return m.roots
}

// Package represents a Go package
type Package struct {
	Files map[string]*File `yaml:"files,omitempty"`
	Types map[string]*Type `yaml:"types,omitempty"`
}

// File represents a Go source file
type File struct {
	Types           map[string]*Type     `yaml:"types,omitempty"`
	Functions       map[string]*Function `yaml:"functions,omitempty"`
	Variables       map[string]*Variable `yaml:"variables,omitempty"`
	StructInstances []StructInstance     `yaml:"struct_instances,omitempty"`
	// Selectors       []Selector           `yaml:"selectors"`
	Imports map[int]int `yaml:"imports"` // alias -> path
}

// Type represents a Go type
type Type struct {
	Name          int      `yaml:"name,omitempty"`
	Kind          int      `yaml:"kind,omitempty"`
	Target        int      `yaml:"target,omitempty"`
	Implements    []int    `yaml:"implements,omitempty"`
	ImplementedBy []int    `yaml:"implemented_by,omitempty"`
	Embeds        []int    `yaml:"embeds,omitempty"`
	Fields        []Field  `yaml:"fields,omitempty"`
	Scope         int      `yaml:"scope,omitempty"`
	Methods       []Method `yaml:"methods,omitempty"`
	Comments      int      `yaml:"comments,omitempty"`
	Tags          []int    `yaml:"tags,omitempty"`
}

// Field represents a struct field
type Field struct {
	Name     int `yaml:"name,omitempty"`
	Type     int `yaml:"type,omitempty"`
	Tag      int `yaml:"tag,omitempty"`
	Scope    int `yaml:"scope,omitempty"`
	Comments int `yaml:"comments,omitempty"`

	// For nested struct types, store the nested type definition
	NestedType *Type `yaml:"nested_type,omitempty"`
}

// Method represents a method
type Method struct {
	Name         int          `yaml:"name,omitempty"`
	Receiver     int          `yaml:"receiver,omitempty"`
	Signature    CallArgument `yaml:"signature,omitempty"`
	SignatureStr int          `yaml:"signature_str,omitempty"`
	Position     int          `yaml:"position,omitempty"`
	Scope        int          `yaml:"scope,omitempty"`
	Comments     int          `yaml:"comments,omitempty"`
	Tags         []int        `yaml:"tags,omitempty"`
	Filename     int          `yaml:"filename,omitempty"`

	// Type parameter names for generics
	TypeParams []string `yaml:"type_params,omitempty"`

	// Return value origins for tracing through return values
	ReturnVars []CallArgument `yaml:"return_vars,omitempty"`

	// map of variable name to all assignments (for alias/reassignment tracking)
	AssignmentMap map[string][]Assignment `yaml:"assignments,omitempty"`
}

// Function represents a function
type Function struct {
	Name      int          `yaml:"name,omitempty"`
	Signature CallArgument `yaml:"signature,omitempty"`
	Position  int          `yaml:"position,omitempty"`
	Scope     int          `yaml:"scope,omitempty"`
	Comments  int          `yaml:"comments,omitempty"`
	Tags      []int        `yaml:"tags,omitempty"`

	// Type parameter names for generics
	TypeParams []string `yaml:"type_params,omitempty"`

	// Return value origins for tracing through return values
	ReturnVars []CallArgument `yaml:"return_vars,omitempty"`

	// map of variable name to all assignments (for alias/reassignment tracking)
	AssignmentMap map[string][]Assignment `yaml:"assignments,omitempty"`
}

// Variable represents a variable
type Variable struct {
	Name     int `yaml:"name,omitempty"`
	Tok      int `yaml:"tok,omitempty"`
	Type     int `yaml:"type,omitempty"`
	Value    int `yaml:"value,omitempty"`
	Position int `yaml:"position,omitempty"`
	Comments int `yaml:"comments,omitempty"`
}

// Selector represents a selector expression
type Selector struct {
	Expr     CallArgument `yaml:"expr,omitempty"`
	Kind     int          `yaml:"kind,omitempty"`
	Position int          `yaml:"position,omitempty"`
}

// StructInstance represents a struct literal instance
type StructInstance struct {
	Type     int         `yaml:"type,omitempty"`
	Position int         `yaml:"position,omitempty"`
	Fields   map[int]int `yaml:"fields,omitempty"`
}

// Assignment represents a variable assignment
type Assignment struct {
	VariableName int          `yaml:"variable_name,omitempty"`
	Pkg          int          `yaml:"pkg,omitempty"`
	ConcreteType int          `yaml:"concrete_type,omitempty"`
	Position     int          `yaml:"position,omitempty"`
	Scope        int          `yaml:"scope,omitempty"`
	Value        CallArgument `yaml:"value,omitempty"`
	Lhs          CallArgument `yaml:"lhs,omitempty"`
	Func         int          `yaml:"func,omitempty"`

	// For assignments from function calls
	CalleeFunc  string `yaml:"callee_func,omitempty"`
	CalleePkg   string `yaml:"callee_pkg,omitempty"`
	ReturnIndex int    `yaml:"return_index,omitempty"`
}

// CallArgument represents a function call argument or expression
type CallArgument struct {
	idstr    string
	Kind     int                    `yaml:"kind"`            // ident, literal, selector, call, raw
	Name     int                    `yaml:"name,omitempty"`  // for ident
	Value    int                    `yaml:"value,omitempty"` // for literal
	X        *CallArgument          `yaml:"x,omitempty"`     // for selector/call
	Sel      *CallArgument          `yaml:"sel,omitempty"`   // for selector
	Fun      *CallArgument          `yaml:"fun,omitempty"`   // for call
	Args     []CallArgument         `yaml:"args,omitempty"`  // for call
	Raw      int                    `yaml:"raw,omitempty"`   // fallback
	Extra    map[string]interface{} `yaml:"extra,omitempty"` // extensibility
	Pkg      int                    `yaml:"pkg,omitempty"`   // for ident
	Type     int                    `yaml:"type,omitempty"`  // for ident
	Position int                    `yaml:"position,omitempty"`

	// Callee edge for the same call if it's kind is call
	Edge *CallGraphEdge `yaml:"-"`

	// fields for argument-to-parameter and type parameter mapping
	ParamArgMap  map[string]CallArgument `yaml:"-"` // parameter name -> argument
	TypeParamMap map[string]string       `yaml:"-"` // type parameter name -> concrete type

	// Type parameter resolution information
	ResolvedType    int  `yaml:"resolved_type,omitempty"`     // The concrete type after type parameter resolution
	IsGenericType   bool `yaml:"is_generic_type,omitempty"`   // Whether this argument represents a generic type
	GenericTypeName int  `yaml:"generic_type_name,omitempty"` // The generic type parameter name (e.g., "TRequest", "TData")

	// Reference to metadata for StringPool access
	Meta *Metadata `yaml:"-"`
}

// Helper methods to get string values from StringPool indices
func (a *CallArgument) GetKind() string {
	if a.Kind >= 0 && a.Meta.StringPool != nil {
		kind := a.Meta.StringPool.GetString(a.Kind)
		return kind
	}
	return ""
}

func (a *CallArgument) GetName() string {
	if a.Name >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.Name)
	}
	return ""
}

func (a *CallArgument) GetValue() string {
	if a.Value >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.Value)
	}
	return ""
}

func (a *CallArgument) GetRaw() string {
	if a.Raw >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.Raw)
	}
	return ""
}

func (a *CallArgument) GetPkg() string {
	if a.Pkg >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.Pkg)
	}
	return ""
}

func (a *CallArgument) GetType() string {
	if a.Type >= 0 && a.Meta.StringPool != nil {
		typ := a.Meta.StringPool.GetString(a.Type)
		return typ
	}
	return ""
}

func (a *CallArgument) GetPosition() string {
	if a.Position >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.Position)
	}
	return ""
}

func (a *CallArgument) GetResolvedType() string {
	if a.ResolvedType >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.ResolvedType)
	}
	return ""
}

func (a *CallArgument) GetGenericTypeName() string {
	if a.GenericTypeName >= 0 && a.Meta.StringPool != nil {
		return a.Meta.StringPool.GetString(a.GenericTypeName)
	}
	return ""
}

// NewCallArgument creates a new CallArgument with metadata reference
func NewCallArgument(meta *Metadata) *CallArgument {
	if meta == nil {
		panic("metadata is nil")
	}

	return &CallArgument{
		Kind:            -1,
		Name:            -1,
		Value:           -1,
		Raw:             -1,
		Pkg:             -1,
		Type:            -1,
		Position:        -1,
		ResolvedType:    -1,
		GenericTypeName: -1,
		Meta:            meta,
	}
}

// SetString methods to set string values using StringPool
func (a *CallArgument) SetKind(kind string) {
	if a.Meta.StringPool != nil {
		a.Kind = a.Meta.StringPool.Get(kind)
	}
}

func (a *CallArgument) SetName(name string) {
	if a.Meta.StringPool != nil {
		a.Name = a.Meta.StringPool.Get(name)
	}
}

func (a *CallArgument) SetValue(value string) {
	if a.Meta.StringPool != nil {
		a.Value = a.Meta.StringPool.Get(value)
	}
}

func (a *CallArgument) SetRaw(raw string) {
	if a.Meta.StringPool != nil {
		a.Raw = a.Meta.StringPool.Get(raw)
	}
}

func (a *CallArgument) SetPkg(pkg string) {
	if a.Meta.StringPool != nil {
		a.Pkg = a.Meta.StringPool.Get(pkg)
	}
}

func (a *CallArgument) SetType(typeStr string) {
	if a.Meta.StringPool != nil {
		a.Type = a.Meta.StringPool.Get(typeStr)
	}
}

func (a *CallArgument) SetPosition(position string) {
	if a.Meta.StringPool != nil {
		a.Position = a.Meta.StringPool.Get(position)
	}
}

func (a *CallArgument) SetResolvedType(resolvedType string) {
	if a.Meta.StringPool != nil {
		a.ResolvedType = a.Meta.StringPool.Get(resolvedType)
	}
}

func (a *CallArgument) SetGenericTypeName(genericTypeName string) {
	if a.Meta.StringPool != nil {
		a.GenericTypeName = a.Meta.StringPool.Get(genericTypeName)
	}
}

func (a *CallArgument) TypeParams() map[string]string {
	if a.TypeParamMap == nil {
		a.TypeParamMap = map[string]string{}
	}

	// Propagate type resolving
	if a.Edge != nil && len(a.Edge.TypeParamMap) > 0 {
		maps.Copy(a.TypeParamMap, a.Edge.TypeParamMap)
	}

	return a.TypeParamMap
}

func (a *CallArgument) ID() string {
	var pos string

	if a.idstr != "" {
		return a.idstr
	}

	position := a.GetPosition()
	if position != "" {
		pos = "@" + position
	}

	id, typeParam := a.id(".")

	a.idstr = id + typeParam + pos

	a.idstr = strings.TrimPrefix(a.idstr, "*")

	return a.idstr
}

// ID returns a unique identifier for the call argument
func (a *CallArgument) id(sep string) (string, string) {
	var typeParam string

	typeParams := a.TypeParams()
	if len(typeParams) > 0 {
		var genericParts []string
		for param, concrete := range typeParams {
			genericParts = append(genericParts, fmt.Sprintf("%s=%s", param, concrete))
		}
		sort.Slice(genericParts, func(i, j int) bool { return genericParts[i] < genericParts[j] })
		typeParam = fmt.Sprintf("[%s]", strings.Join(genericParts, ","))
	}

	kind := a.GetKind()
	switch kind {
	case KindIdent:
		typeStr := a.GetType()
		pkgStr := a.GetPkg()
		nameStr := a.GetName()

		if typeStr != "" && sep == "/" {
			return typeStr, typeParam
		} else if pkgStr != "" {
			if sep == "/" {
				return "", typeParam
			}
			return pkgStr + sep + nameStr, typeParam
		}
		return nameStr, typeParam
	case KindLiteral:
		return a.GetValue(), typeParam
	case KindSelector:
		if a.X != nil {
			xID, xTypeParam := a.X.id("/")
			if xID == "" {
				xID = a.Sel.GetPkg()
			}
			id := xID + sep + a.Sel.GetName()

			if xTypeParam != "" {
				typeParam = xTypeParam
			}

			return id, typeParam
		}
		return a.Sel.GetName(), typeParam
	case KindFuncType:
		return a.GetValue(), typeParam
	case KindCall:
		if a.Fun != nil {
			funID, funTypeParam := a.Fun.id(".")
			if funTypeParam != "" {
				typeParam = funTypeParam
			}

			return funID, typeParam
		}
		return KindCall, typeParam
	case KindUnary:
		if a.X != nil {
			xID, xTypeParam := a.X.id("/")
			if xID == "" {
				xID = a.GetPkg()
			}
			id := a.GetValue() + xID

			if xTypeParam != "" {
				typeParam = xTypeParam
			}

			return id, typeParam
		}
		return "", ""
	case KindCompositeLit:
		if a.X != nil {
			xID, xTypeParam := a.X.id("/")
			if xID == "" {
				xID = a.GetPkg()
			}
			id := xID

			if xTypeParam != "" {
				typeParam = xTypeParam
			}

			return id, typeParam
		}
		return "", ""
	case KindIndex:
		if a.X != nil {
			xID, xTypeParam := a.X.id("/")
			if xID == "" {
				xID = a.GetPkg()
			}
			id := xID

			if xTypeParam != "" {
				typeParam = xTypeParam
			}

			return id, typeParam
		}
		return "", ""
	default:
		return a.GetRaw(), typeParam
	}
}

type Call struct {
	Meta *Metadata      `yaml:"-"`
	Edge *CallGraphEdge `yaml:"-"`

	// Separate fields for different ID components
	identifier *CallIdentifier `yaml:"-"`

	// Keep existing fields for serialization
	Name     int `yaml:"name,omitempty"`
	Pkg      int `yaml:"pkg,omitempty"`
	Position int `yaml:"position,omitempty"`
	RecvType int `yaml:"recv_type,omitempty"`
}

// ID returns different types of identifiers based on context
func (c *Call) ID() string {
	return c.InstanceID() // Default to instance ID for backward compatibility
}

// BaseID returns the base identifier without position or generics
func (c *Call) BaseID() string {
	if c.identifier == nil {
		c.buildIdentifier()
	}
	return c.identifier.ID(BaseID)
}

// GenericID returns the identifier with generic type parameters but no position
func (c *Call) GenericID() string {
	if c.identifier == nil {
		c.buildIdentifier()
	}
	return c.identifier.ID(GenericID)
}

// InstanceID returns the full instance identifier with position and generics
func (c *Call) InstanceID() string {
	if c.identifier == nil {
		c.buildIdentifier()
	}
	return c.identifier.ID(InstanceID)
}

func (c *Call) buildIdentifier() {
	var generics map[string]string
	if c.Edge != nil && c.Edge.TypeParamMap != nil {
		generics = make(map[string]string)
		for k, v := range c.Edge.TypeParamMap {
			generics[k] = v
		}
	}

	c.identifier = NewCallIdentifier(
		c.Meta.StringPool.GetString(c.Pkg),
		c.Meta.StringPool.GetString(c.Name),
		c.Meta.StringPool.GetString(c.RecvType),
		c.Meta.StringPool.GetString(c.Position),
		generics,
	)
}

// CallGraphEdge represents an edge in the call graph
type CallGraphEdge struct {
	Caller        Call                    `yaml:"caller,omitempty"`
	Callee        Call                    `yaml:"callee,omitempty"`
	Position      int                     `yaml:"position,omitempty"`
	Args          []CallArgument          `yaml:"args,omitempty"`
	AssignmentMap map[string][]Assignment `yaml:"assignments,omitempty"`

	// New fields for argument-to-parameter and type parameter mapping
	ParamArgMap  map[string]CallArgument `yaml:"param_arg_map,omitempty"`  // parameter name -> argument
	TypeParamMap map[string]string       `yaml:"type_param_map,omitempty"` // type parameter name -> concrete type

	CalleeVarName     string `yaml:"callee_var_name,omitempty"`
	CalleeRecvVarName string `yaml:"callee_recv_var_name,omitempty"`

	meta *Metadata
}

func (edge *CallGraphEdge) NewCall(name, pkg, position, recvType int) *Call {
	return &Call{
		Edge:     edge,
		Meta:     edge.meta,
		Name:     name,
		Pkg:      pkg,
		Position: position,
		RecvType: recvType,
	}
}

// GlobalAssignment represents a global variable assignment
type GlobalAssignment struct {
	ConcreteType string `yaml:"-"`
	PkgName      string `yaml:"-"`
}

// NEW: Enhanced metadata structures for tracker tree simplification

// ArgumentType represents the classification of an argument
type ArgumentType int

const (
	ArgTypeDirectCallee ArgumentType = iota // Direct function call (existing callee)
	ArgTypeFunctionCall                     // Function call as argument
	ArgTypeVariable                         // Variable reference
	ArgTypeLiteral                          // Literal value
	ArgTypeSelector                         // Field/method selector
	ArgTypeComplex                          // Complex expression
	ArgTypeUnary                            // Unary expression (*ptr, &val)
	ArgTypeBinary                           // Binary expression (a + b)
	ArgTypeIndex                            // Index expression (arr[i])
	ArgTypeComposite                        // Composite literal (struct{})
	ArgTypeTypeAssert                       // Type assertion (val.(type))
)

// VariableOrigin represents the origin of a variable
type VariableOrigin struct {
	OriginVar  string
	OriginPkg  string
	OriginFunc string
	OriginArg  *CallArgument
}

// AssignmentLink represents a link between an assignment and a call graph edge
type AssignmentLink struct {
	AssignmentKey AssignmentKey
	Assignment    *Assignment
	Edge          *CallGraphEdge
}

// VariableLink represents a link between a variable and a call graph edge
type VariableLink struct {
	ParamKey   ParamKey
	OriginVar  string
	OriginPkg  string
	OriginFunc string
	Edge       *CallGraphEdge
	Argument   *CallArgument
}

// ProcessedArgument represents a processed argument with enhanced information
type ProcessedArgument struct {
	Argument   *CallArgument
	Edge       *CallGraphEdge
	ArgType    ArgumentType
	ArgIndex   int
	ArgContext string
	Children   []*ProcessedArgument
}

// AssignmentKey represents a key for assignment relationships
type AssignmentKey struct {
	Name      string
	Pkg       string
	Type      string
	Container string
}

func (k AssignmentKey) String() string {
	return k.Pkg + k.Type + k.Name + k.Container
}

// ParamKey represents a key for parameter relationships
type ParamKey struct {
	Name      string
	Pkg       string
	Container string
}

// ArgumentProcessor handles argument processing and classification
type ArgumentProcessor struct {
	// Argument classification cache
	argTypeCache map[string]ArgumentType

	// Variable tracing cache
	variableOriginCache map[string]VariableOrigin

	// Assignment linking cache
	assignmentLinkCache map[string][]AssignmentLink
}

// GenericTypeResolver handles generic type parameter resolution
type GenericTypeResolver struct {
	// Type parameter mapping cache
	typeParamCache map[string]map[string]string

	// Generic type compatibility cache
	compatibilityCache map[string]bool
}

// TrackerLimits holds configuration for tree/graph traversal limits
type TrackerLimits struct {
	MaxNodesPerTree    int
	MaxChildrenPerNode int
	MaxArgsPerFunction int
	MaxNestedArgsDepth int
}

// ProcessFunctionReturnTypes processes all functions and methods in the metadata
// to fill their ResolvedType based on their ReturnVars, assuming the first return value is the target type
func (m *Metadata) ProcessFunctionReturnTypes() {
	for _, pkg := range m.Packages {
		for _, file := range pkg.Files {
			// Process functions
			for funcName, fn := range file.Functions {
				m.processFunctionReturnType(fn)
				file.Functions[funcName] = fn
			}

			// Process methods in types
			for typeName, typ := range file.Types {
				for i := range typ.Methods {
					m.processMethodReturnType(&typ.Methods[i])
				}
				file.Types[typeName] = typ
			}
		}
	}

	// Process call graph edges to set ResolvedType on function call arguments
	m.processCallGraphReturnTypes()
}

// processFunctionReturnType processes a single function to set its ResolvedType
func (m *Metadata) processFunctionReturnType(fn *Function) {
	// Set the Meta reference for the signature
	fn.Signature.Meta = m

	// Extract return type from function signature
	resolvedType := m.extractReturnTypeFromSignature(fn.Signature)

	if resolvedType != "" {
		fn.Signature.SetResolvedType(resolvedType)
	}
}

// processMethodReturnType processes a single method to set its ResolvedType
func (m *Metadata) processMethodReturnType(method *Method) {
	// Set the Meta reference for the signature
	method.Signature.Meta = m

	// Extract return type from method signature
	resolvedType := m.extractReturnTypeFromSignature(method.Signature)

	if resolvedType != "" {
		method.Signature.SetResolvedType(resolvedType)
	}
}

// extractReturnTypeFromSignature extracts the return type from a function signature
func (m *Metadata) extractReturnTypeFromSignature(signature CallArgument) string {
	// Set the Meta reference
	signature.Meta = m

	switch signature.GetKind() {
	case KindFuncType:
		// For function types, extract the return type from the results
		if signature.Fun != nil && signature.Fun.GetKind() == KindFuncResults {
			if len(signature.Fun.Args) > 0 {
				// Get the first return type
				firstReturn := signature.Fun.Args[0]
				firstReturn.Meta = m
				return m.extractTypeFromCallArgument(firstReturn)
			}
		}
	case KindCall:
		// For function calls, try to extract return type
		if signature.Fun != nil {
			signature.Fun.Meta = m
			return m.extractTypeFromCallArgument(*signature.Fun)
		}
	default:
		// For other types, try to extract type directly
		return m.extractTypeFromCallArgument(signature)
	}

	return ""
}

// extractTypeFromCallArgument extracts the type from a CallArgument
func (m *Metadata) extractTypeFromCallArgument(arg CallArgument) string {
	// Set the Meta reference
	arg.Meta = m

	switch arg.GetKind() {
	case KindIdent:
		// For identifiers, return the type if available
		if arg.Type != -1 {
			return arg.GetType()
		}
		return arg.GetName()
	case KindSelector:
		// For selectors, try to resolve the field type
		return m.resolveSelectorReturnType(arg, "")
	case KindCall:
		// For function calls, try to resolve the return type
		return m.resolveCallReturnType(arg, "")
	case KindCompositeLit:
		// For composite literals, resolve the type
		return m.resolveCompositeReturnType(arg, "")
	case KindUnary:
		// For unary expressions, resolve the underlying type
		return m.resolveUnaryReturnType(arg, "")
	case KindLiteral:
		// For literals, return the literal type
		return arg.GetType()
	case KindArrayType:
		// For array types, extract the element type
		if arg.X != nil {
			arg.X.Meta = m
			elementType := m.extractTypeFromCallArgument(*arg.X)
			if arg.GetValue() != "" {
				return "[" + arg.GetValue() + "]" + elementType
			}
			return "[]" + elementType
		}
		return "[]" + arg.GetType()
	case KindSlice:
		// For slice types, extract the element type
		if arg.X != nil {
			arg.X.Meta = m
			elementType := m.extractTypeFromCallArgument(*arg.X)
			return "[]" + elementType
		}
		return "[]" + arg.GetType()
	case KindMapType:
		// For map types, extract key and value types
		if arg.X != nil && arg.Fun != nil {
			arg.X.Meta = m
			arg.Fun.Meta = m
			keyType := m.extractTypeFromCallArgument(*arg.X)
			valueType := m.extractTypeFromCallArgument(*arg.Fun)
			if keyType != "" && valueType != "" {
				return "map[" + keyType + "]" + valueType
			}
		}
		// Try to get type from the Type field as fallback
		if arg.Type != -1 {
			return arg.GetType()
		}
		return "map"
	case KindStar:
		// For pointer types, add asterisk
		if arg.X != nil {
			arg.X.Meta = m
			baseType := m.extractTypeFromCallArgument(*arg.X)
			return "*" + baseType
		}
		return "*" + arg.GetType()
	default:
		// Fallback to the type field
		return arg.GetType()
	}
}

// determineResolvedTypeFromReturnVar determines the resolved type from a return variable
func (m *Metadata) determineResolvedTypeFromReturnVar(returnVar CallArgument, pkgName, contextName string) string {
	// Set the Meta reference
	returnVar.Meta = m

	switch returnVar.GetKind() {
	case KindIdent:
		// For identifiers, try to resolve the type through variable tracing
		return m.resolveIdentReturnType(returnVar, pkgName, contextName)
	case KindSelector:
		// For selectors, resolve the field type
		return m.resolveSelectorReturnType(returnVar, pkgName)
	case KindCall:
		// For function calls, resolve the return type
		return m.resolveCallReturnType(returnVar, pkgName)
	case KindCompositeLit:
		// For composite literals, resolve the type
		return m.resolveCompositeReturnType(returnVar, pkgName)
	case KindUnary, KindStar:
		// For unary expressions, resolve the underlying type
		return m.resolveUnaryReturnType(returnVar, pkgName)
	case KindLiteral:
		// For literals, return the literal type
		return returnVar.GetType()
	default:
		// Fallback to the type field
		return returnVar.GetType()
	}
}

// resolveIdentReturnType resolves the type of an identifier return value
func (m *Metadata) resolveIdentReturnType(returnVar CallArgument, pkgName, contextName string) string {
	varName := returnVar.GetName()

	// First, check if it's a variable in the current package
	if pkg, exists := m.Packages[pkgName]; exists {
		for _, file := range pkg.Files {
			// Check variables
			if variable, exists := file.Variables[varName]; exists {
				return m.StringPool.GetString(variable.Type)
			}

			// Check function assignments
			if fn, exists := file.Functions[contextName]; exists {
				if assignments, exists := fn.AssignmentMap[varName]; exists && len(assignments) > 0 {
					// Use the most recent assignment
					assign := assignments[len(assignments)-1]
					if assign.ConcreteType != -1 {
						return m.StringPool.GetString(assign.ConcreteType)
					}
					// Try to resolve from the assignment value
					assign.Value.Meta = m
					return m.determineResolvedTypeFromReturnVar(assign.Value, pkgName, contextName)
				}
			}
		}
	}

	// If not found, return the variable name as type
	return varName
}

// resolveSelectorReturnType resolves the type of a selector return value
func (m *Metadata) resolveSelectorReturnType(returnVar CallArgument, pkgName string) string {
	if returnVar.X == nil || returnVar.Sel == nil {
		return returnVar.GetType()
	}

	// Set Meta references
	returnVar.X.Meta = m
	returnVar.Sel.Meta = m

	baseType := m.determineResolvedTypeFromReturnVar(*returnVar.X, pkgName, "")
	fieldName := returnVar.Sel.GetName()

	// Try to find the field type in metadata
	for pkgName, pkg := range m.Packages {
		for _, file := range pkg.Files {
			// Try both with and without package prefix
			typeNames := []string{baseType, pkgName + "." + baseType}
			for _, typeName := range typeNames {
				if typ, exists := file.Types[typeName]; exists {
					// Find the field
					for _, field := range typ.Fields {
						if m.StringPool.GetString(field.Name) == fieldName {
							return m.StringPool.GetString(field.Type)
						}
					}
				}
			}
		}
	}

	// Fallback to concatenated form
	return baseType + "." + fieldName
}

// resolveCallReturnType resolves the type of a function call return value
func (m *Metadata) resolveCallReturnType(returnVar CallArgument, pkgName string) string {
	if returnVar.Fun == nil {
		return "func()"
	}

	// Set Meta reference
	returnVar.Fun.Meta = m

	// Try to determine return type from function signature
	funcType := m.determineResolvedTypeFromReturnVar(*returnVar.Fun, pkgName, "")

	// If it's a function type, extract return type
	if strings.HasPrefix(funcType, "func(") {
		// Simple extraction of return type
		if strings.Contains(funcType, ")") {
			parts := strings.Split(funcType, ")")
			if len(parts) > 1 {
				returnType := strings.TrimSpace(parts[1])
				if returnType != "" {
					return returnType
				}
			}
		}
	}

	return funcType
}

// resolveCompositeReturnType resolves the type of a composite literal return value
func (m *Metadata) resolveCompositeReturnType(returnVar CallArgument, pkgName string) string {
	if returnVar.X == nil {
		return returnVar.GetType()
	}

	// Set Meta reference
	returnVar.X.Meta = m

	// For composite literals, the type is usually in the X field
	return m.determineResolvedTypeFromReturnVar(*returnVar.X, pkgName, "")
}

// resolveUnaryReturnType resolves the type of a unary expression return value
func (m *Metadata) resolveUnaryReturnType(returnVar CallArgument, pkgName string) string {
	if returnVar.X == nil {
		return returnVar.GetType()
	}

	// Set Meta reference
	returnVar.X.Meta = m

	baseType := m.determineResolvedTypeFromReturnVar(*returnVar.X, pkgName, "")

	// Handle pointer dereferencing
	if strings.HasPrefix(returnVar.GetType(), "*") {
		// Dereference
		if after, ok := strings.CutPrefix(baseType, "*"); ok {
			return after
		}
		return baseType
	}

	// Add pointer
	return "*" + baseType
}

// processCallGraphReturnTypes processes call graph edges to set ResolvedType on function call arguments
func (m *Metadata) processCallGraphReturnTypes() {
	for i := range m.CallGraph {
		edge := &m.CallGraph[i]

		// Set Meta references
		edge.Caller.Meta = m
		edge.Callee.Meta = m

		// Process all arguments in the call
		for j := range edge.Args {
			arg := &edge.Args[j]
			arg.Meta = m

			// If this is a function call, try to resolve its return type
			if arg.GetKind() == KindCall {
				m.processFunctionCallReturnType(arg)
			}
		}
	}
}

// processFunctionCallReturnType processes a function call argument to set its ResolvedType
func (m *Metadata) processFunctionCallReturnType(arg *CallArgument) {
	if arg.Fun == nil {
		return
	}

	// Set Meta reference
	arg.Fun.Meta = m

	// Try to find the function being called
	funcName := arg.Fun.GetName()
	if funcName == "" {
		return
	}

	// Look for the function in metadata
	for _, pkg := range m.Packages {
		for _, file := range pkg.Files {
			// Check functions
			if fn, exists := file.Functions[funcName]; exists {
				if fn.Signature.ResolvedType != -1 {
					arg.SetResolvedType(fn.Signature.GetResolvedType())
				}
				return
			}

			// Check methods
			for _, typ := range file.Types {
				for _, method := range typ.Methods {
					if m.StringPool.GetString(method.Name) == funcName {
						if method.Signature.ResolvedType != -1 {
							arg.SetResolvedType(method.Signature.GetResolvedType())
						}
						return
					}
				}
			}
		}
	}
}

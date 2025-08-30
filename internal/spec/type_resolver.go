package spec

import (
	"fmt"
	"strings"

	"github.com/ehabterra/swagen/internal/metadata"
)

// TypeResolverImpl implements TypeResolver
type TypeResolverImpl struct {
	meta         *metadata.Metadata
	cfg          *SwagenConfig
	schemaMapper SchemaMapper
}

// NewTypeResolver creates a new type resolver
func NewTypeResolver(meta *metadata.Metadata, cfg *SwagenConfig, schemaMapper SchemaMapper) *TypeResolverImpl {
	return &TypeResolverImpl{
		meta:         meta,
		cfg:          cfg,
		schemaMapper: schemaMapper,
	}
}

// ResolveType resolves a Go type to its concrete type, handling generics and type parameters
func (t *TypeResolverImpl) ResolveType(arg metadata.CallArgument, context TrackerNodeInterface) string {
	// Handle the case where context is an interface containing a nil pointer
	if context == nil {
		return t.resolveTypeFromArgument(arg)
	}

	// Check if the edge is nil
	if context.GetEdge() == nil {
		return t.resolveTypeFromArgument(arg)
	}

	// First, try to resolve type parameters from the call graph edge
	if resolvedType := t.resolveTypeParameter(arg, context); resolvedType != "" {
		return resolvedType
	}

	// Then try to resolve through variable tracing
	if resolvedType := t.resolveTypeThroughTracing(arg, context); resolvedType != "" {
		return resolvedType
	}

	// Fallback to direct argument resolution
	return t.resolveTypeFromArgument(arg)
}

// resolveTypeParameter resolves type parameters from call graph edges
func (t *TypeResolverImpl) resolveTypeParameter(arg metadata.CallArgument, node TrackerNodeInterface) string {
	// Check if this argument corresponds to a type parameter
	for paramName, concreteType := range node.GetTypeParamMap() {
		if arg.GetName() == paramName {
			return concreteType
		}
	}

	// Check if this argument is mapped to a parameter
	if node.GetEdge() != nil {
		if paramArg, exists := node.GetEdge().ParamArgMap[arg.GetName()]; exists {
			return t.resolveTypeFromArgument(paramArg)
		}
	}

	return ""
}

// resolveTypeThroughTracing resolves type through variable tracing
func (t *TypeResolverImpl) resolveTypeThroughTracing(arg metadata.CallArgument, context TrackerNodeInterface) string {
	if arg.GetKind() != metadata.KindIdent {
		return ""
	}

	// Use metadata.TraceVariableOrigin to trace the variable
	originVar, _, originType, _ := metadata.TraceVariableOrigin(
		arg.GetName(),
		t.getCallerName(context),
		t.getCallerPkg(context),
		t.meta,
	)

	// If we found an origin type, use it
	if originType != nil {
		return t.resolveTypeFromArgument(*originType)
	}

	// If the origin variable is different, try to resolve it
	if originVar != arg.GetName() {
		return originVar
	}

	return ""
}

// resolveTypeFromArgument resolves type directly from a CallArgument
func (t *TypeResolverImpl) resolveTypeFromArgument(arg metadata.CallArgument) string {
	switch arg.GetKind() {
	case metadata.KindIdent:
		return t.resolveIdentType(arg)
	case metadata.KindSelector:
		return t.resolveSelectorType(arg)
	case metadata.KindCall:
		return t.resolveCallType(arg)
	case metadata.KindUnary, metadata.KindStar:
		return t.resolveUnaryType(arg)
	case metadata.KindCompositeLit:
		return t.resolveCompositeType(arg)
	case metadata.KindIndex:
		return t.resolveIndexType(arg)
	case metadata.KindInterfaceType:
		return "interface{}"
	case metadata.KindMapType:
		return t.resolveMapType(arg)
	case metadata.KindLiteral:
		return arg.GetType()
	case metadata.KindRaw:
		return arg.GetRaw()
	default:
		return arg.GetType()
	}
}

// resolveIdentType resolves type for identifier arguments
func (t *TypeResolverImpl) resolveIdentType(arg metadata.CallArgument) string {
	// If we have a direct type, use it
	if arg.Type != -1 {
		return arg.GetType()
	}

	// Try to find the variable in metadata
	if arg.Pkg != -1 {
		pkgName := arg.GetPkg()
		if pkg, exists := t.meta.Packages[pkgName]; exists {
			for _, file := range pkg.Files {
				varName := arg.GetName()
				if variable, exists := file.Variables[varName]; exists {
					return t.getString(variable.Type)
				}
			}
		}
	}

	// Fallback to name
	return arg.GetName()
}

// resolveSelectorType resolves type for selector expressions
func (t *TypeResolverImpl) resolveSelectorType(arg metadata.CallArgument) string {
	if arg.X == nil {
		return arg.Sel.GetName()
	}

	baseType := t.resolveTypeFromArgument(*arg.X)
	if baseType == "" {
		return arg.Sel.GetName()
	}

	// For field access, try to find the field type in metadata
	for pkgName, pkg := range t.meta.Packages {
		for _, file := range pkg.Files {
			// Try both with and without package prefix
			typeNames := []string{baseType, pkgName + "." + baseType}
			for _, typeName := range typeNames {
				if typ, exists := file.Types[typeName]; exists {
					// Find the field
					for _, field := range typ.Fields {
						if t.getString(field.Name) == arg.Sel.GetName() {
							return t.getString(field.Type)
						}
					}
				}
			}
		}
	}

	// Fallback to concatenated form
	return baseType + "." + arg.Sel.GetName()
}

// resolveCallType resolves type for function calls
func (t *TypeResolverImpl) resolveCallType(arg metadata.CallArgument) string {
	if arg.Fun == nil {
		return "func()"
	}

	// Try to determine return type from function signature
	funcType := t.resolveTypeFromArgument(*arg.Fun)

	// If it's a function type, extract return type
	if strings.HasPrefix(funcType, "func(") {
		// Simple extraction of return type
		// This could be enhanced with proper parsing
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

// resolveUnaryType resolves type for unary expressions
func (t *TypeResolverImpl) resolveUnaryType(arg metadata.CallArgument) string {
	if arg.X == nil {
		return "*" + arg.GetType()
	}

	baseType := t.resolveTypeFromArgument(*arg.X)
	if after, ok := strings.CutPrefix(baseType, "*"); ok {
		// Dereference
		return after
	}

	// Add pointer
	return "*" + baseType
}

// resolveCompositeType resolves type for composite literals
func (t *TypeResolverImpl) resolveCompositeType(arg metadata.CallArgument) string {
	if arg.X == nil {
		return metadata.KindCompositeLit
	}

	baseType := t.resolveTypeFromArgument(*arg.X)
	if baseType == "" {
		return metadata.KindCompositeLit
	}

	return baseType
}

// resolveIndexType resolves type for index expressions
func (t *TypeResolverImpl) resolveIndexType(arg metadata.CallArgument) string {
	if arg.X == nil {
		return metadata.KindIndex
	}

	baseType := t.resolveTypeFromArgument(*arg.X)
	if strings.HasPrefix(baseType, "[]") {
		// For slices, return element type
		return strings.TrimPrefix(baseType, "[]")
	}

	if strings.HasPrefix(baseType, "map[") {
		// For maps, return value type
		endIdx := strings.Index(baseType, "]")
		if endIdx > 4 {
			valueType := strings.TrimSpace(baseType[endIdx+1:])
			return valueType
		}
	}

	return baseType
}

// resolveMapType resolves type for map expressions
func (t *TypeResolverImpl) resolveMapType(arg metadata.CallArgument) string {
	if arg.X == nil || arg.Fun == nil {
		return "map"
	}

	keyType := t.resolveTypeFromArgument(*arg.X)
	valueType := t.resolveTypeFromArgument(*arg.Fun)

	return fmt.Sprintf("map[%s]%s", keyType, valueType)
}

// MapToOpenAPISchema maps a Go type to OpenAPI schema
func (t *TypeResolverImpl) MapToOpenAPISchema(goType string) *Schema {
	return t.schemaMapper.MapGoTypeToOpenAPISchema(goType)
}

// Helper methods

func (t *TypeResolverImpl) getString(idx int) string {
	if t.meta == nil || t.meta.StringPool == nil {
		return ""
	}
	return t.meta.StringPool.GetString(idx)
}

func (t *TypeResolverImpl) getCallerName(context TrackerNodeInterface) string {
	if context == nil || context.GetEdge() == nil {
		return ""
	}
	return t.getString(context.GetEdge().Caller.Name)
}

func (t *TypeResolverImpl) getCallerPkg(context TrackerNodeInterface) string {
	if context == nil || context.GetEdge() == nil {
		return ""
	}
	return t.getString(context.GetEdge().Caller.Pkg)
}

// ResolveGenericType resolves a generic type with concrete type parameters
func (t *TypeResolverImpl) ResolveGenericType(genericType string, typeParams map[string]string) string {
	if len(typeParams) == 0 {
		// If no type parameters provided, clean up empty brackets
		baseType, paramStr := t.extractBaseTypeAndParams(genericType)
		if baseType == "" {
			return genericType
		}

		// If the parameter string is empty or just whitespace/brackets, return just the base type
		if strings.TrimSpace(paramStr) == "" || strings.TrimSpace(paramStr) == "[]" {
			return baseType
		}

		return genericType
	}

	// Extract the base type name and parameters
	baseType, paramStr := t.extractBaseTypeAndParams(genericType)
	if baseType == "" {
		return genericType
	}

	// Handle empty parameters
	if paramStr == "" {
		return baseType
	}

	// Check if the parameter string is just whitespace
	if strings.TrimSpace(paramStr) == "" {
		return baseType
	}

	// Check if the parameter string is just "[]" or empty
	if strings.TrimSpace(paramStr) == "" {
		return baseType
	}

	// Check if the parameter string is just "[]"
	if strings.TrimSpace(paramStr) == "[]" {
		return baseType
	}

	// Split the parameters and replace them one by one
	paramList := t.splitTypeParameters(paramStr)
	var resolvedParams []string

	for _, param := range paramList {
		param = strings.TrimSpace(param)
		if param == "" {
			continue
		}

		// Check if this parameter is itself a generic type that needs resolution
		if strings.Contains(param, "[") && strings.Contains(param, "]") {
			// For nested generics, recursively resolve them
			resolvedParam := t.ResolveGenericType(param, typeParams)
			resolvedParams = append(resolvedParams, resolvedParam)
		} else {
			// Try to find a concrete type by parameter name
			concreteType := t.findConcreteTypeByName(param, typeParams)
			if concreteType != "" {
				resolvedParams = append(resolvedParams, concreteType)
			} else {
				// Keep original parameter if no concrete type provided
				resolvedParams = append(resolvedParams, param)
			}
		}
	}

	// Reconstruct the resolved type
	if len(resolvedParams) > 0 {
		return baseType + "[" + strings.Join(resolvedParams, ",") + "]"
	}

	return baseType
}

// findConcreteTypeByName finds a concrete type by parameter name
func (t *TypeResolverImpl) findConcreteTypeByName(paramName string, typeParams map[string]string) string {
	// First try exact match
	if concreteType, exists := typeParams[paramName]; exists {
		return concreteType
	}

	// If no exact match, try to extract the parameter name from complex parameters
	extractedName := t.extractParameterName(paramName)
	if extractedName != "" && extractedName != paramName {
		if concreteType, exists := typeParams[extractedName]; exists {
			return concreteType
		}
	}

	return ""
}

// extractBaseTypeAndParams extracts the base type name and parameter string
func (t *TypeResolverImpl) extractBaseTypeAndParams(genericType string) (string, string) {
	start := strings.Index(genericType, "[")
	if start == -1 {
		return genericType, ""
	}

	end := strings.LastIndex(genericType, "]")
	if end == -1 || end <= start {
		return genericType, ""
	}

	baseType := strings.TrimSpace(genericType[:start])
	paramStr := strings.TrimSpace(genericType[start+1 : end])

	return baseType, paramStr
}

// extractParameterName extracts the parameter name from a parameter string
func (t *TypeResolverImpl) extractParameterName(param string) string {
	// For simple parameters like "K", "V", "T", just return as is
	if len(param) == 1 && strings.Contains("ABCDEFGHIJKLMNOPQRSTUVWXYZ", strings.ToUpper(param)) {
		return param
	}

	// For nested parameters like "List[V]", extract the nested parameter
	if strings.Contains(param, "[") && strings.Contains(param, "]") {
		// Extract the parameter inside the brackets
		start := strings.Index(param, "[")
		end := strings.LastIndex(param, "]")
		if start < end {
			nestedParam := param[start+1 : end]
			// Recursively extract parameter name from nested parameter
			return t.extractParameterName(nestedParam)
		}
	}

	// For more complex parameters, try to extract the first identifier
	// This is a simplified approach - could be enhanced with proper parsing
	words := strings.Fields(param)
	if len(words) > 0 {
		firstWord := words[0]
		if len(firstWord) == 1 && strings.Contains("ABCDEFGHIJKLMNOPQRSTUVWXYZ", strings.ToUpper(firstWord)) {
			return firstWord
		}
	}

	return param
}

// ExtractTypeParameters extracts type parameters from a generic type
func (t *TypeResolverImpl) ExtractTypeParameters(genericType string) map[string]string {
	params := make(map[string]string)

	// Find the type parameter section
	if !strings.Contains(genericType, "[") || !strings.Contains(genericType, "]") {
		return params
	}

	start := strings.Index(genericType, "[")
	end := strings.LastIndex(genericType, "]")
	if start >= end {
		return params
	}

	paramStr := genericType[start+1 : end]
	paramStr = strings.TrimSpace(paramStr)

	// Handle empty parameters
	if paramStr == "" {
		return params
	}

	// Parse multiple type parameters
	params = t.parseTypeParameterList(paramStr)

	return params
}

// parseTypeParameterList parses a comma-separated list of type parameters
func (t *TypeResolverImpl) parseTypeParameterList(paramStr string) map[string]string {
	params := make(map[string]string)

	// Split by comma, but handle nested brackets
	paramList := t.splitTypeParameters(paramStr)

	for i, param := range paramList {
		param = strings.TrimSpace(param)
		if param == "" {
			continue
		}

		// Generate parameter name (T, U, V, etc.)
		paramName := t.generateParameterName(i)
		params[paramName] = param
	}

	return params
}

// splitTypeParameters splits a type parameter string by commas, respecting nested brackets
func (t *TypeResolverImpl) splitTypeParameters(paramStr string) []string {
	var result []string
	var current strings.Builder
	var bracketCount int

	for _, char := range paramStr {
		switch char {
		case '[':
			bracketCount++
			current.WriteRune(char)
		case ']':
			bracketCount--
			current.WriteRune(char)
		case ',':
			if bracketCount == 0 {
				// Only split on comma if we're not inside brackets
				result = append(result, current.String())
				current.Reset()
			} else {
				current.WriteRune(char)
			}
		default:
			current.WriteRune(char)
		}
	}

	// Add the last parameter
	if current.Len() > 0 {
		result = append(result, current.String())
	}

	return result
}

// generateParameterName generates a parameter name based on index
func (t *TypeResolverImpl) generateParameterName(index int) string {
	// Use single letters: T, U, V, W, X, Y, Z, then T1, U1, etc.
	if index < 26 {
		return string(rune('T' + index))
	}
	// For more than 26 parameters, use T1, U1, etc.
	base := index % 26
	number := index / 26
	if number == 0 {
		return string(rune('T' + base))
	}
	return fmt.Sprintf("%c%d", rune('T'+base), number)
}

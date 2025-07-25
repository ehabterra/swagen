package spec

import (
	"testing"

	"go/ast"
	"go/parser"
	"go/token"
	"go/types"

	"github.com/ehabterra/swagen/internal/metadata"
)

func TestExtractQueryParameters(t *testing.T) {
	// Create a simple metadata with a handler that uses r.URL.Query().Get()
	stringPool := metadata.NewStringPool()
	meta := &metadata.Metadata{
		StringPool: stringPool,
		Packages: map[string]*metadata.Package{
			"main": {
				Files: map[string]*metadata.File{
					"handler.go": {
						Functions: map[string]*metadata.Function{
							"getUserHandler": {
								Name: stringPool.Get("getUserHandler"),
							},
						},
					},
				},
			},
		},
	}

	// Create a TrackerTree with a route node and a parameter node
	tree := &TrackerTree{
		meta: meta,
		roots: []*TrackerNode{
			{
				CallGraphEdge: &metadata.CallGraphEdge{
					Callee: metadata.Call{
						Name: stringPool.Get("GET"),
						Pkg:  stringPool.Get("net/http"),
					},
					Caller: metadata.Call{
						Name: stringPool.Get("getUserHandler"),
						Pkg:  stringPool.Get("main"),
					},
					Args: []metadata.CallArgument{
						{
							Kind:  "literal",
							Value: "\"/users\"",
						},
						{
							Kind: "ident",
							Name: "getUserHandler",
						},
					},
				},
				children: []*TrackerNode{
					{
						CallGraphEdge: &metadata.CallGraphEdge{
							Callee: metadata.Call{
								Name: stringPool.Get("Get"),
								Pkg:  stringPool.Get("net/http"),
							},
							Caller: metadata.Call{
								Name: stringPool.Get("getUserHandler"),
								Pkg:  stringPool.Get("main"),
							},
							Args: []metadata.CallArgument{
								{
									Kind:  "literal",
									Value: "\"user_id\"",
								},
							},
						},
					},
				},
			},
		},
	}

	// Create a config with route and parameter patterns
	cfg := &SwagenConfig{
		Framework: FrameworkConfig{
			RoutePatterns: []RoutePattern{
				{
					CallRegex:       `(?i)(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD)$`,
					MethodFromCall:  true,
					PathFromArg:     true,
					HandlerFromArg:  true,
					PathArgIndex:    0,
					HandlerArgIndex: 1,
				},
			},
			ParamPatterns: []ParamPattern{
				{
					CallRegex:     "^Get$",
					ParamIn:       "query",
					ParamArgIndex: 0,
					RecvType:      "url.Values",
				},
			},
		},
	}

	// Create extractor and extract routes
	extractor := NewExtractor(tree, cfg)
	routes := extractor.ExtractRoutes()

	// Verify that the parameter was extracted
	if len(routes) == 0 {
		t.Fatal("No routes extracted")
	}

	route := routes[0]
	if len(route.Params) == 0 {
		t.Fatal("No parameters extracted")
	}

	param := route.Params[0]
	if param.Name != "user_id" {
		t.Errorf("Expected parameter name 'user_id', got '%s'", param.Name)
	}
	if param.In != "query" {
		t.Errorf("Expected parameter location 'query', got '%s'", param.In)
	}
}

func TestExtractParameterDetailsFromNode_DefaultSchema(t *testing.T) {
	// Create a simple metadata
	stringPool := metadata.NewStringPool()
	meta := &metadata.Metadata{
		StringPool: stringPool,
		Packages:   make(map[string]*metadata.Package),
	}

	// Create a tracker tree with a simple node
	tree := &TrackerTree{
		meta: meta,
	}

	// Create a config with a parameter pattern that doesn't specify TypeFromArg
	cfg := &SwagenConfig{
		Framework: FrameworkConfig{
			ParamPatterns: []ParamPattern{
				{
					CallRegex:     "Get",
					ParamIn:       "query",
					ParamArgIndex: 0,
					// Note: TypeFromArg is false, so no type will be extracted
				},
			},
		},
	}

	extractor := NewExtractor(tree, cfg)

	// Create a mock node
	node := &TrackerNode{
		CallGraphEdge: &metadata.CallGraphEdge{
			Callee: metadata.Call{
				Name: stringPool.Get("Get"),
			},
			Args: []metadata.CallArgument{
				{
					Kind:  "literal",
					Value: "x-requested-from",
				},
			},
		},
	}

	// Extract parameter details
	param := extractor.extractParameterDetailsFromNode(node, cfg.Framework.ParamPatterns[0])

	// Verify the parameter has a schema
	if param == nil {
		t.Fatal("expected parameter to be extracted")
	}

	if param.Name != "x-requested-from" {
		t.Errorf("expected parameter name 'x-requested-from', got '%s'", param.Name)
	}

	if param.In != "query" {
		t.Errorf("expected parameter in 'query', got '%s'", param.In)
	}

	// This is the key test - verify that a schema is assigned even without a type
	if param.Schema == nil {
		t.Fatal("expected parameter to have a schema")
	}

	if param.Schema.Type != "string" {
		t.Errorf("expected schema type 'string', got '%s'", param.Schema.Type)
	}
}

// Helper to map callerName to HTTP method
func methodForCaller(callerName string) string {
	switch callerName {
	case "getUserHandler":
		return "GET"
	case "createUserHandler":
		return "POST"
	case "headUserHandler":
		return "HEAD"
	default:
		return ""
	}
}

func TestMatchRequestBodyPattern_ContextAware(t *testing.T) {
	// Create a simple metadata
	stringPool := metadata.NewStringPool()
	meta := &metadata.Metadata{
		StringPool: stringPool,
		Packages:   make(map[string]*metadata.Package),
	}

	// Create a tracker tree with a simple node
	tree := &TrackerTree{
		meta: meta,
	}

	// Create a config with request body patterns
	cfg := &SwagenConfig{
		Framework: FrameworkConfig{
			RequestBodyPatterns: []RequestBodyPattern{
				{
					CallRegex:    `^Unmarshal$`,
					TypeArgIndex: 0,
					TypeFromArg:  true,
					Deref:        true,
				},
				{
					CallRegex:    `^Decode$`,
					TypeArgIndex: 0,
					TypeFromArg:  true,
					Deref:        true,
				},
			},
		},
	}

	extractor := NewExtractor(tree, cfg)

	tests := []struct {
		name          string
		callerName    string
		calleeName    string
		expectedMatch bool
		description   string
	}{
		{
			name:          "GET method with Unmarshal - should not match",
			callerName:    "getUserHandler",
			calleeName:    "Unmarshal",
			expectedMatch: false,
			description:   "GET methods should not match request body patterns unless explicitly allowed",
		},
		{
			name:          "POST method with Unmarshal - should match",
			callerName:    "createUserHandler",
			calleeName:    "Unmarshal",
			expectedMatch: true,
			description:   "POST methods should match request body patterns",
		},
		{
			name:          "GET method with Decode - should not match",
			callerName:    "getUserHandler",
			calleeName:    "Decode",
			expectedMatch: false,
			description:   "GET methods should not match request body patterns unless explicitly allowed",
		},
		{
			name:          "HEAD method with Unmarshal - should not match",
			callerName:    "headUserHandler",
			calleeName:    "Unmarshal",
			expectedMatch: false,
			description:   "HEAD methods should not match request body patterns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a mock node
			node := &TrackerNode{
				CallGraphEdge: &metadata.CallGraphEdge{
					Caller: metadata.Call{
						Name: stringPool.Get(tt.callerName),
					},
					Callee: metadata.Call{
						Name: stringPool.Get(tt.calleeName),
					},
					Args: []metadata.CallArgument{
						{
							Kind:  "literal",
							Value: "test",
						},
					},
				},
			}

			// Only check the relevant pattern for the callee
			patternIdx := 0
			if tt.calleeName == "Decode" {
				patternIdx = 1
			}
			pattern := cfg.Framework.RequestBodyPatterns[patternIdx]
			route := &RouteInfo{Method: methodForCaller(tt.callerName)}
			matched := extractor.matchRequestBodyPattern(node, pattern, route)
			if matched != tt.expectedMatch {
				t.Errorf("Pattern %d (%s): expected match=%v, got=%v. %s",
					patternIdx, pattern.CallRegex, tt.expectedMatch, matched, tt.description)
			}
		})
	}
}

func TestExtractQueryParameters_RecvTypeRegex(t *testing.T) {
	stringPool := metadata.NewStringPool()
	meta := &metadata.Metadata{
		StringPool: stringPool,
		Packages: map[string]*metadata.Package{
			"main": {
				Files: map[string]*metadata.File{
					"handler.go": {
						Functions: map[string]*metadata.Function{
							"getUserHandler": {
								Name: stringPool.Get("getUserHandler"),
							},
						},
					},
				},
			},
		},
	}
	tree := &TrackerTree{
		meta: meta,
		roots: []*TrackerNode{
			{
				CallGraphEdge: &metadata.CallGraphEdge{
					Callee: metadata.Call{
						Name:     stringPool.Get("GET"),
						Pkg:      stringPool.Get("net/http"),
						RecvType: stringPool.Get("*custom.Values"),
					},
					Caller: metadata.Call{
						Name: stringPool.Get("getUserHandler"),
						Pkg:  stringPool.Get("main"),
					},
					Args: []metadata.CallArgument{
						{
							Kind:  "literal",
							Value: "\"/users\"",
						},
						{
							Kind: "ident",
							Name: "getUserHandler",
						},
					},
				},
				children: []*TrackerNode{
					{
						CallGraphEdge: &metadata.CallGraphEdge{
							Callee: metadata.Call{
								Name:     stringPool.Get("Get"),
								Pkg:      stringPool.Get("net/http"),
								RecvType: stringPool.Get("*custom.Values"),
							},
							Caller: metadata.Call{
								Name: stringPool.Get("getUserHandler"),
								Pkg:  stringPool.Get("main"),
							},
							Args: []metadata.CallArgument{
								{
									Kind:  "literal",
									Value: "\"user_id\"",
								},
							},
						},
					},
				},
			},
		},
	}
	cfg := &SwagenConfig{
		Framework: FrameworkConfig{
			RoutePatterns: []RoutePattern{
				{
					CallRegex:       `(?i)(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD)$`,
					MethodFromCall:  true,
					PathFromArg:     true,
					HandlerFromArg:  true,
					PathArgIndex:    0,
					HandlerArgIndex: 1,
				},
			},
			ParamPatterns: []ParamPattern{
				{
					CallRegex:     "^Get$",
					ParamIn:       "query",
					ParamArgIndex: 0,
					RecvTypeRegex: ".*custom\\.Values$",
				},
			},
		},
	}
	extractor := NewExtractor(tree, cfg)
	routes := extractor.ExtractRoutes()
	if len(routes) == 0 {
		t.Fatal("No routes extracted")
	}
	route := routes[0]
	if len(route.Params) == 0 {
		t.Fatal("No parameters extracted")
	}
	param := route.Params[0]
	if param.Name != "user_id" {
		t.Errorf("Expected parameter name 'user_id', got '%s'", param.Name)
	}
	if param.In != "query" {
		t.Errorf("Expected parameter location 'query', got '%s'", param.In)
	}
}

func TestRouteGroupPrefixLinking_TableDriven(t *testing.T) {
	tests := []struct {
		desc        string
		src         string
		expectPath  string
		expectGroup string
	}{
		{
			desc: "Simple route, no group",
			src: `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.New()
	r.GET("/users", handler)
}
func handler(c *gin.Context) {}`,
			expectPath:  "/users",
			expectGroup: "",
		},
		{
			desc: "Route with single group prefix",
			src: `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.New()
	g := r.Group("/api")
	g.GET("/users", handler)
}
func handler(c *gin.Context) {}`,
			expectPath:  "/api/users",
			expectGroup: "/api",
		},
		{
			desc: "Route with nested group prefixes",
			src: `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.New()
	g := r.Group("/api")
	sub := g.Group("/v1")
	sub.GET("/users", handler)
}
func handler(c *gin.Context) {}`,
			expectPath:  "/v1/users", // Note: Only the innermost group is currently supported unless recursive prefixing is implemented
			expectGroup: "/v1",
		},
		{
			desc: "Route with alias group variable",
			src: `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.New()
	g := r.Group("/api")
	h := g
	h.GET("/users", handler)
}
func handler(c *gin.Context) {}`,
			expectPath:  "/api/users",
			expectGroup: "/api",
		},
		{
			desc: "Route with group variable reassignment",
			src: `package main
import "github.com/gin-gonic/gin"
func main() {
	r := gin.New()
	g := r.Group("/api")
	g = g.Group("/v2")
	g.GET("/users", handler)
}
func handler(c *gin.Context) {}`,
			expectPath:  "/v2/users",
			expectGroup: "/v2",
		},
	}

	for _, tc := range tests {
		t.Run(tc.desc, func(t *testing.T) {
			// Parse the source and build metadata
			fset, pkgs, importPaths, fileToInfo := parseGoSourceForTest(tc.src)
			meta := metadata.GenerateMetadata(pkgs, fileToInfo, importPaths, fset)
			tree := BuildTrackerTree(meta) // Assume this builds a TrackerTree from metadata
			extractor := NewExtractor(tree, minimalGinConfig())
			routes := extractor.ExtractRoutes()
			if len(routes) == 0 {
				t.Fatal("No routes extracted")
			}
			route := routes[0]
			if route.Path != tc.expectPath {
				t.Errorf("expected path %q, got %q", tc.expectPath, route.Path)
			}
			if route.GroupPrefix != tc.expectGroup {
				t.Errorf("expected group prefix %q, got %q", tc.expectGroup, route.GroupPrefix)
			}
		})
	}
}

// minimalGinConfig returns a minimal config for Gin route extraction
func minimalGinConfig() *SwagenConfig {
	return &SwagenConfig{
		Framework: FrameworkConfig{
			RoutePatterns: []RoutePattern{{
				CallRegex:       `(?i)(GET|POST|PUT|DELETE|PATCH|OPTIONS|HEAD)$`,
				MethodFromCall:  true,
				PathFromArg:     true,
				HandlerFromArg:  true,
				PathArgIndex:    0,
				HandlerArgIndex: 1,
				RecvTypeRegex:   "^github.com/gin-gonic/gin.\\*(Engine|RouterGroup)$",
			}},
			MountPatterns: []MountPattern{{
				CallRegex:     `^Group$`,
				PathFromArg:   true,
				RouterFromArg: true,
				PathArgIndex:  0,
				IsMount:       true,
				RecvTypeRegex: "^github.com/gin-gonic/gin.\\*(Engine|RouterGroup)$",
			}},
		},
	}
}

// parseGoSourceForTest parses Go source and returns fset, pkgs, importPaths, fileToInfo for testing
func parseGoSourceForTest(src string) (*token.FileSet, map[string]map[string]*ast.File, map[string]string, map[*ast.File]*types.Info) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "test.go", src, parser.AllErrors)
	if err != nil {
		panic(err)
	}
	pkgs := map[string]map[string]*ast.File{"main": {"test.go": file}}
	importPaths := map[string]string{"main": "main"}
	fileToInfo := map[*ast.File]*types.Info{}
	return fset, pkgs, importPaths, fileToInfo
}

// BuildTrackerTree is a minimal mock that attaches meta to a TrackerTree for testing
func BuildTrackerTree(meta *metadata.Metadata) *TrackerTree {
	return &TrackerTree{meta: meta}
}

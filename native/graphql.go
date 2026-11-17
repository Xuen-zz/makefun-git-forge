package main

import (
	_ "embed"
	"encoding/json"
	"fmt"

	graphql "github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
)

//go:embed schema.graphql
var schemaSource string

type querySchema struct {
	schema graphql.Schema
	root   mapJSON
}
type gqlField func(mapJSON) any

func newQuerySchema(s *store) *querySchema {
	doc, err := parser.Parse(parser.ParseParams{Source: schemaSource})
	if err != nil {
		panic(err)
	}
	types := map[string]graphql.Type{"String": graphql.String, "Boolean": graphql.Boolean, "Int": graphql.Int, "ID": graphql.ID, "Float": graphql.Float}
	var resolve func(ast.Type) graphql.Type
	resolve = func(t ast.Type) graphql.Type {
		switch x := t.(type) {
		case *ast.Named:
			return types[x.Name.Value]
		case *ast.List:
			return graphql.NewList(resolve(x.Type))
		case *ast.NonNull:
			return graphql.NewNonNull(resolve(x.Type))
		}
		panic("unknown schema type")
	}
	resolveField := func(p graphql.ResolveParams) (value any, err error) {
		defer func() {
			if failure := recover(); failure != nil {
				if e, ok := failure.(apiError); ok {
					err = e
					return
				}
				panic(failure)
			}
		}()
		m := objectVal(p.Source)
		if field, ok := m[p.Info.FieldName].(gqlField); ok {
			value := field(p.Args)
			if object, isObject := value.(mapJSON); isObject && object == nil {
				return nil, nil
			}
			return value, nil
		}
		return m[p.Info.FieldName], nil
	}
	fields := func(source []*ast.FieldDefinition) graphql.Fields {
		out := graphql.Fields{}
		for _, f := range source {
			args := graphql.FieldConfigArgument{}
			for _, a := range f.Arguments {
				args[a.Name.Value] = &graphql.ArgumentConfig{Type: resolve(a.Type).(graphql.Input)}
			}
			out[f.Name.Value] = &graphql.Field{Type: resolve(f.Type).(graphql.Output), Args: args, Resolve: resolveField}
		}
		return out
	}
	typeOf := func(p graphql.ResolveTypeParams) *graphql.Object {
		value, _ := types[stringVal(objectVal(p.Value)["__typename"])].(*graphql.Object)
		return value
	}
	for _, definition := range doc.Definitions {
		switch d := definition.(type) {
		case *ast.ScalarDefinition:
			if d.Name.Value == "DateTime" {
				types[d.Name.Value] = graphql.NewScalar(graphql.ScalarConfig{Name: d.Name.Value, Serialize: func(v any) any { return graphql.String.Serialize(v) }})
			} else {
				types[d.Name.Value] = graphql.NewScalar(graphql.ScalarConfig{Name: d.Name.Value, Serialize: graphql.ID.Serialize, ParseValue: graphql.ID.ParseValue, ParseLiteral: graphql.ID.ParseLiteral})
			}
		case *ast.EnumDefinition:
			values := graphql.EnumValueConfigMap{}
			for _, x := range d.Values {
				values[x.Name.Value] = &graphql.EnumValueConfig{Value: x.Name.Value}
			}
			types[d.Name.Value] = graphql.NewEnum(graphql.EnumConfig{Name: d.Name.Value, Values: values})
		case *ast.ObjectDefinition:
			types[d.Name.Value] = graphql.NewObject(graphql.ObjectConfig{Name: d.Name.Value,
				Fields: graphql.FieldsThunk(func() graphql.Fields { return fields(d.Fields) }),
				Interfaces: graphql.InterfacesThunk(func() []*graphql.Interface {
					result := make([]*graphql.Interface, 0, len(d.Interfaces))
					for _, i := range d.Interfaces {
						result = append(result, types[i.Name.Value].(*graphql.Interface))
					}
					return result
				})})
		case *ast.InterfaceDefinition:
			types[d.Name.Value] = graphql.NewInterface(graphql.InterfaceConfig{Name: d.Name.Value, ResolveType: typeOf,
				Fields: graphql.FieldsThunk(func() graphql.Fields { return fields(d.Fields) })})
		case *ast.InputObjectDefinition:
			types[d.Name.Value] = graphql.NewInputObject(graphql.InputObjectConfig{Name: d.Name.Value,
				Fields: graphql.InputObjectConfigFieldMapThunk(func() graphql.InputObjectConfigFieldMap {
					out := graphql.InputObjectConfigFieldMap{}
					for _, f := range d.Fields {
						out[f.Name.Value] = &graphql.InputObjectFieldConfig{Type: resolve(f.Type).(graphql.Input)}
					}
					return out
				})})
		case *ast.UnionDefinition:
			types[d.Name.Value] = graphql.NewUnion(graphql.UnionConfig{Name: d.Name.Value, ResolveType: typeOf,
				Types: graphql.UnionTypesThunk(func() []*graphql.Object {
					out := make([]*graphql.Object, 0, len(d.Types))
					for _, t := range d.Types {
						out = append(out, types[t.Name.Value].(*graphql.Object))
					}
					return out
				})})
		default:
			panic(fmt.Sprintf("unexpected schema declaration %T", definition))
		}
	}
	all := make([]graphql.Type, 0, len(types))
	for _, t := range types {
		all = append(all, t)
	}
	schema, err := graphql.NewSchema(graphql.SchemaConfig{Query: types["Query"].(*graphql.Object), Mutation: types["Mutation"].(*graphql.Object), Types: all})
	if err != nil {
		panic(err)
	}
	return &querySchema{schema, s.queryRoot()}
}
func graphError(message string) any { return mapJSON{"errors": []mapJSON{{"message": message}}} }
func (g *querySchema) execute(input any) any {
	row := objectVal(input)
	query, ok := row["query"].(string)
	if !ok {
		return graphError("A GraphQL query is required")
	}
	vars := objectVal(row["variables"])
	if row["variables"] != nil && vars == nil {
		return graphError("GraphQL variables must be an object")
	}
	op, _ := row["operationName"].(string)
	if row["operationName"] != nil {
		if _, ok := row["operationName"].(string); !ok {
			return graphError("GraphQL operationName must be a string")
		}
	}
	return graphql.Do(graphql.Params{Schema: g.schema, RequestString: query, RootObject: g.root, VariableValues: vars, OperationName: op})
}
func (s *store) serveGraphQL(method string, body []byte) httpReply {
	if method != "POST" {
		reject(405, "Method Not Allowed")
	}
	var raw any
	if json.Unmarshal(body, &raw) != nil {
		reject(400, "Malformed JSON")
	}
	if batch, ok := raw.([]any); ok {
		if len(batch) == 0 || len(batch) > 50 {
			reject(400, "Expected 1 to 50 GraphQL operations")
		}
		results := make([]any, 0, len(batch))
		for _, v := range batch {
			results = append(results, s.gql.execute(v))
		}
		return jsonReply(200, results)
	}
	return jsonReply(200, s.gql.execute(raw))
}

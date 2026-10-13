package catalog

import (
	"context"
	"reflect"
)

// AuthorizationResource is one required action/resource pair. An empty Action
// uses the operation's generated IAMAction. TagsKnown distinguishes absent tags
// from a resource family whose tags cannot be evaluated.
type AuthorizationResource struct {
	Tags      map[string]string
	Action    string
	ARN       string
	TagsKnown bool
}

// Authorizer is implemented next to each service's resource lookups.
// Empty results request wildcard-only evaluation for that operation.
type Authorizer interface {
	AuthorizationResources(context.Context, string, any) ([]AuthorizationResource, error)
}

// AuthorizerOf discovers the optional interface without changing generated APIs.
func AuthorizerOf(api any) Authorizer {
	authorizer, _ := api.(Authorizer)
	return authorizer
}

// InputString reads a named, top-level decoded string member without serializing
// streaming bodies or requiring a type switch across generated request types.
func InputString(input any, name string) string {
	values := InputStrings(input, name)
	if len(values) == 0 {
		return ""
	}

	return values[0]
}

// InputStrings reads a decoded string or string-list member.
func InputStrings(input any, name string) []string {
	v := indirect(reflect.ValueOf(input))
	if v.Kind() != reflect.Struct {
		return nil
	}

	v = indirect(v.FieldByName(name))
	switch v.Kind() {
	case reflect.String:
		return []string{v.String()}
	case reflect.Slice:
		var values []string
		for i := range v.Len() {
			if item := indirect(v.Index(i)); item.Kind() == reflect.String {
				values = append(values, item.String())
			}
		}

		return values
	}

	return nil
}

// InputMapKeys reads keys from a named decoded map member, such as DynamoDB's
// RequestItems, whose keys are table names.
func InputMapKeys(input any, name string) []string {
	v := indirect(reflect.ValueOf(input))
	if v.Kind() != reflect.Struct {
		return nil
	}

	v = indirect(v.FieldByName(name))
	if v.Kind() != reflect.Map || v.Type().Key().Kind() != reflect.String {
		return nil
	}

	var out []string
	for _, key := range v.MapKeys() {
		out = append(out, key.String())
	}

	return out
}

// NestedInputStrings reads nested string fields in a decoded structure/list.
// Only structs and slices are traversed; streams and byte slices are ignored.
func NestedInputStrings(input any, name string) []string {
	var out []string
	var walk func(reflect.Value)
	walk = func(v reflect.Value) {
		v = indirect(v)
		switch v.Kind() {
		case reflect.Struct:
			for i := range v.NumField() {
				field := v.Field(i)
				if v.Type().Field(i).Name == name {
					if value := indirect(field); value.Kind() == reflect.String {
						out = append(out, value.String())
					}
				} else {
					walk(field)
				}
			}
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.Uint8 {
				return
			}

			for i := range v.Len() {
				walk(v.Index(i))
			}
		}
	}

	walk(reflect.ValueOf(input))
	return out
}

func indirect(v reflect.Value) reflect.Value {
	for v.IsValid() && (v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface) {
		if v.IsNil() {
			return reflect.Value{}
		}

		v = v.Elem()
	}

	return v
}

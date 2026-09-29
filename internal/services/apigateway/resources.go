package apigateway

import (
	"context"
	"maps"
	"regexp"
	"slices"
	"strings"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/apigateway"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

var pathPart = regexp.MustCompile(`^(\{[a-zA-Z0-9._-]+\+?\}|[a-zA-Z0-9._\-:~@%$]+)$`)

func isVariable(part string) bool { return strings.HasPrefix(part, "{") }

func loadResource(a *restAPI, id *string) (*api.Resource, error) {
	r, ok := a.Resources[value(id)]
	if !ok {
		return nil, notFound("Invalid Resource identifier specified")
	}

	return r, nil
}

// childPath is the path of a child of parent.
func childPath(parent, part string) string {
	if parent == "/" {
		return "/" + part
	}

	return parent + "/" + part
}

// checkChild rejects a path part that would clash with a sibling's, or that
// would go under a greedy path variable.
func checkChild(a *restAPI, parent *api.Resource, id, part string) error {
	if !pathPart.MatchString(part) {
		return badRequest("Resource's path part only allow a-zA-Z0-9._- and curly braces at the beginning and " +
			"the end and an optional plus sign before the closing brace.")
	}

	if strings.HasSuffix(value(parent.PathPart), "+}") {
		return badRequest("Cannot create a child of a resource with a greedy path variable: %s", *parent.Path)
	}

	for _, r := range a.Resources {
		if value(r.ParentId) != *parent.Id || *r.Id == id {
			continue
		}

		switch {
		case value(r.PathPart) == part:
			return conflict("Another resource with the same parent already has this name: %s", part)
		case isVariable(part) && isVariable(value(r.PathPart)):
			return badRequest("A sibling (%s) of this resource already has a variable path part -- only one is "+
				"allowed", value(r.PathPart))
		}
	}

	return nil
}

// repath recomputes the paths of r and everything under it.
func repath(a *restAPI, r *api.Resource) {
	r.Path = new(childPath(value(a.Resources[value(r.ParentId)].Path), value(r.PathPart)))
	for _, child := range a.Resources {
		if value(child.ParentId) == *r.Id {
			repath(a, child)
		}
	}
}

// resourceView is a resource as the API reports it: with its methods in full
// when embed asks for them, and otherwise only their names.
func resourceView(r *api.Resource, embed []string) api.Resource {
	out := *clone(r)
	if len(out.ResourceMethods) == 0 {
		out.ResourceMethods = nil
		return out
	}

	if !slices.Contains(embed, "methods") {
		for k := range out.ResourceMethods {
			out.ResourceMethods[k] = api.Method{}
		}
	}

	return out
}

func (s *Service) CreateResource(ctx context.Context, in *api.CreateResourceRequest) (*api.Resource, error) {
	var out *api.Resource
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		parent, loadErr := loadResource(a, in.ParentId)
		if loadErr != nil {
			return loadErr
		}

		id := newID(6)
		if checkErr := checkChild(a, parent, id, value(in.PathPart)); checkErr != nil {
			return checkErr
		}

		out = &api.Resource{
			Id: new(id), ParentId: parent.Id, PathPart: in.PathPart, Path: new(childPath(*parent.Path, *in.PathPart)),
		}
		a.Resources[id] = out
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) GetResource(ctx context.Context, in *api.GetResourceRequest) (*api.Resource, error) {
	var out api.Resource
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		r, loadErr := loadResource(a, in.ResourceId)
		if loadErr != nil {
			return loadErr
		}

		out = resourceView(r, in.Embed)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) GetResources(ctx context.Context, in *api.GetResourcesRequest) (*api.Resources, error) {
	out := &api.Resources{Items: []api.Resource{}}
	err := s.viewAPI(ctx, in.RestApiId, func(_ storage.ReadTx, _ request.Scope, a *restAPI) error {
		items := make([]api.Resource, 0, len(a.Resources))
		for _, id := range slices.Sorted(maps.Keys(a.Resources)) {
			items = append(items, resourceView(a.Resources[id], in.Embed))
		}

		var pageErr error
		items, out.Position, pageErr = page(items, func(r api.Resource) string { return *r.Id }, in.Position, in.Limit)
		out.Items = append(out.Items, items...)
		return pageErr
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) UpdateResource(ctx context.Context, in *api.UpdateResourceRequest) (*api.Resource, error) {
	var out api.Resource
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		r, loadErr := loadResource(a, in.ResourceId)
		if loadErr != nil {
			return loadErr
		}

		if r.ParentId == nil {
			return badRequest("The root resource cannot be changed.")
		}

		changed := clone(r)
		if patchErr := patch(changed, in.PatchOperations, "pathPart", "parentId"); patchErr != nil {
			return patchErr
		}

		parent, loadErr := loadResource(a, changed.ParentId)
		if loadErr != nil {
			return loadErr
		}

		// A resource cannot move under itself.
		for p := parent; p != nil; p = a.Resources[value(p.ParentId)] {
			if *p.Id == *r.Id {
				return badRequest("A resource cannot be moved under itself.")
			}
		}

		if checkErr := checkChild(a, parent, *r.Id, value(changed.PathPart)); checkErr != nil {
			return checkErr
		}

		r.PathPart, r.ParentId = changed.PathPart, changed.ParentId
		repath(a, r)
		out = resourceView(r, nil)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &out, nil
}

func (s *Service) DeleteResource(
	ctx context.Context,
	in *api.DeleteResourceRequest,
) (*api.DeleteResourceOutput, error) {
	err := s.changeAPI(ctx, in.RestApiId, func(_ storage.WriteTx, _ request.Scope, a *restAPI) error {
		r, loadErr := loadResource(a, in.ResourceId)
		if loadErr != nil {
			return loadErr
		}

		if r.ParentId == nil {
			return badRequest("Cannot delete root resource")
		}

		deleteTree(a, *r.Id)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteResourceOutput{}, nil
}

func deleteTree(a *restAPI, id string) {
	for _, child := range a.Resources {
		if value(child.ParentId) == id {
			deleteTree(a, *child.Id)
		}
	}

	delete(a.Resources, id)
}

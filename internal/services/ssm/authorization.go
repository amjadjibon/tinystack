package ssm

import (
	"context"
	"slices"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

func (s *Service) AuthorizationResources(
	ctx context.Context,
	operation string,
	input any,
) ([]catalog.AuthorizationResource, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if resources, handled, docErr := s.documentResources(ctx, m.Scope, operation, input); handled {
		return resources, docErr
	}

	names := catalog.InputStrings(input, "Name")
	names = append(names, catalog.InputStrings(input, "Names")...)
	if operation == "GetParametersByPath" {
		names = []string{catalog.InputString(input, "Path")}
	}

	if operation == "AddTagsToResource" || operation == "RemoveTagsFromResource" || operation == "ListTagsForResource" {
		if catalog.InputString(input, "ResourceType") == "Parameter" {
			names = []string{catalog.InputString(input, "ResourceId")}
		}
	}

	var out []catalog.AuthorizationResource
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, name := range names {
			if name == "" {
				continue
			}

			if !strings.HasPrefix(name, "arn:") {
				name, _, _ = strings.Cut(name, ":")
			}

			arn := name
			if !strings.HasPrefix(arn, "arn:") {
				arn = parameterARN(m.Scope, name)
			} else {
				_, name, _ = strings.Cut(arn, ":parameter/")
				name = "/" + name
			}

			target := catalog.AuthorizationResource{ARN: arn, TagsKnown: true}
			stored, _, readErr := storage.Read[parameter](tx, parameterKey(m.Scope, name))
			if readErr != nil {
				return readErr
			}

			if stored != nil {
				target.Tags = common.CloneTags(stored.Tags)
			}

			out = append(out, target)
		}

		return nil
	})
	return out, err
}

// documentOperations take a document's name in Name.
var documentOperations = []string{
	"CreateDocument", "GetDocument", "DescribeDocument", "UpdateDocument", "DeleteDocument",
	"ListDocumentVersions", "UpdateDocumentDefaultVersion", "DescribeDocumentPermission", "ModifyDocumentPermission",
}

// documentResources names the resources of the document and Run Command
// operations, and of tagging a document. It reports false for the others.
func (s *Service) documentResources(ctx context.Context, scope request.Scope, operation string, input any) (
	[]catalog.AuthorizationResource,
	bool,
	error,
) {
	var names []string
	switch {
	case slices.Contains(documentOperations, operation):
		names = []string{catalog.InputString(input, "Name")}
	case operation == "SendCommand":
		names = []string{catalog.InputString(input, "DocumentName")}
	case (operation == "AddTagsToResource" || operation == "RemoveTagsFromResource" ||
		operation == "ListTagsForResource") && catalog.InputString(input, "ResourceType") == documentKind:
		names = []string{catalog.InputString(input, "ResourceId")}
	case slices.Contains([]string{
		"ListDocuments", "ListCommands", "ListCommandInvocations", "GetCommandInvocation", "CancelCommand",
	}, operation):
		return nil, true, nil
	default:
		return nil, false, nil
	}

	var out []catalog.AuthorizationResource
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, ref := range names {
			name, refErr := documentRef(&ref)
			if refErr != nil {
				continue
			}

			rec := &docRecord{Name: name, Owner: scope.AccountID}
			if builtinRec, _, ok := builtin(name); ok {
				rec = builtinRec
			}

			target := catalog.AuthorizationResource{ARN: documentARN(scope, rec), TagsKnown: true}
			stored, _, readErr := storage.Read[docRecord](tx, docKey(scope, name))
			if readErr != nil {
				return readErr
			}

			if stored != nil {
				target.Tags = common.CloneTags(stored.Tags)
			}

			out = append(out, target)
		}

		return nil
	})
	if err != nil {
		return nil, true, err
	}

	if operation == "SendCommand" {
		for _, id := range catalog.InputStrings(input, "InstanceIds") {
			out = append(out, catalog.AuthorizationResource{
				ARN: "arn:aws:ec2:" + scope.Region + ":" + scope.AccountID + ":instance/" + id,
			})
		}
	}

	return out, true, nil
}

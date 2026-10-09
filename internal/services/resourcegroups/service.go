// Package resourcegroups implements AWS Resource Groups without HTTP
// dependencies. A group is a name, a description, tags, and either a resource
// query or a configuration. Groups belong to an account and Region:
//
//	<scope>/resourcegroups/group/<name>
//	<scope>/resourcegroups/account/settings
//
// A group's members are never stored: ListGroupResources and SearchResources
// run the query against the resources every service lists in its inventory
// (the same listing the Resource Groups Tagging API reads, in listing), so a
// TAG_FILTERS_1_0 query finds whatever is tagged now, and a
// CLOUDFORMATION_STACK_1_0 query finds the resources of a stack, matched to
// the inventory by physical ID. Resource types are CloudFormation-style names
// (types.go).
package resourcegroups

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/resourcegroups"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	service     = "resourcegroups"
	kindGroup   = "group"
	kindAccount = "account"
	nameSetting = "settings"

	maxName        = 300
	maxDescription = 1024
	maxText        = 300
	maxPage        = 50
	maxFilters     = 5

	statusActive   = "ACTIVE"
	statusInactive = "INACTIVE"
)

var (
	_ api.API = (*Service)(nil)

	namePattern        = regexp.MustCompile(`^[a-zA-Z0-9_.-]+$`)
	descriptionPattern = regexp.MustCompile(`^[\sa-zA-Z0-9_.-]*$`)
	textPattern        = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)
	arnPattern         = regexp.MustCompile(
		`^arn:aws(?:-[a-z]+)*:resource-groups:([a-z]{2}(?:-[a-z]+)+-\d):(\d{12}):group/([a-zA-Z0-9_.-]{1,300})$`)
)

// Service is the Resource Groups service.
type Service struct {
	store storage.Store
	// caller lists every service's resources and reads CloudFormation stacks.
	caller *catalog.Caller
}

// New returns a Resource Groups service over store.
func New(store storage.Store) *Service { return &Service{store: store} }

// UseCaller hands over the caller that reaches every other service. app.New
// calls it before the gateway serves requests.
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

// group is a group's record.
type group struct {
	Criticality   *int64
	Query         *api.ResourceQuery
	Tags          map[string]string
	Name          string
	Description   string
	Owner         string
	DisplayName   string
	Configuration []api.GroupConfigurationItem
}

// settings is an account's group lifecycle events setting. It settles at once.
type settings struct {
	Status string
}

func bad(format string, args ...any) error {
	return awserror.New("BadRequestException", fmt.Sprintf(format, args...))
}

func notFound(name string) error {
	return awserror.New("NotFoundException", "Group "+name+" does not exist.")
}

func invalidToken() error { return bad("The pagination token is invalid.") }

func groupARN(scope request.Scope, name string) string {
	return "arn:aws:resource-groups:" + scope.Region + ":" + scope.AccountID + ":group/" + name
}

func (g *group) output(scope request.Scope) *api.Group {
	return &api.Group{
		GroupArn: new(groupARN(scope, g.Name)), Name: new(g.Name), Description: textOrNil(g.Description),
		Criticality: g.Criticality, Owner: textOrNil(g.Owner), DisplayName: textOrNil(g.DisplayName),
	}
}

// textOrNil leaves an unset text member out of a response.
func textOrNil(v string) *string {
	if v == "" {
		return nil
	}

	return new(v)
}

// groupName resolves the Group (a name or an ARN) or the deprecated GroupName
// of a request to the name of a group in the caller's scope.
func groupName(scope request.Scope, current, deprecated *string) (string, error) {
	ref := current
	if ref == nil {
		ref = deprecated
	}

	if ref == nil {
		return "", bad("Either Group or GroupName is required.")
	}

	if strings.HasPrefix(*ref, "arn:") {
		parts := arnPattern.FindStringSubmatch(*ref)
		if parts == nil {
			return "", bad("%q is not a valid group ARN.", *ref)
		}

		if parts[1] != scope.Region || parts[2] != scope.AccountID {
			return "", notFound(parts[3])
		}

		return parts[3], nil
	}

	if n := utf8.RuneCountInString(*ref); n > maxName || !namePattern.MatchString(*ref) {
		return "", bad("%q is not a valid group name or ARN.", *ref)
	}

	return *ref, nil
}

// load reads a group in a transaction.
func load(tx storage.ReadTx, scope request.Scope, name string) (*group, error) {
	g, found, err := storage.Read[group](tx, scope.Key(service, kindGroup, name))
	if err == nil && !found {
		err = notFound(name)
	}

	return g, err
}

package codecommit

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/codecommit"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	serviceName    = "codecommit"
	repositoryKind = "repository"
)

var _ api.API = (*Service)(nil)

type Service struct {
	store  storage.Store
	caller *catalog.Caller
	// root holds the repositories' git objects, outside the store; temp is whether Stop removes it.
	root string
	mu   sync.Mutex
	temp bool
}

// UseCaller lets the service publish repository state changes to EventBridge.
func (s *Service) UseCaller(c *catalog.Caller) { s.caller = c }

func New(store storage.Store) *Service {
	return &Service{store: store}
}

type repositoryRecord struct {
	Metadata *api.RepositoryMetadata `json:"metadata"`
	Tags     map[string]string       `json:"tags,omitempty"`
}

type cursor struct {
	Name string  `json:"n"`
	Time float64 `json:"t,omitempty"`
}

func repoArn(scope request.Scope, name string) string {
	return fmt.Sprintf("arn:aws:codecommit:%s:%s:%s", scope.Region, scope.AccountID, name)
}

func cloneUrlHttp(scope request.Scope, name string) string {
	return fmt.Sprintf("https://git-codecommit.%s.amazonaws.com/v1/repos/%s", scope.Region, name)
}

func cloneUrlSsh(scope request.Scope, name string) string {
	return fmt.Sprintf("ssh://git-codecommit.%s.amazonaws.com/v1/repos/%s", scope.Region, name)
}

func newRepositoryID() string {
	const hexAlphabet = "0123456789abcdef"
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		common.RandomID(8, hexAlphabet),
		common.RandomID(4, hexAlphabet),
		common.RandomID(4, hexAlphabet),
		common.RandomID(4, hexAlphabet),
		common.RandomID(12, hexAlphabet),
	)
}

func (s *Service) CreateRepository(
	ctx context.Context,
	in *api.CreateRepositoryInput,
) (*api.CreateRepositoryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	if descErr := validateRepoDescription(in.RepositoryDescription); descErr != nil {
		return nil, descErr
	}

	if kmsErr := validateKmsKeyID(in.KmsKeyId); kmsErr != nil {
		return nil, kmsErr
	}

	if tagsErr := tagError(tagging.CheckSet(in.Tags)); tagsErr != nil {
		return nil, tagsErr
	}

	tags := maps.Clone(in.Tags)

	now := common.Unix(time.Now())
	arn := repoArn(scope, name)
	cloneHttp := cloneUrlHttp(scope, name)
	cloneSsh := cloneUrlSsh(scope, name)
	repoID := newRepositoryID()

	meta := &api.RepositoryMetadata{
		AccountId:             &scope.AccountID,
		RepositoryId:          &repoID,
		RepositoryName:        &name,
		RepositoryDescription: in.RepositoryDescription,
		Arn:                   &arn,
		CloneUrlHttp:          &cloneHttp,
		CloneUrlSsh:           &cloneSsh,
		CreationDate:          &now,
		LastModifiedDate:      &now,
		KmsKeyId:              in.KmsKeyId,
	}

	record := &repositoryRecord{
		Metadata: meta,
		Tags:     tags,
	}

	key := scope.Key(serviceName, repositoryKind, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		_, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if found {
			return awserror.New("RepositoryNameExistsException",
				fmt.Sprintf("The repository name %s already exists in your Amazon Web Services account.", name))
		}

		return storage.Write(tx, key, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRepositoryOutput{
		RepositoryMetadata: meta,
	}, nil
}

func (s *Service) GetRepository(ctx context.Context, in *api.GetRepositoryInput) (*api.GetRepositoryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	var record *repositoryRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, scope.Key(serviceName, repositoryKind, name))
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name))
		}

		record = rec
		return nil
	})
	if err != nil {
		return nil, err
	}

	meta := common.Clone(record.Metadata)
	dir, err := s.repositoryDir(scope, *meta.RepositoryId)
	if err != nil {
		return nil, err
	}

	// The default branch is the repository's HEAD; there is none until a branch is pushed.
	r := &repository{scope: scope, meta: meta, name: name, id: *meta.RepositoryId, dir: dir}
	if r.exists() {
		current, _, branchErr := r.defaultBranch(ctx)
		if branchErr != nil {
			return nil, branchErr
		}

		if current != "" {
			meta.DefaultBranch = &current
		}
	}

	return &api.GetRepositoryOutput{
		RepositoryMetadata: meta,
	}, nil
}

func (s *Service) ListRepositories(
	ctx context.Context,
	in *api.ListRepositoriesInput,
) (*api.ListRepositoriesOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	by, err := validateSortBy(in.SortBy)
	if err != nil {
		return nil, err
	}

	order, err := validateOrder(in.Order)
	if err != nil {
		return nil, err
	}

	direction := 1
	if order == orderDescending {
		direction = -1
	}

	sum := sha256.Sum256([]byte(scope.AccountID + "/" + scope.Region + "/ListRepositories/" + by + "/" + order))
	prefix := hex.EncodeToString(sum[:]) + ":"
	var after *cursor
	if in.NextToken != nil && *in.NextToken != "" {
		raw, decErr := base64.RawURLEncoding.DecodeString(*in.NextToken)
		c := cursor{}
		if decErr != nil || !strings.HasPrefix(string(raw), prefix) ||
			json.Unmarshal([]byte(strings.TrimPrefix(string(raw), prefix)), &c) != nil ||
			!namePattern.MatchString(c.Name) || c.Time < 0 {
			return nil, awserror.New(
				"InvalidContinuationTokenException",
				"The specified continuation token is not valid.",
			)
		}

		after = &c
	}

	var rows []*repositoryRecord
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var readErr error
		rows, readErr = storage.Records[repositoryRecord](tx, scope.Prefix(serviceName, repositoryKind))
		return readErr
	})
	if err != nil {
		return nil, err
	}

	compare := func(a, b *repositoryRecord) int {
		if by == sortByLastModifiedDate {
			ta := common.Value(a.Metadata.LastModifiedDate)
			tb := common.Value(b.Metadata.LastModifiedDate)
			if c := cmp.Compare(ta, tb); c != 0 {
				return c
			}
		}

		return strings.Compare(*a.Metadata.RepositoryName, *b.Metadata.RepositoryName)
	}

	slices.SortFunc(rows, func(a, b *repositoryRecord) int {
		return direction * compare(a, b)
	})

	if after != nil {
		afterRec := &repositoryRecord{
			Metadata: &api.RepositoryMetadata{
				RepositoryName:   &after.Name,
				LastModifiedDate: &after.Time,
			},
		}

		rows = slices.DeleteFunc(rows, func(r *repositoryRecord) bool {
			return direction*compare(r, afterRec) <= 0
		})
	}

	// The model documents batches of 1,000 repositories.
	const pageSize = 1000
	selected := rows
	var nextToken *string
	if len(selected) > pageSize {
		selected = selected[:pageSize]
		last := selected[pageSize-1]
		cur := cursor{
			Name: *last.Metadata.RepositoryName,
			Time: common.Value(last.Metadata.LastModifiedDate),
		}

		raw, marshalErr := json.Marshal(cur)
		if marshalErr != nil {
			return nil, marshalErr
		}

		tokenStr := base64.RawURLEncoding.EncodeToString([]byte(prefix + string(raw)))
		nextToken = &tokenStr
	}

	out := &api.ListRepositoriesOutput{
		Repositories: make([]api.RepositoryNameIdPair, 0, len(selected)),
		NextToken:    nextToken,
	}

	for _, r := range selected {
		out.Repositories = append(out.Repositories, api.RepositoryNameIdPair{
			RepositoryId:   r.Metadata.RepositoryId,
			RepositoryName: r.Metadata.RepositoryName,
		})
	}

	return out, nil
}

func (s *Service) UpdateRepositoryName(
	ctx context.Context,
	in *api.UpdateRepositoryNameInput,
) (*api.UpdateRepositoryNameOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	oldName, err := validateRepoName(in.OldName)
	if err != nil {
		return nil, err
	}

	newName, err := validateRepoName(in.NewName)
	if err != nil {
		return nil, err
	}

	if oldName == newName {
		return &api.UpdateRepositoryNameOutput{}, nil
	}

	oldKey := scope.Key(serviceName, repositoryKind, oldName)
	newKey := scope.Key(serviceName, repositoryKind, newName)

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		oldRec, found, readErr := storage.Read[repositoryRecord](tx, oldKey)
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", oldName))
		}

		_, exists, readErr := storage.Read[repositoryRecord](tx, newKey)
		if readErr != nil {
			return readErr
		}

		if exists {
			return awserror.New("RepositoryNameExistsException",
				fmt.Sprintf("The repository name %s already exists in your Amazon Web Services account.", newName))
		}

		now := common.Unix(time.Now())
		newArn := repoArn(scope, newName)
		newHttp := cloneUrlHttp(scope, newName)
		newSsh := cloneUrlSsh(scope, newName)

		newMeta := common.Clone(oldRec.Metadata)
		newMeta.RepositoryName = &newName
		newMeta.Arn = &newArn
		newMeta.CloneUrlHttp = &newHttp
		newMeta.CloneUrlSsh = &newSsh
		newMeta.LastModifiedDate = &now

		newRec := &repositoryRecord{
			Metadata: newMeta,
			Tags:     maps.Clone(oldRec.Tags),
		}

		if writeErr := storage.Write(tx, newKey, newRec); writeErr != nil {
			return writeErr
		}

		tx.Delete(oldKey)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateRepositoryNameOutput{}, nil
}

func (s *Service) UpdateRepositoryDescription(
	ctx context.Context,
	in *api.UpdateRepositoryDescriptionInput,
) (*api.UpdateRepositoryDescriptionOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	if descErr := validateRepoDescription(in.RepositoryDescription); descErr != nil {
		return nil, descErr
	}

	key := scope.Key(serviceName, repositoryKind, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name))
		}

		now := common.Unix(time.Now())
		rec.Metadata.RepositoryDescription = in.RepositoryDescription
		rec.Metadata.LastModifiedDate = &now

		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateRepositoryDescriptionOutput{}, nil
}

func (s *Service) DeleteRepository(
	ctx context.Context,
	in *api.DeleteRepositoryInput,
) (*api.DeleteRepositoryOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := validateRepoName(in.RepositoryName)
	if err != nil {
		return nil, err
	}

	var deletedID *string
	key := scope.Key(serviceName, repositoryKind, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return nil
		}

		deletedID = rec.Metadata.RepositoryId
		tx.Delete(key)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// The record is gone, so nothing can reach the content; removing it needs no transaction.
	if deletedID != nil {
		dir, dirErr := s.repositoryDir(scope, *deletedID)
		if dirErr != nil {
			return nil, dirErr
		}

		if removeErr := os.RemoveAll(dir); removeErr != nil {
			return nil, removeErr
		}
	}

	return &api.DeleteRepositoryOutput{
		RepositoryId: deletedID,
	}, nil
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceInput) (*api.TagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseRepositoryArn(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	if len(in.Tags) == 0 {
		return nil, awserror.New("TagsMapRequiredException", "A tags map is required, but was not specified.")
	}

	if tagsErr := tagError(tagging.CheckSet(in.Tags)); tagsErr != nil {
		return nil, tagsErr
	}

	key := scope.Key(serviceName, repositoryKind, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name))
		}

		merged, mergeErr := tagging.Merge(rec.Tags, in.Tags)
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		rec.Tags = merged
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceOutput{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceInput) (*api.UntagResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseRepositoryArn(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	if len(in.TagKeys) == 0 {
		return nil, awserror.New(
			"TagKeysListRequiredException",
			"A list of tag keys is required, but was not specified.",
		)
	}

	if keysErr := validateTagKeys(in.TagKeys); keysErr != nil {
		return nil, keysErr
	}

	key := scope.Key(serviceName, repositoryKind, name)
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, key)
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name))
		}

		rec.Tags = tagging.Remove(rec.Tags, in.TagKeys)
		return storage.Write(tx, key, rec)
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceOutput{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceInput,
) (*api.ListTagsForResourceOutput, error) {
	scope, err := request.RegionalScope(ctx)
	if err != nil {
		return nil, err
	}

	name, err := parseRepositoryArn(scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var tags map[string]string
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		rec, found, readErr := storage.Read[repositoryRecord](tx, scope.Key(serviceName, repositoryKind, name))
		if readErr != nil {
			return readErr
		}

		if !found {
			return awserror.New("RepositoryDoesNotExistException",
				fmt.Sprintf("The repository %s does not exist.", name))
		}

		tags = maps.Clone(rec.Tags)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.ListTagsForResourceOutput{
		Tags: tags,
	}, nil
}

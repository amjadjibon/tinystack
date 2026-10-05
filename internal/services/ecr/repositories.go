package ecr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/ecr"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	mutable      = "MUTABLE"
	immutable    = "IMMUTABLE"
	aes256       = "AES256"
	maxPolicy    = 10240
	maxLifecycle = 30720
)

// repository is a repository's record. Policies are stored, never evaluated;
// the lifecycle policy expires images whenever it or the repository's images
// change.
type repository struct {
	Created    time.Time
	Evaluated  time.Time `json:",omitzero"`
	Tags       map[string]string
	Name       string
	Mutability string
	Policy     string `json:",omitempty"`
	Lifecycle  string `json:",omitempty"`
	ScanOnPush bool
}

func (r *repository) output(m *request.Metadata) *api.Repository {
	return &api.Repository{
		RepositoryArn: new(repoARN(m.Scope, r.Name)), RegistryId: new(m.AccountID), RepositoryName: new(r.Name),
		RepositoryUri: new(registryHost(m) + "/" + r.Name), CreatedAt: common.SecondsMilli(r.Created),
		ImageTagMutability:         new(r.Mutability),
		ImageScanningConfiguration: &api.ImageScanningConfiguration{ScanOnPush: new(r.ScanOnPush)},
		EncryptionConfiguration:    &api.EncryptionConfiguration{EncryptionType: new(aes256)},
	}
}

func checkMutability(v *string) error {
	if v != nil && *v != mutable && *v != immutable {
		return invalidParameter("TinyStack supports imageTagMutability MUTABLE and IMMUTABLE, not %s.", *v)
	}

	return nil
}

func tagMap(tags []api.Tag) map[string]string {
	return tagging.Map(tags, func(t api.Tag) (*string, *string) { return t.Key, t.Value })
}

func tagList(tags map[string]string) []api.Tag {
	return tagging.List(tags, func(k, v string) api.Tag { return api.Tag{Key: &k, Value: &v} })
}

// tagError reports a tagging violation with ECR's codes.
func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return awserror.New("InvalidTagParameterException", err.Error())
}

func (s *Service) CreateRepository(
	ctx context.Context,
	in *api.CreateRepositoryRequest,
) (*api.CreateRepositoryResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if registryErr := checkRegistry(m.Scope, in.RegistryId); registryErr != nil {
		return nil, registryErr
	}

	if nameErr := checkRepoName(in.RepositoryName); nameErr != nil {
		return nil, nameErr
	}

	if mutabilityErr := checkMutability(in.ImageTagMutability); mutabilityErr != nil {
		return nil, mutabilityErr
	}

	if e := in.EncryptionConfiguration; e != nil && (common.Value(e.EncryptionType) != aes256 || e.KmsKey != nil) {
		return nil, invalidParameter("TinyStack supports only AES256 encryption.")
	}

	tags := tagMap(in.Tags)
	if tagErr := tagging.Check(tags); tagErr != nil {
		return nil, tagError(tagErr)
	}

	r := &repository{
		Created: s.now().UTC(), Name: *in.RepositoryName, Mutability: mutable, Tags: tags,
		ScanOnPush: common.Value(common.Value(in.ImageScanningConfiguration).ScanOnPush),
	}
	if in.ImageTagMutability != nil {
		r.Mutability = *in.ImageTagMutability
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if _, exists := tx.Get(repoKey(m.Scope, r.Name)); exists {
			return awserror.New("RepositoryAlreadyExistsException", "The repository with name '"+r.Name+
				"' already exists in the registry with id '"+m.AccountID+"'")
		}

		return storage.Write(tx, repoKey(m.Scope, r.Name), r)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateRepositoryResponse{Repository: r.output(&m)}, nil
}

func (s *Service) DescribeRepositories(
	ctx context.Context,
	in *api.DescribeRepositoriesRequest,
) (*api.DescribeRepositoriesResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	if registryErr := checkRegistry(m.Scope, in.RegistryId); registryErr != nil {
		return nil, registryErr
	}

	if len(in.RepositoryNames) > 0 && (in.MaxResults != nil || in.NextToken != nil) {
		return nil, invalidParameter("maxResults and nextToken cannot be used with repositoryNames.")
	}

	n, err := limit(in.MaxResults)
	if err != nil {
		return nil, err
	}

	out := &api.DescribeRepositoriesResponse{Repositories: []api.Repository{}}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		names := in.RepositoryNames
		if len(names) == 0 {
			names = suffixes(tx, prefix(m.Scope, "repo"))
			names, out.NextToken, err = common.Page(
				names,
				func(n string) string { return n },
				in.NextToken,
				n,
				errPageToken,
			)
			if err != nil {
				return err
			}
		}

		for _, name := range names {
			r, loadErr := loadRepo(tx, m.Scope, nil, &name)
			if loadErr != nil {
				return loadErr
			}

			out.Repositories = append(out.Repositories, *r.output(&m))
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRepository(
	ctx context.Context,
	in *api.DeleteRepositoryRequest,
) (*api.DeleteRepositoryResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	var out *api.Repository
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		r, loadErr := loadRepo(tx, m.Scope, in.RegistryId, in.RepositoryName)
		if loadErr != nil {
			return loadErr
		}

		if len(tx.Keys(owned(m.Scope, "image", r.Name))) > 0 && !common.Value(in.Force) {
			return awserror.New("RepositoryNotEmptyException", "The repository with name '"+r.Name+
				"' in registry with id '"+m.AccountID+"' cannot be deleted because it still contains images")
		}

		for _, kind := range repoKinds {
			for _, k := range tx.Keys(owned(m.Scope, kind, r.Name)) {
				tx.Delete(k)
			}
		}

		tx.Delete(repoKey(m.Scope, r.Name))
		out = r.output(&m)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteRepositoryResponse{Repository: out}, nil
}

func (s *Service) PutImageTagMutability(
	ctx context.Context,
	in *api.PutImageTagMutabilityRequest,
) (*api.PutImageTagMutabilityResponse, error) {
	if in.ImageTagMutability == nil {
		return nil, invalidParameter("imageTagMutability is required.")
	}

	if err := checkMutability(in.ImageTagMutability); err != nil {
		return nil, err
	}

	var out *api.PutImageTagMutabilityResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.WriteTx, m *request.Metadata, r *repository) error {
			r.Mutability = *in.ImageTagMutability
			out = &api.PutImageTagMutabilityResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), ImageTagMutability: new(r.Mutability),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) PutImageScanningConfiguration(
	ctx context.Context,
	in *api.PutImageScanningConfigurationRequest,
) (*api.PutImageScanningConfigurationResponse, error) {
	if in.ImageScanningConfiguration == nil {
		return nil, invalidParameter("imageScanningConfiguration is required.")
	}

	var out *api.PutImageScanningConfigurationResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.WriteTx, m *request.Metadata, r *repository) error {
			r.ScanOnPush = common.Value(in.ImageScanningConfiguration.ScanOnPush)
			out = &api.PutImageScanningConfigurationResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name),
				ImageScanningConfiguration: &api.ImageScanningConfiguration{ScanOnPush: new(r.ScanOnPush)},
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// checkPolicy accepts a JSON policy document with a Statement.
func checkPolicy(text *string) error {
	var doc struct {
		Statement json.RawMessage
	}
	if text == nil || len(*text) > maxPolicy || json.Unmarshal([]byte(*text), &doc) != nil || len(doc.Statement) == 0 {
		return invalidParameter("Invalid parameter at 'PolicyText' failed to satisfy constraint: " +
			"'Invalid repository policy provided'")
	}

	return nil
}

func (s *Service) SetRepositoryPolicy(
	ctx context.Context,
	in *api.SetRepositoryPolicyRequest,
) (*api.SetRepositoryPolicyResponse, error) {
	// Policies are not evaluated, so none can lock the caller out and force
	// changes nothing.
	if err := checkPolicy(in.PolicyText); err != nil {
		return nil, err
	}

	var out *api.SetRepositoryPolicyResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.WriteTx, m *request.Metadata, r *repository) error {
			r.Policy = *in.PolicyText
			out = &api.SetRepositoryPolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), PolicyText: new(r.Policy),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func policyNotFound(m *request.Metadata, r *repository) error {
	return awserror.New("RepositoryPolicyNotFoundException", "Repository policy does not exist for the repository "+
		"with name '"+r.Name+"' in the registry with id '"+m.AccountID+"'")
}

func (s *Service) GetRepositoryPolicy(
	ctx context.Context,
	in *api.GetRepositoryPolicyRequest,
) (*api.GetRepositoryPolicyResponse, error) {
	var out *api.GetRepositoryPolicyResponse
	err := s.view(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.ReadTx, m *request.Metadata, r *repository) error {
			if r.Policy == "" {
				return policyNotFound(m, r)
			}

			out = &api.GetRepositoryPolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), PolicyText: new(r.Policy),
			}
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

func (s *Service) DeleteRepositoryPolicy(
	ctx context.Context,
	in *api.DeleteRepositoryPolicyRequest,
) (*api.DeleteRepositoryPolicyResponse, error) {
	var out *api.DeleteRepositoryPolicyResponse
	err := s.update(ctx, in.RegistryId, in.RepositoryName,
		func(_ storage.WriteTx, m *request.Metadata, r *repository) error {
			if r.Policy == "" {
				return policyNotFound(m, r)
			}

			out = &api.DeleteRepositoryPolicyResponse{
				RegistryId: new(m.AccountID), RepositoryName: new(r.Name), PolicyText: new(r.Policy),
			}
			r.Policy = ""
			return nil
		})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// taggedRepo returns the name of the scope's repository arn names; only
// repositories take tags.
func taggedRepo(scope request.Scope, arn *string) (*string, error) {
	name, ok := strings.CutPrefix(common.Value(arn), repoARN(scope, ""))
	if !ok || name == "" {
		return nil, invalidParameter("Invalid parameter at 'resourceArn' failed to satisfy constraint: "+
			"'Invalid ARN' (%q)", common.Value(arn))
	}

	return &name, nil
}

// tagged runs fn on the repository arn names and stores it after.
func (s *Service) tagged(ctx context.Context, arn *string, fn func(*repository) error) error {
	m, err := request.Regional(ctx)
	if err != nil {
		return err
	}

	name, err := taggedRepo(m.Scope, arn)
	if err != nil {
		return err
	}

	return s.update(ctx, nil, name, func(_ storage.WriteTx, _ *request.Metadata, r *repository) error {
		return fn(r)
	})
}

func (s *Service) TagResource(ctx context.Context, in *api.TagResourceRequest) (*api.TagResourceResponse, error) {
	err := s.tagged(ctx, in.ResourceArn, func(r *repository) error {
		tags, mergeErr := tagging.Merge(r.Tags, tagMap(in.Tags))
		if mergeErr != nil {
			return tagError(mergeErr)
		}

		r.Tags = tags
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.TagResourceResponse{}, nil
}

func (s *Service) UntagResource(ctx context.Context, in *api.UntagResourceRequest) (*api.UntagResourceResponse, error) {
	err := s.tagged(ctx, in.ResourceArn, func(r *repository) error {
		r.Tags = tagging.Remove(r.Tags, in.TagKeys)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.UntagResourceResponse{}, nil
}

func (s *Service) ListTagsForResource(
	ctx context.Context,
	in *api.ListTagsForResourceRequest,
) (*api.ListTagsForResourceResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	name, err := taggedRepo(m.Scope, in.ResourceArn)
	if err != nil {
		return nil, err
	}

	var out *api.ListTagsForResourceResponse
	err = s.view(ctx, nil, name, func(_ storage.ReadTx, _ *request.Metadata, r *repository) error {
		out = &api.ListTagsForResourceResponse{Tags: tagList(r.Tags)}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return out, nil
}

// DescribeRegistry reports the caller's registry, which never replicates.
func (s *Service) DescribeRegistry(
	ctx context.Context,
	_ *api.DescribeRegistryRequest,
) (*api.DescribeRegistryResponse, error) {
	m, err := request.Regional(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DescribeRegistryResponse{
		RegistryId:               new(m.AccountID),
		ReplicationConfiguration: &api.ReplicationConfiguration{Rules: []api.ReplicationRule{}},
	}, nil
}

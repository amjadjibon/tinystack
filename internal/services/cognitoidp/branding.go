package cognitoidp

import (
	"context"
	"crypto/sha256"
	"fmt"
	"regexp"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/cognitoidp"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

type uiCustomization struct {
	Output api.UICustomizationType
	Image  []byte
}

func customizationClient(tx storage.ReadTx, scope request.Scope, p *pool, id *string) (string, error) {
	name := common.Value(id)
	if name == "" || name == allClients {
		return allClients, nil
	}

	_, err := loadClient(tx, scope, p, id)
	return name, err
}

func (s *Service) GetUICustomization(
	ctx context.Context, in *api.GetUICustomizationRequest,
) (*api.GetUICustomizationResponse, error) {
	var out *api.UICustomizationType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		id, err := customizationClient(tx, scope, p, in.ClientId)
		if err != nil {
			return err
		}

		record, ok, err := storage.Read[uiCustomization](tx, scope.Key(service, "ui", p.ID+"/"+id))
		if err != nil {
			return err
		}

		if !ok && id != allClients {
			record, ok, err = storage.Read[uiCustomization](tx, scope.Key(service, "ui", p.ID+"/ALL"))
			if err != nil {
				return err
			}
		}

		if ok {
			out = &record.Output
		} else {
			out = &api.UICustomizationType{UserPoolId: new(p.ID), ClientId: new(id)}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.GetUICustomizationResponse{UICustomization: out}, nil
}

func (s *Service) SetUICustomization(
	ctx context.Context, in *api.SetUICustomizationRequest,
) (*api.SetUICustomizationResponse, error) {
	if len(common.Value(in.CSS)) > 131072 || len(in.ImageFile) > 131072 {
		return nil, invalidParameter("CSS and ImageFile must each be at most 131072 bytes.")
	}

	var out *api.UICustomizationType
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		id, err := customizationClient(tx, scope, p, in.ClientId)
		if err != nil {
			return err
		}

		key := scope.Key(service, "ui", p.ID+"/"+id)
		record, ok, err := storage.Read[uiCustomization](tx, key)
		if err != nil {
			return err
		}

		if !ok {
			record = &uiCustomization{Output: api.UICustomizationType{
				UserPoolId: new(p.ID), ClientId: new(id),
				CreationDate: common.SecondsMilli(s.now()),
			}}
		}

		if in.CSS != nil {
			record.Output.CSS = in.CSS
			sum := sha256.Sum256([]byte(*in.CSS))
			record.Output.CSSVersion = new(fmt.Sprintf("%x", sum[:8]))
		}

		if in.ImageFile != nil {
			record.Image = slices.Clone(in.ImageFile)
		}

		record.Output.LastModifiedDate = common.SecondsMilli(s.now())
		out = &record.Output
		return storage.Write(tx, key, record)
	})
	if err != nil {
		return nil, err
	}

	return &api.SetUICustomizationResponse{UICustomization: out}, nil
}

type branding struct {
	Output api.ManagedLoginBrandingType
	Client string
}

var (
	brandingID = regexp.MustCompile(
		`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-4[0-9a-fA-F]{3}-[89abAB][0-9a-fA-F]{3}-[0-9a-fA-F]{12}$`,
	)
	assetResource = regexp.MustCompile(`^[\w\- ]{1,40}$`)
)

func brandingKey(scope request.Scope, pool, id string) string {
	return scope.Key(service, "branding", pool+"/"+id)
}

func checkBranding(out *api.ManagedLoginBrandingType) error {
	if len(out.Assets) > 40 {
		return invalidParameter("At most 40 branding assets are allowed.")
	}
	if common.Value(out.UseCognitoProvidedValues) && (out.Settings != nil || len(out.Assets) > 0) {
		return invalidParameter("UseCognitoProvidedValues cannot be combined with Settings or Assets.")
	}

	for _, a := range out.Assets {
		if a.Category == nil || !oneOf(
			a.Category,
			"FAVICON_ICO",
			"FAVICON_SVG",
			"EMAIL_GRAPHIC",
			"SMS_GRAPHIC",
			"AUTH_APP_GRAPHIC",
			"PASSWORD_GRAPHIC",
			"PASSKEY_GRAPHIC",
			"PAGE_HEADER_LOGO",
			"PAGE_HEADER_BACKGROUND",
			"PAGE_FOOTER_LOGO",
			"PAGE_FOOTER_BACKGROUND",
			"PAGE_BACKGROUND",
			"FORM_BACKGROUND",
			"FORM_LOGO",
			"IDP_BUTTON_ICON",
		) {
			return invalidParameter("Invalid asset Category.")
		}

		if a.ColorMode == nil || !oneOf(a.ColorMode, "LIGHT", "DARK", "DYNAMIC") {
			return invalidParameter("Invalid asset ColorMode.")
		}

		if a.Extension == nil || !oneOf(a.Extension, "ICO", "JPEG", "PNG", "SVG", "WEBP") {
			return invalidParameter("Invalid asset Extension.")
		}

		if len(a.Bytes) > 1000000 {
			return invalidParameter("Asset Bytes exceeds 1000000 bytes.")
		}

		if a.ResourceId != nil && !assetResource.MatchString(*a.ResourceId) {
			return invalidParameter("Invalid asset ResourceId.")
		}
	}

	return nil
}

func loadBranding(tx storage.ReadTx, scope request.Scope, p *pool, id *string) (*branding, error) {
	if !brandingID.MatchString(common.Value(id)) {
		return nil, invalidParameter("Invalid ManagedLoginBrandingId.")
	}

	record, ok, err := storage.Read[branding](tx, brandingKey(scope, p.ID, *id))
	if err != nil {
		return nil, err
	}

	if !ok {
		return nil, notFound("Managed login branding does not exist.")
	}

	return record, nil
}

func (s *Service) CreateManagedLoginBranding(
	ctx context.Context, in *api.CreateManagedLoginBrandingRequest,
) (*api.CreateManagedLoginBrandingResponse, error) {
	output, operationErr := convert[api.ManagedLoginBrandingType](in)
	if operationErr != nil {
		return nil, operationErr
	}

	if validationErr := checkBranding(&output); validationErr != nil {
		return nil, validationErr
	}

	operationErr = s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		c, err := loadClient(tx, scope, p, in.ClientId)
		if err != nil {
			return err
		}

		records, err := storage.Records[branding](tx, scope.Prefix(service, "branding")+p.ID+"/")
		if err != nil {
			return err
		}

		for _, record := range records {
			if record.Client == c.ID {
				return awserror.New("ManagedLoginBrandingExistsException", "Branding already exists for client.")
			}
		}

		output.ManagedLoginBrandingId = new(newSub())
		output.CreationDate, output.LastModifiedDate = common.SecondsMilli(s.now()), common.SecondsMilli(s.now())
		return storage.Write(
			tx,
			brandingKey(scope, p.ID, *output.ManagedLoginBrandingId),
			branding{Client: c.ID, Output: output},
		)
	})
	if operationErr != nil {
		return nil, operationErr
	}

	return &api.CreateManagedLoginBrandingResponse{ManagedLoginBranding: &output}, nil
}

func (s *Service) DescribeManagedLoginBranding(
	ctx context.Context, in *api.DescribeManagedLoginBrandingRequest,
) (*api.DescribeManagedLoginBrandingResponse, error) {
	if common.Value(in.ReturnMergedResources) {
		return nil, invalidParameter("Merged default branding resources are not supported.")
	}

	var record *branding
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		var err error
		record, err = loadBranding(tx, scope, p, in.ManagedLoginBrandingId)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeManagedLoginBrandingResponse{ManagedLoginBranding: &record.Output}, nil
}

func (s *Service) DescribeManagedLoginBrandingByClient(
	ctx context.Context, in *api.DescribeManagedLoginBrandingByClientRequest,
) (*api.DescribeManagedLoginBrandingByClientResponse, error) {
	if common.Value(in.ReturnMergedResources) {
		return nil, invalidParameter("Merged default branding resources are not supported.")
	}

	var out *api.ManagedLoginBrandingType
	err := s.view(ctx, in.UserPoolId, func(tx storage.ReadTx, scope request.Scope, p *pool) error {
		if _, err := loadClient(tx, scope, p, in.ClientId); err != nil {
			return err
		}

		records, err := storage.Records[branding](tx, scope.Prefix(service, "branding")+p.ID+"/")
		if err != nil {
			return err
		}

		for _, record := range records {
			if record.Client == *in.ClientId {
				out = &record.Output
				return nil
			}
		}

		return notFound("Managed login branding does not exist.")
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeManagedLoginBrandingByClientResponse{ManagedLoginBranding: out}, nil
}

func (s *Service) UpdateManagedLoginBranding(
	ctx context.Context, in *api.UpdateManagedLoginBrandingRequest,
) (*api.UpdateManagedLoginBrandingResponse, error) {
	var record *branding
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		var err error
		record, err = loadBranding(tx, scope, p, in.ManagedLoginBrandingId)
		if err != nil {
			return err
		}

		if common.Value(in.UseCognitoProvidedValues) {
			if in.Settings != nil || len(in.Assets) > 0 {
				return invalidParameter("Default branding cannot include Settings or Assets.")
			}

			record.Output.Settings, record.Output.Assets = nil, nil
		}

		record.Output.UseCognitoProvidedValues = common.OrElse(in.UseCognitoProvidedValues, new(false))
		if in.Settings != nil {
			record.Output.Settings = in.Settings
		}

		if in.Assets != nil {
			record.Output.Assets = in.Assets
		}

		if err := checkBranding(&record.Output); err != nil {
			return err
		}

		record.Output.LastModifiedDate = common.SecondsMilli(s.now())
		return storage.Write(tx, brandingKey(scope, p.ID, *in.ManagedLoginBrandingId), record)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateManagedLoginBrandingResponse{ManagedLoginBranding: &record.Output}, nil
}

func (s *Service) DeleteManagedLoginBranding(
	ctx context.Context, in *api.DeleteManagedLoginBrandingRequest,
) (*api.DeleteManagedLoginBrandingOutput, error) {
	err := s.update(ctx, in.UserPoolId, func(tx storage.WriteTx, scope request.Scope, p *pool) error {
		if _, err := loadBranding(tx, scope, p, in.ManagedLoginBrandingId); err != nil {
			return err
		}

		tx.Delete(brandingKey(scope, p.ID, *in.ManagedLoginBrandingId))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteManagedLoginBrandingOutput{}, nil
}

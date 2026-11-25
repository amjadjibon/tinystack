package iam

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"slices"
	"strings"
	"time"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/iam"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	kindServiceCred           = "service-credential"
	maxServiceCredsPerService = 2
	maxCredentialAgeDays      = 36600
)

var supportedCredentialServices = map[string]bool{
	"codecommit.amazonaws.com": true,
	"cassandra.amazonaws.com":  true,
	"hdfs.amazonaws.com":       true,
	"bedrock.amazonaws.com":    true,
}

type serviceCredential struct {
	UserName        string
	ID              string
	ServiceName     string
	ServiceUserName string
	ServicePassword string
	Status          string
	CreateDate      int64
	ExpirationDate  int64 `json:",omitempty"`
}

func randomServicePassword() string {
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	return base64.StdEncoding.EncodeToString(buf)
}

func serviceCredsOf(tx storage.ReadTx, account, userName string) ([]*serviceCredential, error) {
	all, err := list[serviceCredential](tx, account, kindServiceCred)
	if err != nil {
		return nil, err
	}

	var out []*serviceCredential
	for _, c := range all {
		if strings.EqualFold(c.UserName, userName) {
			out = append(out, c)
		}
	}

	return out, nil
}

func renameServiceCreds(tx storage.WriteTx, account, from, to string) error {
	creds, err := serviceCredsOf(tx, account, from)
	if err != nil {
		return err
	}

	for _, c := range creds {
		c.UserName = to
		if writeErr := storage.Write(tx, key(account, kindServiceCred, c.ID), c); writeErr != nil {
			return writeErr
		}
	}

	return nil
}

func serviceCredentialOutput(c *serviceCredential) *api.ServiceSpecificCredential {
	out := &api.ServiceSpecificCredential{
		CreateDate:                  millis(c.CreateDate),
		ServiceName:                 new(c.ServiceName),
		ServiceUserName:             new(c.ServiceUserName),
		ServicePassword:             new(c.ServicePassword),
		ServiceSpecificCredentialId: new(c.ID),
		UserName:                    new(c.UserName),
		Status:                      new(c.Status),
	}
	if c.ExpirationDate > 0 {
		out.ExpirationDate = millis(c.ExpirationDate)
	}

	return out
}

func loadUserServiceCred(tx storage.ReadTx, account, userName string, id *string) (*serviceCredential, string, error) {
	if err := required("ServiceSpecificCredentialId", id); err != nil {
		return nil, "", err
	}

	ck := key(account, kindServiceCred, *id)
	c, ok, err := storage.Read[serviceCredential](tx, ck)
	if err != nil {
		return nil, "", err
	}

	if !ok || !strings.EqualFold(c.UserName, userName) {
		return nil, "", noSuchEntity("The service-specific credential with id %s cannot be found.", *id)
	}

	return c, ck, nil
}

func (s *Service) CreateServiceSpecificCredential(
	ctx context.Context,
	in *api.CreateServiceSpecificCredentialRequest,
) (*api.CreateServiceSpecificCredentialResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	serviceName := common.Value(in.ServiceName)
	if !supportedCredentialServices[serviceName] {
		return nil, awserror.New("ServiceNotSupportedException", "Service "+serviceName+" is not supported.")
	}

	var expiration int64
	now := s.now()
	if in.CredentialAgeDays != nil {
		if *in.CredentialAgeDays < 1 || *in.CredentialAgeDays > maxCredentialAgeDays {
			return nil, validation("CredentialAgeDays must be between 1 and %d", maxCredentialAgeDays)
		}

		expiration = now.Add(time.Duration(*in.CredentialAgeDays) * 24 * time.Hour).UnixMilli()
	}

	var stored *serviceCredential
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := loadUser(tx, m.AccountID, in.UserName)
		if userErr != nil {
			return userErr
		}

		existing, listErr := serviceCredsOf(tx, m.AccountID, u.Name)
		if listErr != nil {
			return listErr
		}

		count := 0
		for _, c := range existing {
			if c.ServiceName == serviceName {
				count++
			}
		}

		if count >= maxServiceCredsPerService {
			return limitExceeded("Cannot exceed quota for service-specific credentials per service: %d",
				maxServiceCredsPerService)
		}

		stored = &serviceCredential{
			UserName:        u.Name,
			ID:              newID("ACCA"),
			ServiceName:     serviceName,
			ServiceUserName: u.Name + "-at-" + m.AccountID,
			ServicePassword: randomServicePassword(),
			Status:          keyActive,
			CreateDate:      now.UnixMilli(),
			ExpirationDate:  expiration,
		}
		return storage.Write(tx, key(m.AccountID, kindServiceCred, stored.ID), stored)
	})
	if err != nil {
		return nil, err
	}

	return &api.CreateServiceSpecificCredentialResponse{
		ServiceSpecificCredential: serviceCredentialOutput(stored),
	}, nil
}

func (s *Service) ListServiceSpecificCredentials(
	ctx context.Context,
	in *api.ListServiceSpecificCredentialsRequest,
) (*api.ListServiceSpecificCredentialsResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	serviceName := common.Value(in.ServiceName)
	if serviceName != "" && !supportedCredentialServices[serviceName] {
		return nil, awserror.New("ServiceNotSupportedException", "Service "+serviceName+" is not supported.")
	}

	var creds []api.ServiceSpecificCredentialMetadata
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var listCreds []*serviceCredential
		var listErr error
		if common.Value(in.AllUsers) {
			listCreds, listErr = list[serviceCredential](tx, m.AccountID, kindServiceCred)
		} else {
			u, _, userErr := keyUser(tx, &m, in.UserName)
			if userErr != nil {
				return userErr
			}

			listCreds, listErr = serviceCredsOf(tx, m.AccountID, u.Name)
		}

		if listErr != nil {
			return listErr
		}

		for _, c := range listCreds {
			if serviceName != "" && c.ServiceName != serviceName {
				continue
			}

			meta := api.ServiceSpecificCredentialMetadata{
				UserName:                    new(c.UserName),
				Status:                      new(c.Status),
				ServiceUserName:             new(c.ServiceUserName),
				CreateDate:                  millis(c.CreateDate),
				ServiceSpecificCredentialId: new(c.ID),
				ServiceName:                 new(c.ServiceName),
			}
			if c.ExpirationDate > 0 {
				meta.ExpirationDate = millis(c.ExpirationDate)
			}

			creds = append(creds, meta)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	id := func(c api.ServiceSpecificCredentialMetadata) string { return *c.ServiceSpecificCredentialId }
	slices.SortFunc(
		creds,
		func(a, b api.ServiceSpecificCredentialMetadata) int { return strings.Compare(id(a), id(b)) },
	)
	selected, truncated, next, err := page(creds, id, in.Marker, in.MaxItems)
	if err != nil {
		return nil, err
	}

	if selected == nil {
		selected = []api.ServiceSpecificCredentialMetadata{}
	}

	return &api.ListServiceSpecificCredentialsResponse{
		ServiceSpecificCredentials: selected,
		IsTruncated:                truncated,
		Marker:                     next,
	}, nil
}

func (s *Service) UpdateServiceSpecificCredential(
	ctx context.Context,
	in *api.UpdateServiceSpecificCredentialRequest,
) (*api.UpdateServiceSpecificCredentialOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	status := common.Value(in.Status)
	if status != keyActive && status != keyInactive {
		return nil, validation("Status must be Active or Inactive")
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := keyUser(tx, &m, in.UserName)
		if userErr != nil {
			return userErr
		}

		c, ck, credErr := loadUserServiceCred(tx, m.AccountID, u.Name, in.ServiceSpecificCredentialId)
		if credErr != nil {
			return credErr
		}

		c.Status = status
		return storage.Write(tx, ck, c)
	})
	if err != nil {
		return nil, err
	}

	return &api.UpdateServiceSpecificCredentialOutput{}, nil
}

func (s *Service) ResetServiceSpecificCredential(
	ctx context.Context,
	in *api.ResetServiceSpecificCredentialRequest,
) (*api.ResetServiceSpecificCredentialResponse, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	var stored *serviceCredential
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := keyUser(tx, &m, in.UserName)
		if userErr != nil {
			return userErr
		}

		var ck string
		var credErr error
		stored, ck, credErr = loadUserServiceCred(tx, m.AccountID, u.Name, in.ServiceSpecificCredentialId)
		if credErr != nil {
			return credErr
		}

		stored.ServicePassword = randomServicePassword()
		return storage.Write(tx, ck, stored)
	})
	if err != nil {
		return nil, err
	}

	return &api.ResetServiceSpecificCredentialResponse{
		ServiceSpecificCredential: serviceCredentialOutput(stored),
	}, nil
}

func (s *Service) DeleteServiceSpecificCredential(
	ctx context.Context,
	in *api.DeleteServiceSpecificCredentialRequest,
) (*api.DeleteServiceSpecificCredentialOutput, error) {
	m, err := request.Global(ctx)
	if err != nil {
		return nil, err
	}

	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		u, _, userErr := keyUser(tx, &m, in.UserName)
		if userErr != nil {
			return userErr
		}

		_, ck, credErr := loadUserServiceCred(tx, m.AccountID, u.Name, in.ServiceSpecificCredentialId)
		if credErr != nil {
			return credErr
		}

		tx.Delete(ck)
		return nil
	})
	if err != nil {
		return nil, err
	}

	return &api.DeleteServiceSpecificCredentialOutput{}, nil
}

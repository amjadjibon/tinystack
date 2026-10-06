package iot

import (
	"context"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/iot"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// prefixIndex is the partition-wide index of endpoint prefixes, whose records
// hold the account each belongs to.
const prefixIndex = "aws/" + service + "/" + kindEndpoint + "/"

// endpointHosts are the hosts DescribeEndpoint reports for each endpoint
// type, around the account's prefix and the Region. The *.localhost names
// resolve to the loopback address.
var endpointHosts = map[string]func(prefix, region string) string{
	"iot:Data":     func(p, r string) string { return p + ".iot." + r + ".localhost" },
	"iot:Data-ATS": func(p, r string) string { return p + "-ats.iot." + r + ".localhost" },
	"iot:CredentialProvider": func(p, r string) string {
		return p + ".credentials.iot." + r + ".localhost"
	},
	"iot:Jobs": func(p, r string) string { return p + ".jobs.iot." + r + ".localhost" },
}

// accountPrefixKey is the key of an account's endpoint prefix, which every
// Region shares.
func accountPrefixKey(account string) string {
	return account + "/" + service + "/" + kindEndpoint + "/prefix"
}

// endpointPrefix returns the account's endpoint prefix: 14 lowercase letters
// and digits, created on first use and unique across accounts.
func endpointPrefix(tx storage.WriteTx, account string) (string, error) {
	stored, found, err := storage.Read[string](tx, accountPrefixKey(account))
	if err != nil || found {
		return common.Value(stored), err
	}

	prefix := common.RandomID(14, common.AlphabetLowerHex)
	for exists(tx, prefixIndex+prefix) {
		prefix = common.RandomID(14, common.AlphabetLowerHex)
	}

	if err := storage.Write(tx, prefixIndex+prefix, account); err != nil {
		return "", err
	}

	return prefix, storage.Write(tx, accountPrefixKey(account), prefix)
}

func (s *Service) DescribeEndpoint(
	ctx context.Context,
	in *api.DescribeEndpointRequest,
) (*api.DescribeEndpointResponse, error) {
	kind := common.Value(in.EndpointType)
	if in.EndpointType == nil {
		kind = "iot:Data"
	}

	host, ok := endpointHosts[kind]
	if !ok {
		return nil, invalid("The endpoint type %s is not supported; use iot:Data, iot:Data-ATS, "+
			"iot:CredentialProvider, or iot:Jobs.", kind)
	}

	var address string
	err := s.update(ctx, func(tx storage.WriteTx, scope request.Scope) error {
		prefix, err := endpointPrefix(tx, scope.AccountID)
		address = host(prefix, scope.Region)
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.DescribeEndpointResponse{EndpointAddress: new(address)}, nil
}

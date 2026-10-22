package route53

import (
	"context"
	"fmt"
	"strings"

	"github.com/miekg/dns"

	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/common"
	resolver "github.com/amjadjibon/tinystack/internal/dns"
	"github.com/amjadjibon/tinystack/internal/storage"
)

// LookupDNS reads the longest matching hosted zone in the selected account.
// Routed records and aliases fail explicitly rather than inventing routing decisions.
func (s *Service) LookupDNS(ctx context.Context, account, name string, qtype uint16) ([]dns.RR, bool, error) {
	var answer []dns.RR
	owned := false
	err := s.store.View(ctx, func(tx storage.ReadTx) error {
		zones, err := storage.Records[zone](tx, key(account, kindZone, ""))
		if err != nil {
			return err
		}

		var selected *zone
		for _, z := range zones {
			apex := common.Value(z.Zone.Name)
			if within(name, apex) && (selected == nil || len(apex) > len(common.Value(selected.Zone.Name))) {
				selected = z
			}
		}

		if selected == nil {
			return nil
		}

		owned = true
		records, err := storage.Records[api.ResourceRecordSet](tx, recordPrefix(account, bareID(selected.Zone.Id)))
		if err != nil {
			return err
		}

		exists := false
		for _, rs := range records {
			if common.Value(rs.Name) == name {
				exists = true
				break
			}
		}

		match := name
		if !exists {
			_, parent, _ := strings.Cut(name, ".")
			match = "\\052." + parent
			for _, rs := range records {
				if common.Value(rs.Name) == match {
					exists = true
					break
				}
			}
		}

		if !exists {
			return resolver.ErrNameNotFound
		}

		for _, rs := range records {
			if common.Value(rs.Name) != match {
				continue
			}

			kind := common.Value(rs.Type)
			if dns.StringToType[kind] != qtype && kind != typeCNAME {
				continue
			}

			if rs.AliasTarget != nil || rs.SetIdentifier != nil {
				return fmt.Errorf("DNS aliases and routing policies are unsupported")
			}

			for _, value := range rs.ResourceRecords {
				rr, parseErr := dns.NewRR(fmt.Sprintf("%s %d IN %s %s", name, common.Value(rs.TTL), kind,
					strings.TrimSpace(common.Value(value.Value))))
				if parseErr != nil {
					return parseErr
				}

				answer = append(answer, rr)
			}
		}

		return nil
	})
	return answer, owned, err
}

package route53

import (
	"context"
	"net"
	"reflect"
	"slices"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/route53"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	checkHTTP         = "HTTP"
	checkHTTPS        = "HTTPS"
	checkHTTPMatch    = "HTTP_STR_MATCH"
	checkHTTPSMatch   = "HTTPS_STR_MATCH"
	checkTCP          = "TCP"
	checkCalculated   = "CALCULATED"
	checkMetric       = "CLOUDWATCH_METRIC"
	maxCheckReference = 64
	defaultChecks     = 100
	healthCheckURL    = "https://route53.amazonaws.com/2013-04-01/healthcheck/"
	noSuchHealthCheck = "NoSuchHealthCheck"
)

// healthCheck is a health check's record.
type healthCheck struct {
	Check api.HealthCheck
	Tags  map[string]string
}

func loadHealthCheck(tx storage.ReadTx, account, id string) (*healthCheck, error) {
	h, found, err := read[healthCheck](tx, key(account, kindHealthCheck, id))
	if err == nil && !found {
		err = awserror.New(noSuchHealthCheck, "No health check exists with the ID "+id)
	}

	return h, err
}

// checkHealthConfig fills in a health check's defaults and checks it.
//
//nolint:gocyclo,cyclop // One check per member.
func checkHealthConfig(tx storage.ReadTx, account string, c *api.HealthCheckConfig) error {
	t := value(c.Type)
	endpoint := t == checkHTTP || t == checkHTTPS || t == checkHTTPMatch || t == checkHTTPSMatch || t == checkTCP
	secure := t == checkHTTPS || t == checkHTTPSMatch
	switch {
	case !endpoint && t != checkCalculated && t != checkMetric:
		return invalid("TinyStack does not support %q health checks.", t)
	case endpoint && c.IPAddress == nil && c.FullyQualifiedDomainName == nil:
		return invalid("An endpoint health check needs an IPAddress or a FullyQualifiedDomainName.")
	case c.IPAddress != nil && net.ParseIP(*c.IPAddress) == nil:
		return invalid("The IPAddress %s is not valid.", *c.IPAddress)
	case t == checkTCP && c.Port == nil:
		return invalid("A TCP health check needs a Port.")
	case c.Port != nil && (*c.Port < 1 || *c.Port > 65535):
		return invalid("The Port must be 1 to 65535.")
	case (t == checkHTTPMatch || t == checkHTTPSMatch) != (c.SearchString != nil):
		return invalid("Only string-matching health checks have a SearchString, and they need one.")
	case len(value(c.SearchString)) > 255 || len(value(c.ResourcePath)) > 255:
		return invalid("The SearchString and ResourcePath must be at most 255 characters long.")
	case c.RequestInterval != nil && *c.RequestInterval != 10 && *c.RequestInterval != 30:
		return invalid("The RequestInterval must be 10 or 30.")
	case c.FailureThreshold != nil && (*c.FailureThreshold < 1 || *c.FailureThreshold > 10):
		return invalid("The FailureThreshold must be 1 to 10.")
	case c.Regions != nil && len(c.Regions) < 3:
		return invalid("A health check needs at least three Regions.")
	case t == checkCalculated && (c.HealthThreshold == nil || *c.HealthThreshold < 0 || *c.HealthThreshold > 256):
		return invalid("A calculated health check needs a HealthThreshold of 0 to 256.")
	case t != checkCalculated && (c.ChildHealthChecks != nil || c.HealthThreshold != nil):
		return invalid("Only calculated health checks have ChildHealthChecks and a HealthThreshold.")
	case t == checkMetric && (c.AlarmIdentifier == nil || c.InsufficientDataHealthStatus == nil):
		return invalid("A CloudWatch metric health check needs an AlarmIdentifier and InsufficientDataHealthStatus.")
	case c.InsufficientDataHealthStatus != nil && !slices.Contains(
		[]string{"Healthy", "Unhealthy", "LastKnownStatus"}, *c.InsufficientDataHealthStatus):
		return invalid("InsufficientDataHealthStatus must be Healthy, Unhealthy, or LastKnownStatus.")
	}

	for _, child := range c.ChildHealthChecks {
		if _, ok := tx.Get(key(account, kindHealthCheck, child)); !ok {
			return awserror.New(noSuchHealthCheck, "No health check exists with the ID "+child)
		}
	}

	if !endpoint {
		return nil
	}

	if c.Port == nil {
		c.Port = new(int64(80))
		if secure {
			c.Port = new(int64(443))
		}
	}

	if c.RequestInterval == nil {
		c.RequestInterval = new(int64(30))
	}

	if c.FailureThreshold == nil {
		c.FailureThreshold = new(int64(3))
	}

	if c.MeasureLatency == nil {
		c.MeasureLatency = new(false)
	}

	if c.EnableSNI == nil && secure {
		c.EnableSNI = new(true)
	}

	c.Inverted, c.Disabled = new(value(c.Inverted)), new(value(c.Disabled))
	return nil
}

func (s *Service) CreateHealthCheck(
	ctx context.Context, in *api.CreateHealthCheckRequest,
) (*api.CreateHealthCheckResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	ref := value(in.CallerReference)
	switch {
	case ref == "" || len(ref) > maxCheckReference:
		return nil, invalid("The CallerReference must be 1 to 64 characters long.")
	case in.HealthCheckConfig == nil:
		return nil, invalid("The HealthCheckConfig is required.")
	}

	out := &api.CreateHealthCheckResponse{}
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if checkErr := checkHealthConfig(tx, m.AccountID, in.HealthCheckConfig); checkErr != nil {
			return checkErr
		}

		refKey := key(m.AccountID, kindReference, kindHealthCheck+"/"+ref)
		if data, taken := tx.Get(refKey); taken {
			h, loadErr := loadHealthCheck(tx, m.AccountID, string(data))
			if loadErr != nil {
				return loadErr
			}

			if !reflect.DeepEqual(h.Check.HealthCheckConfig, in.HealthCheckConfig) {
				return awserror.New("HealthCheckAlreadyExists",
					"A health check with the CallerReference "+ref+" already exists with another configuration.")
			}

			out.HealthCheck = &h.Check
			return nil
		}

		id := newUUID()
		h := &healthCheck{
			Check: api.HealthCheck{
				Id: new(id), CallerReference: new(ref), HealthCheckConfig: in.HealthCheckConfig,
				HealthCheckVersion: new(int64(1)),
			},
			Tags: map[string]string{},
		}
		tx.Put(refKey, []byte(id))
		out.HealthCheck = &h.Check
		return write(tx, key(m.AccountID, kindHealthCheck, id), h)
	})
	if err != nil {
		return nil, err
	}

	out.Location = new(healthCheckURL + value(out.HealthCheck.Id))
	return out, nil
}

func (s *Service) GetHealthCheck(
	ctx context.Context, in *api.GetHealthCheckRequest,
) (*api.GetHealthCheckResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var h *healthCheck
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		h, err = loadHealthCheck(tx, m.AccountID, value(in.HealthCheckId))
		return err
	})
	if err != nil {
		return nil, err
	}

	return &api.GetHealthCheckResponse{HealthCheck: &h.Check}, nil
}

func (s *Service) ListHealthChecks(
	ctx context.Context, in *api.ListHealthChecksRequest,
) (*api.ListHealthChecksResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit, err := maxItems(in.MaxItems, defaultChecks)
	if err != nil {
		return nil, err
	}

	out := &api.ListHealthChecksResponse{
		HealthChecks: []api.HealthCheck{}, Marker: new(value(in.Marker)), IsTruncated: new(false), MaxItems: new(limit),
	}
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(m.AccountID, kindHealthCheck, "")) {
			h, _, readErr := read[healthCheck](tx, k)
			if readErr != nil {
				return readErr
			}

			id := value(h.Check.Id)
			if in.Marker != nil && id < *in.Marker {
				continue
			}

			if int64(len(out.HealthChecks)) == limit {
				out.IsTruncated, out.NextMarker = new(true), new(id)
				return nil
			}

			out.HealthChecks = append(out.HealthChecks, h.Check)
		}

		return nil
	})

	return out, err
}

//nolint:gocyclo,cyclop // One member at a time.
func (s *Service) UpdateHealthCheck(
	ctx context.Context, in *api.UpdateHealthCheckRequest,
) (*api.UpdateHealthCheckResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.HealthCheckId)
	out := &api.UpdateHealthCheckResponse{}
	return out, s.store.Update(ctx, func(tx storage.WriteTx) error {
		h, loadErr := loadHealthCheck(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		if v := in.HealthCheckVersion; v != nil && *v != value(h.Check.HealthCheckVersion) {
			return awserror.New(
				"HealthCheckVersionMismatch",
				"The HealthCheckVersion does not match the health check's.",
			)
		}

		c := *h.Check.HealthCheckConfig
		for _, reset := range in.ResetElements {
			switch reset {
			case "FullyQualifiedDomainName":
				c.FullyQualifiedDomainName = nil
			case "Regions":
				c.Regions = nil
			case "ResourcePath":
				c.ResourcePath = nil
			case "ChildHealthChecks":
				c.ChildHealthChecks = nil
			default:
				return invalid("The element %s cannot be reset.", reset)
			}
		}

		for dst, src := range map[**string]*string{
			&c.IPAddress: in.IPAddress, &c.ResourcePath: in.ResourcePath, &c.SearchString: in.SearchString,
			&c.FullyQualifiedDomainName:     in.FullyQualifiedDomainName,
			&c.InsufficientDataHealthStatus: in.InsufficientDataHealthStatus,
		} {
			if src != nil {
				*dst = src
			}
		}

		for dst, src := range map[**int64]*int64{
			&c.Port: in.Port, &c.FailureThreshold: in.FailureThreshold, &c.HealthThreshold: in.HealthThreshold,
		} {
			if src != nil {
				*dst = src
			}
		}

		flags := map[**bool]*bool{&c.Inverted: in.Inverted, &c.Disabled: in.Disabled, &c.EnableSNI: in.EnableSNI}
		for dst, src := range flags {
			if src != nil {
				*dst = src
			}
		}

		if in.ChildHealthChecks != nil {
			c.ChildHealthChecks = in.ChildHealthChecks
		}

		if in.Regions != nil {
			c.Regions = in.Regions
		}

		if in.AlarmIdentifier != nil {
			c.AlarmIdentifier = in.AlarmIdentifier
		}

		if checkErr := checkHealthConfig(tx, m.AccountID, &c); checkErr != nil {
			return checkErr
		}

		h.Check.HealthCheckConfig = &c
		h.Check.HealthCheckVersion = new(value(h.Check.HealthCheckVersion) + 1)
		out.HealthCheck = &h.Check
		return write(tx, key(m.AccountID, kindHealthCheck, id), h)
	})
}

func (s *Service) DeleteHealthCheck(
	ctx context.Context, in *api.DeleteHealthCheckRequest,
) (*api.DeleteHealthCheckResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	id := value(in.HealthCheckId)
	return &api.DeleteHealthCheckResponse{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		h, loadErr := loadHealthCheck(tx, m.AccountID, id)
		if loadErr != nil {
			return loadErr
		}

		for _, k := range tx.Keys(key(m.AccountID, kindRecord, "")) {
			rs, _, readErr := read[api.ResourceRecordSet](tx, k)
			if readErr != nil {
				return readErr
			}

			if value(rs.HealthCheckId) == id {
				return awserror.New("HealthCheckInUse", "The health check "+id+" is used by record set "+setName(rs))
			}
		}

		tx.Delete(key(m.AccountID, kindHealthCheck, id))
		tx.Delete(key(m.AccountID, kindReference, kindHealthCheck+"/"+value(h.Check.CallerReference)))
		return nil
	})
}

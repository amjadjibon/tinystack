package acm

import (
	"cmp"
	"context"
	"crypto"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"uuid"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	api "github.com/amjadjibon/tinystack/internal/aws/generated/acm"
	"github.com/amjadjibon/tinystack/internal/aws/request"
	"github.com/amjadjibon/tinystack/internal/common"
	"github.com/amjadjibon/tinystack/internal/services/tagging"
	"github.com/amjadjibon/tinystack/internal/storage"
)

const (
	statusIssued    = "ISSUED"
	statusExpired   = "EXPIRED"
	typeIssued      = "AMAZON_ISSUED"
	typeImported    = "IMPORTED"
	methodDNS       = "DNS"
	methodEmail     = "EMAIL"
	enabled         = "ENABLED"
	disabled        = "DISABLED"
	ineligible      = "INELIGIBLE"
	domainSuccess   = "SUCCESS"
	maxNames        = 100
	maxItems        = 1000
	tokenLife       = time.Hour
	maxCertificate  = 32 * 1024
	maxPrivateKey   = 5 * 1024
	maxChain        = 2 * 1024 * 1024
	serverAuthOID   = "1.3.6.1.5.5.7.3.1"
	clientAuthOID   = "1.3.6.1.5.5.7.3.2"
	serverAuthName  = "TLS_WEB_SERVER_AUTHENTICATION"
	clientAuthName  = "TLS_WEB_CLIENT_AUTHENTICATION"
	sortAscending   = "ASCENDING"
	sortDescending  = "DESCENDING"
	sortByCreatedAt = "CREATED_AT"
)

var (
	domainLabel = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)
	tokenFormat = regexp.MustCompile(`^\w{1,32}$`)
	statuses    = []string{
		"PENDING_VALIDATION", statusIssued, "INACTIVE", statusExpired, "VALIDATION_TIMED_OUT", "REVOKED", "FAILED",
	}
	keyTypes = []string{"RSA_1024", algRSA2048, "RSA_3072", "RSA_4096", algEC256, algEC384, "EC_secp521r1"}
)

// checkDomain accepts a fully qualified domain name, optionally with a
// leading wildcard label.
func checkDomain(name string) error {
	if name == "" || len(name) > 253 {
		return invalidParameter("The domain name %q must be 1 to 253 characters long.", name)
	}

	labels := strings.Split(strings.TrimPrefix(name, "*."), ".")
	if len(labels) < 2 {
		return invalidParameter("The domain name %q is not fully qualified.", name)
	}

	for _, l := range labels {
		if !domainLabel.MatchString(l) {
			return invalidParameter("The domain name %q is not valid.", name)
		}
	}

	return nil
}

// names returns a request's domain name followed by its other names.
func names(in *api.RequestCertificateRequest) ([]string, error) {
	domain := common.Value(in.DomainName)
	if domain == "" {
		return nil, awserror.Validation("The DomainName is required.")
	}

	out := []string{domain}
	for _, n := range in.SubjectAlternativeNames {
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}

	if len(out) > maxNames {
		return nil, awserror.New("LimitExceededException",
			"A certificate can have at most 100 domain names, including the DomainName.")
	}

	for _, n := range out {
		if err := checkDomain(n); err != nil {
			return nil, err
		}
	}

	return out, nil
}

// validations describes how each name would be validated.
func validations(scope request.Scope, domains []string, method string, options []api.DomainValidationOption,
) ([]api.DomainValidation, error) {
	custom := map[string]string{}
	for _, o := range options {
		name, domain := common.Value(o.DomainName), common.Value(o.ValidationDomain)
		switch {
		case !slices.Contains(domains, name):
			return nil, awserror.New("InvalidDomainValidationOptionsException",
				"The DomainValidationOptions name "+name+" is not one of the certificate's names.")
		case domain != name && !strings.HasSuffix(name, "."+domain):
			return nil, awserror.New("InvalidDomainValidationOptionsException",
				"The validation domain "+domain+" must be "+name+" or one of its parent domains.")
		}

		custom[name] = domain
	}

	out := make([]api.DomainValidation, 0, len(domains))
	for _, name := range domains {
		base := strings.ToLower(strings.TrimPrefix(name, "*."))
		v := api.DomainValidation{
			DomainName: new(name), ValidationDomain: new(cmp.Or(custom[name], name)),
			ValidationStatus: new(domainSuccess), ValidationMethod: new(method),
		}
		if method == methodDNS {
			sum := sha256.Sum256([]byte(scope.AccountID + "/" + base))
			v.ValidationDomain = new(name)
			v.ResourceRecord = &api.ResourceRecord{
				Name:  new("_" + hex.EncodeToString(sum[:16]) + "." + base + "."),
				Type:  new("CNAME"),
				Value: new("_" + hex.EncodeToString(sum[16:]) + ".tinystack.acm-validations.aws."),
			}
		} else {
			domain := strings.TrimPrefix(*v.ValidationDomain, "*.")
			for _, box := range []string{"admin", "administrator", "hostmaster", "postmaster", "webmaster"} {
				v.ValidationEmails = append(v.ValidationEmails, box+"@"+domain)
			}
		}

		out = append(out, v)
	}

	return out, nil
}

func checkOptions(o *api.CertificateOptions) error {
	if o == nil {
		return nil
	}

	if p := o.CertificateTransparencyLoggingPreference; p != nil && *p != enabled && *p != disabled {
		return awserror.Validation("CertificateTransparencyLoggingPreference must be ENABLED or DISABLED.")
	}

	if e := o.Export; e != nil && *e != enabled && *e != disabled {
		return awserror.Validation("Export must be ENABLED or DISABLED.")
	}

	return nil
}

func tagError(err error) error {
	if errors.Is(err, tagging.ErrLimit) {
		return awserror.New("TooManyTagsException", err.Error())
	}

	return awserror.New("InvalidTagException", err.Error())
}

//nolint:gocyclo,cyclop // One check per member.
func (s *Service) RequestCertificate(
	ctx context.Context, in *api.RequestCertificateRequest,
) (*api.RequestCertificateResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	domains, err := names(in)
	if err != nil {
		return nil, err
	}

	method := cmp.Or(common.Value(in.ValidationMethod), methodEmail)
	alg := cmp.Or(common.Value(in.KeyAlgorithm), algRSA2048)
	switch {
	case method != methodDNS && method != methodEmail:
		return nil, invalidParameter("TinyStack does not support the %s validation method.", method)
	case in.IdempotencyToken != nil && !tokenFormat.MatchString(*in.IdempotencyToken):
		return nil, awserror.Validation("The IdempotencyToken must be 1 to 32 word characters.")
	}

	if optErr := checkOptions(in.Options); optErr != nil {
		return nil, optErr
	}

	checks, err := validations(m.Scope, domains, method, in.DomainValidationOptions)
	if err != nil {
		return nil, err
	}

	tags := tagMap(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, tagError(err)
	}

	ca, err := s.authority(ctx)
	if err != nil {
		return nil, err
	}

	now := s.now()
	cert, body, pemKey, err := ca.issue(domains, alg, now)
	if err != nil {
		return nil, err
	}

	options := &api.CertificateOptions{CertificateTransparencyLoggingPreference: new(enabled), Export: new(disabled)}
	if o := in.Options; o != nil {
		options.CertificateTransparencyLoggingPreference = new(
			cmp.Or(common.Value(o.CertificateTransparencyLoggingPreference),
				enabled),
		)
		options.Export = new(cmp.Or(common.Value(o.Export), disabled))
	}

	if common.Value(options.Export) != enabled {
		// Only exportable certificates keep their private key.
		pemKey = ""
	}

	requested, _ := json.Marshal(in)
	id := uuid.NewV4().String()
	var arn string
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		if in.IdempotencyToken != nil {
			k := key(m.Scope, kindToken, *in.IdempotencyToken)
			t, found, readErr := storage.Read[token](tx, k)
			if readErr != nil {
				return readErr
			}

			if found && t.Expires > common.Unix(now) && t.Request == string(requested) {
				arn = certARN(m.Scope, t.ID)
				return nil
			}

			if writeErr := storage.Write(
				tx,
				k,
				token{ID: id, Expires: common.Unix(now.Add(tokenLife)), Request: string(requested)},
			); writeErr != nil {
				return writeErr
			}
		}

		arn = certARN(m.Scope, id)
		detail := describe(cert, now)
		detail.CertificateArn, detail.DomainName, detail.SubjectAlternativeNames = new(arn), new(domains[0]), domains
		detail.DomainValidationOptions, detail.CreatedAt, detail.IssuedAt = checks, new(
			common.Unix(now),
		), new(
			common.Unix(now),
		)
		detail.Type, detail.KeyAlgorithm, detail.Options = new(typeIssued), new(alg), options
		return storage.Write(tx, key(m.Scope, kindCert, id), certificate{
			Detail: detail, Tags: tags, Body: body, Chain: ca.pem, Key: pemKey,
		})
	})
	if err != nil {
		return nil, err
	}

	return &api.RequestCertificateResponse{CertificateArn: new(arn)}, nil
}

// describe fills in the details a certificate carries.
func describe(c *x509.Certificate, now time.Time) api.CertificateDetail {
	status := statusIssued
	if now.After(c.NotAfter) {
		status = statusExpired
	}

	d := api.CertificateDetail{
		Serial: new(serialString(c.SerialNumber)), Subject: new(c.Subject.String()), Issuer: new(issuerName(c)),
		Status: new(status), NotBefore: new(common.Unix(c.NotBefore)), NotAfter: new(common.Unix(c.NotAfter)),
		SignatureAlgorithm: new(signatureName(c.SignatureAlgorithm)), RenewalEligibility: new(ineligible),
	}
	usages := map[x509.KeyUsage]string{
		x509.KeyUsageDigitalSignature: "DIGITAL_SIGNATURE", x509.KeyUsageContentCommitment: "NON_REPUDIATION",
		x509.KeyUsageKeyEncipherment: "KEY_ENCIPHERMENT", x509.KeyUsageDataEncipherment: "DATA_ENCIPHERMENT",
		x509.KeyUsageKeyAgreement: "KEY_AGREEMENT", x509.KeyUsageCertSign: "CERTIFICATE_SIGNING",
		x509.KeyUsageCRLSign: "CRL_SIGNING", x509.KeyUsageEncipherOnly: "ENCIPHER_ONLY",
		x509.KeyUsageDecipherOnly: "DECIPHER_ONLY",
	}
	for bit := x509.KeyUsageDigitalSignature; bit <= x509.KeyUsageDecipherOnly; bit <<= 1 {
		if c.KeyUsage&bit != 0 {
			d.KeyUsages = append(d.KeyUsages, api.KeyUsage{Name: new(usages[bit])})
		}
	}

	for _, u := range c.ExtKeyUsage {
		switch u {
		case x509.ExtKeyUsageServerAuth:
			d.ExtendedKeyUsages = append(d.ExtendedKeyUsages,
				api.ExtendedKeyUsage{Name: new(serverAuthName), OID: new(serverAuthOID)})
		case x509.ExtKeyUsageClientAuth:
			d.ExtendedKeyUsages = append(d.ExtendedKeyUsages,
				api.ExtendedKeyUsage{Name: new(clientAuthName), OID: new(clientAuthOID)})
		}
	}

	return d
}

func (s *Service) DescribeCertificate(
	ctx context.Context, in *api.DescribeCertificateRequest,
) (*api.DescribeCertificateResponse, error) {
	c, err := s.view(ctx, in.CertificateArn)
	if err != nil {
		return nil, err
	}

	d := c.Detail
	if d.InUseBy == nil {
		d.InUseBy = []string{}
	}

	return &api.DescribeCertificateResponse{Certificate: &d}, nil
}

// view reads the certificate arn names.
func (s *Service) view(ctx context.Context, arn *string) (*certificate, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	var c *certificate
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		var loadErr error
		c, loadErr = load(tx, m.Scope, arn)
		return loadErr
	})
	if err != nil {
		return nil, err
	}

	if common.Value(c.Detail.Status) == statusIssued &&
		s.now().After(time.UnixMilli(int64(common.Value(c.Detail.NotAfter)*1000))) {
		c.Detail.Status = new(statusExpired)
	}

	return c, nil
}

//nolint:gocyclo,cyclop // One filter per member.
func (s *Service) ListCertificates(
	ctx context.Context, in *api.ListCertificatesRequest,
) (*api.ListCertificatesResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	limit := int64(maxItems)
	if in.MaxItems != nil {
		limit = *in.MaxItems
	}

	offset := 0
	if in.NextToken != nil {
		if offset, err = strconv.Atoi(*in.NextToken); err != nil || offset < 0 {
			return nil, awserror.New("InvalidArgsException", "The NextToken is not valid.")
		}
	}

	types := []string{"RSA_1024", algRSA2048}
	var export string
	if f := in.Includes; f != nil {
		if f.KeyTypes != nil {
			types = f.KeyTypes
		}

		export = common.Value(f.ExportOption)
	}

	switch {
	case limit < 1 || limit > maxItems:
		return nil, awserror.Validation("MaxItems must be between 1 and 1000.")
	case in.SortBy != nil && *in.SortBy != sortByCreatedAt:
		return nil, awserror.Validation("SortBy must be CREATED_AT.")
	case in.SortOrder != nil && *in.SortOrder != sortAscending && *in.SortOrder != sortDescending:
		return nil, awserror.Validation("SortOrder must be ASCENDING or DESCENDING.")
	case export != "" && export != enabled && export != disabled:
		return nil, awserror.Validation("exportOption must be ENABLED or DISABLED.")
	}

	for _, st := range in.CertificateStatuses {
		if !slices.Contains(statuses, st) {
			return nil, awserror.Validation("The certificate status %s is not valid.", st)
		}
	}

	for _, t := range types {
		if !slices.Contains(keyTypes, t) {
			return nil, awserror.Validation("The key type %s is not valid.", t)
		}
	}

	var all []*certificate
	err = s.store.View(ctx, func(tx storage.ReadTx) error {
		for _, k := range tx.Keys(key(m.Scope, kindCert, "")) {
			c, _, readErr := storage.Read[certificate](tx, k)
			if readErr != nil {
				return readErr
			}

			all = append(all, c)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	if in.SortBy != nil {
		slices.SortStableFunc(all, func(a, b *certificate) int {
			order := cmp.Compare(common.Value(a.Detail.CreatedAt), common.Value(b.Detail.CreatedAt))
			if common.Value(in.SortOrder) == sortDescending {
				return -order
			}

			return order
		})
	}

	now := s.now()
	out := &api.ListCertificatesResponse{CertificateSummaryList: []api.CertificateSummary{}}
	var matched int
	for _, c := range all {
		d := &c.Detail
		status := common.Value(d.Status)
		if status == statusIssued && now.After(time.UnixMilli(int64(common.Value(d.NotAfter)*1000))) {
			status = statusExpired
		}

		switch {
		case in.CertificateStatuses != nil && !slices.Contains(in.CertificateStatuses, status),
			!slices.Contains(types, common.Value(d.KeyAlgorithm)),
			export != "" && common.Value(common.Value(d.Options).Export) != export:
			continue
		}

		matched++
		if matched <= offset {
			continue
		}

		if int64(len(out.CertificateSummaryList)) == limit {
			out.NextToken = new(strconv.Itoa(offset + int(limit)))
			break
		}

		out.CertificateSummaryList = append(out.CertificateSummaryList, summary(c, status))
	}

	return out, nil
}

func summary(c *certificate, status string) api.CertificateSummary {
	d := &c.Detail
	out := api.CertificateSummary{
		CertificateArn:                       d.CertificateArn,
		DomainName:                           d.DomainName,
		SubjectAlternativeNameSummaries:      d.SubjectAlternativeNames,
		HasAdditionalSubjectAlternativeNames: new(false),
		Status:                               new(status),
		Type:                                 d.Type,
		KeyAlgorithm:                         d.KeyAlgorithm,
		ExportOption:                         common.Value(d.Options).Export,
		InUse:                                new(len(d.InUseBy) > 0),
		Exported:                             new(c.Exported),
		RenewalEligibility:                   d.RenewalEligibility,
		NotBefore:                            d.NotBefore,
		NotAfter:                             d.NotAfter,
		CreatedAt:                            d.CreatedAt,
		IssuedAt:                             d.IssuedAt,
		ImportedAt:                           d.ImportedAt,
	}
	for _, u := range d.KeyUsages {
		out.KeyUsages = append(out.KeyUsages, common.Value(u.Name))
	}

	for _, u := range d.ExtendedKeyUsages {
		out.ExtendedKeyUsages = append(out.ExtendedKeyUsages, common.Value(u.Name))
	}

	return out
}

func (s *Service) DeleteCertificate(
	ctx context.Context, in *api.DeleteCertificateRequest,
) (*api.DeleteCertificateOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	return &api.DeleteCertificateOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, err := load(tx, m.Scope, in.CertificateArn)
		if err != nil {
			return err
		}

		if len(c.Detail.InUseBy) > 0 {
			return awserror.New("ResourceInUseException",
				"Certificate "+*in.CertificateArn+" is in use by "+strings.Join(c.Detail.InUseBy, ", ")+".")
		}

		id, _ := certID(m.Scope, in.CertificateArn)
		tx.Delete(key(m.Scope, kindCert, id))
		return nil
	})
}

func (s *Service) GetCertificate(
	ctx context.Context,
	in *api.GetCertificateRequest,
) (*api.GetCertificateResponse, error) {
	c, err := s.view(ctx, in.CertificateArn)
	if err != nil {
		return nil, err
	}

	if st := common.Value(c.Detail.Status); st != statusIssued && st != statusExpired {
		return nil, awserror.New("RequestInProgressException", "Certificate "+*in.CertificateArn+" is "+st+".")
	}

	out := &api.GetCertificateResponse{Certificate: new(c.Body)}
	if c.Chain != "" {
		out.CertificateChain = new(c.Chain)
	}

	return out, nil
}

//nolint:gocyclo,cyclop // One check per member.
func (s *Service) ImportCertificate(
	ctx context.Context, in *api.ImportCertificateRequest,
) (*api.ImportCertificateResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	switch {
	case len(in.Certificate) == 0 || len(in.Certificate) > maxCertificate:
		return nil, awserror.Validation("The Certificate must be 1 to 32768 bytes.")
	case len(in.PrivateKey) == 0 || len(in.PrivateKey) > maxPrivateKey:
		return nil, awserror.Validation("The PrivateKey must be 1 to 5120 bytes.")
	case len(in.CertificateChain) > maxChain:
		return nil, awserror.Validation("The CertificateChain must be at most 2097152 bytes.")
	case in.CertificateArn != nil && in.Tags != nil:
		return nil, invalidParameter("Tags cannot be applied when reimporting a certificate.")
	}

	certs, err := parseCertificates(in.Certificate)
	if err != nil || len(certs) != 1 {
		return nil, awserror.Validation("The Certificate must be one PEM-encoded X.509 certificate.")
	}

	k, err := parseKey(in.PrivateKey)
	if err != nil {
		return nil, awserror.Validation("The PrivateKey is not a valid PEM private key: %v", err)
	}

	cert := certs[0]
	if pub, ok := k.Public().(interface{ Equal(x crypto.PublicKey) bool }); !ok || !pub.Equal(cert.PublicKey) {
		return nil, awserror.Validation("The private key does not match the certificate's public key.")
	}

	alg := keyAlgorithm(cert.PublicKey)
	if alg == "" {
		return nil, awserror.Validation("The certificate's key algorithm is not supported.")
	}

	var chain string
	if len(in.CertificateChain) > 0 {
		chained, chainErr := parseCertificates(in.CertificateChain)
		if chainErr != nil {
			return nil, awserror.Validation("The CertificateChain must be PEM-encoded X.509 certificates.")
		}

		for _, c := range chained {
			chain += encode(blockCert, c.Raw)
		}
	}

	tags := tagMap(in.Tags)
	if err = tagging.Check(tags); err != nil {
		return nil, tagError(err)
	}

	now := s.now()
	domains := cert.DNSNames
	if len(domains) == 0 {
		domains = []string{cert.Subject.CommonName}
	}

	domain := cmp.Or(cert.Subject.CommonName, domains[0])
	id := uuid.NewV4().String()
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		created := common.Unix(now)
		if in.CertificateArn != nil {
			old, loadErr := load(tx, m.Scope, in.CertificateArn)
			if loadErr != nil {
				return loadErr
			}

			if common.Value(old.Detail.Type) != typeImported {
				return invalidParameter("Only imported certificates can be reimported.")
			}

			id, _ = certID(m.Scope, in.CertificateArn)
			created, tags = common.Value(old.Detail.CreatedAt), old.Tags
		}

		detail := describe(cert, now)
		detail.CertificateArn, detail.DomainName = new(certARN(m.Scope, id)), new(domain)
		detail.SubjectAlternativeNames, detail.CreatedAt, detail.ImportedAt = domains, new(
			created,
		), new(
			common.Unix(now),
		)
		detail.Type, detail.KeyAlgorithm = new(typeImported), new(alg)
		detail.Options = &api.CertificateOptions{
			CertificateTransparencyLoggingPreference: new(enabled), Export: new(disabled),
		}
		return storage.Write(tx, key(m.Scope, kindCert, id), certificate{
			Detail: detail, Tags: tags, Body: encode(blockCert, cert.Raw), Chain: chain,
		})
	})
	if err != nil {
		return nil, err
	}

	return &api.ImportCertificateResponse{CertificateArn: new(certARN(m.Scope, id))}, nil
}

func (s *Service) ExportCertificate(
	ctx context.Context, in *api.ExportCertificateRequest,
) (*api.ExportCertificateResponse, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if n := len(in.Passphrase); n < 4 || n > 128 {
		return nil, awserror.Validation("The Passphrase must be 4 to 128 bytes.")
	}

	var out *api.ExportCertificateResponse
	err = s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := load(tx, m.Scope, in.CertificateArn)
		if loadErr != nil {
			return loadErr
		}

		switch {
		case common.Value(c.Detail.Type) != typeIssued || common.Value(common.Value(c.Detail.Options).Export) != enabled:
			return awserror.Validation("Certificate %s is not exportable.", *in.CertificateArn)
		case common.Value(c.Detail.Status) != statusIssued:
			return awserror.New("RequestInProgressException", "Certificate "+*in.CertificateArn+" is not issued.")
		}

		encrypted, encErr := encryptKey(c.Key, in.Passphrase)
		if encErr != nil {
			return encErr
		}

		out = &api.ExportCertificateResponse{
			Certificate: new(c.Body), CertificateChain: new(c.Chain), PrivateKey: new(encrypted),
		}
		c.Exported = true
		id, _ := certID(m.Scope, in.CertificateArn)
		return storage.Write(tx, key(m.Scope, kindCert, id), c)
	})

	return out, err
}

func (s *Service) UpdateCertificateOptions(
	ctx context.Context, in *api.UpdateCertificateOptionsRequest,
) (*api.UpdateCertificateOptionsOutput, error) {
	m, err := identity(ctx)
	if err != nil {
		return nil, err
	}

	if in.Options == nil {
		return nil, awserror.Validation("The Options are required.")
	}

	if optErr := checkOptions(in.Options); optErr != nil {
		return nil, optErr
	}

	return &api.UpdateCertificateOptionsOutput{}, s.store.Update(ctx, func(tx storage.WriteTx) error {
		c, loadErr := load(tx, m.Scope, in.CertificateArn)
		if loadErr != nil {
			return loadErr
		}

		o := c.Detail.Options
		switch {
		case common.Value(c.Detail.Type) == typeImported:
			return awserror.New("InvalidStateException", "The options of imported certificates cannot be updated.")
		case in.Options.Export != nil && *in.Options.Export != common.Value(o.Export):
			return awserror.Validation("The export option of a certificate cannot be changed.")
		}

		if p := in.Options.CertificateTransparencyLoggingPreference; p != nil {
			o.CertificateTransparencyLoggingPreference = p
		}

		id, _ := certID(m.Scope, in.CertificateArn)
		return storage.Write(tx, key(m.Scope, kindCert, id), c)
	})
}

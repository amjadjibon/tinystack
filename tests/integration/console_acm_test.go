package integration_test

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"math/big"
	"net/http"
	"strings"
	"testing"
	"time"
)

func selfSignedPEM(t *testing.T, name string) (certPEM, keyPEM string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(7), Subject: pkix.Name{CommonName: name}, DNSNames: []string{name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}

	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
}

func TestConsoleACMLifecycle(t *testing.T) {
	srv := server(t)
	const account, region = "000000000000", "eu-west-1"
	send := func(status int, operation string, input map[string]any) map[string]any {
		t.Helper()
		return consoleResponse(t, srv,
			consoleLambdaRequest(t, srv, "acm", operation, account, region, input), status)
	}
	call := func(operation string, input map[string]any) map[string]any {
		t.Helper()
		return send(http.StatusOK, operation, input)["output"].(map[string]any)
	}
	encode := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	requested := call("RequestCertificate", map[string]any{
		"DomainName": "app.example.com", "SubjectAlternativeNames": []string{"www.example.com"},
		"ValidationMethod": "DNS", "KeyAlgorithm": "EC_prime256v1", "Options": map[string]any{"Export": "ENABLED"},
		"Tags": []any{map[string]any{"Key": "team", "Value": "console"}},
	})
	arn := requested["CertificateArn"].(string)
	detail := call("DescribeCertificate", map[string]any{"CertificateArn": arn})["Certificate"].(map[string]any)
	validations := detail["DomainValidationOptions"].([]any)
	if detail["Type"] != "AMAZON_ISSUED" || len(validations) != 2 ||
		validations[0].(map[string]any)["ResourceRecord"] == nil {
		t.Fatal(detail)
	}

	if call("GetCertificate", map[string]any{"CertificateArn": arn})["Certificate"] == "" {
		t.Fatal("missing certificate body")
	}

	exported := call("ExportCertificate", map[string]any{"CertificateArn": arn, "Passphrase": encode("passphrase")})
	if !strings.Contains(exported["PrivateKey"].(string), "ENCRYPTED PRIVATE KEY") {
		t.Fatal(exported)
	}

	call("UpdateCertificateOptions", map[string]any{
		"CertificateArn": arn, "Options": map[string]any{"CertificateTransparencyLoggingPreference": "DISABLED"},
	})
	detail = call("DescribeCertificate", map[string]any{"CertificateArn": arn})["Certificate"].(map[string]any)
	if detail["Options"].(map[string]any)["CertificateTransparencyLoggingPreference"] != "DISABLED" {
		t.Fatal(detail["Options"])
	}

	call("AddTagsToCertificate", map[string]any{
		"CertificateArn": arn, "Tags": []any{map[string]any{"Key": "env", "Value": "dev"}},
	})
	call("RemoveTagsFromCertificate", map[string]any{
		"CertificateArn": arn, "Tags": []any{map[string]any{"Key": "team"}},
	})
	tags := call("ListTagsForCertificate", map[string]any{"CertificateArn": arn})["Tags"].([]any)
	if len(tags) != 1 || tags[0].(map[string]any)["Key"] != "env" {
		t.Fatal(tags)
	}

	// A certificate that was not requested as exportable keeps no private key to export.
	plain := call("RequestCertificate", map[string]any{"DomainName": "plain.example.com"})["CertificateArn"]
	response := send(http.StatusBadRequest, "ExportCertificate", map[string]any{
		"CertificateArn": plain, "Passphrase": encode("passphrase"),
	})
	if !strings.Contains(response["message"].(string), "not exportable") {
		t.Fatal(response)
	}

	certPEM, keyPEM := selfSignedPEM(t, "imported.example.com")
	imported := call("ImportCertificate", map[string]any{
		"Certificate": encode(certPEM), "PrivateKey": encode(keyPEM),
		"Tags": []any{map[string]any{"Key": "source", "Value": "import"}},
	})["CertificateArn"].(string)
	again, againKey := selfSignedPEM(t, "imported.example.com")
	if call("ImportCertificate", map[string]any{
		"CertificateArn": imported, "Certificate": encode(again), "PrivateKey": encode(againKey),
	})["CertificateArn"] != imported {
		t.Fatal("reimport changed the ARN")
	}

	reimported := call("DescribeCertificate", map[string]any{"CertificateArn": imported})["Certificate"].(map[string]any)
	if reimported["Type"] != "IMPORTED" || reimported["ImportedAt"] == nil {
		t.Fatal(reimported)
	}

	// Without key types the list holds RSA_2048 certificates only.
	every := map[string]any{"keyTypes": []string{
		"RSA_1024", "RSA_2048", "RSA_3072", "RSA_4096", "EC_prime256v1", "EC_secp384r1", "EC_secp521r1",
	}}
	summaries := call("ListCertificates", map[string]any{
		"CertificateStatuses": []string{"ISSUED"}, "Includes": every,
	})["CertificateSummaryList"].([]any)
	if len(summaries) != 3 {
		t.Fatal(summaries)
	}

	for _, id := range []any{arn, plain, imported} {
		call("DeleteCertificate", map[string]any{"CertificateArn": id})
	}

	if len(call("ListCertificates", map[string]any{"Includes": every})["CertificateSummaryList"].([]any)) != 0 {
		t.Fatal("certificates remain")
	}

	response = send(http.StatusBadRequest, "DescribeCertificate", map[string]any{"CertificateArn": arn})
	if response["code"] != "ResourceNotFoundException" {
		t.Fatal(response)
	}
}

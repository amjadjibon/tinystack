// Package restxml implements the AWS REST XML protocol from generated bindings:
// the rest package places HTTP-bound members, and generated encoding/xml tags
// describe XML bodies.
package restxml

import (
	"bytes"
	"crypto/md5" // #nosec G501 -- Content-MD5 is the protocol's digest.
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"

	"github.com/amjadjibon/tinystack/internal/aws/awserror"
	"github.com/amjadjibon/tinystack/internal/aws/catalog"
	"github.com/amjadjibon/tinystack/internal/aws/protocol/rest"
)

// Decode fills input from the request. labels holds the decoded URI labels and
// body the request payload. Modeled inputs the service does not implement fail,
// and so does a payload that does not match its Content-MD5.
func Decode(r *http.Request, b *catalog.Binding, labels map[string]string, body []byte, input any) error {
	if digest := r.Header.Get("Content-MD5"); digest != "" {
		want, err := base64.StdEncoding.DecodeString(digest)
		if err != nil || len(want) != md5.Size {
			return &awserror.Error{
				Code: "InvalidDigest", Message: "The Content-MD5 you specified was invalid.", StatusCode: 400,
			}
		}

		if got := md5.Sum(body); !bytes.Equal(got[:], want) { // #nosec G401 -- the protocol's digest.
			return &awserror.Error{
				Code: "BadDigest", Message: "The Content-MD5 you specified did not match what we received.",
				StatusCode: 400,
			}
		}
	}

	return rest.Decode(r, b, labels, body, input, decodeXML)
}

// decodeXML decodes a body strictly: elements the generated type does not
// declare fail instead of being ignored.
func decodeXML(body []byte, v any) error {
	malformed := awserror.New("MalformedXML", "The XML you provided was not well-formed or did not validate")
	if err := xml.Unmarshal(body, v); err != nil {
		return malformed
	}

	known := map[string]bool{}
	t := reflect.TypeOf(v).Elem()
	for i := range t.NumField() {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("xml"), ",")
		name, _, _ = strings.Cut(name, ">")
		if name != "" && name != "-" && t.Field(i).Name != "XMLName" {
			known[name] = true
		}
	}

	dec := xml.NewDecoder(bytes.NewReader(body))
	depth := 0
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}

		if err != nil {
			return malformed
		}

		switch el := tok.(type) {
		case xml.StartElement:
			depth++
			if depth == 2 && !known[el.Name.Local] {
				return rest.NotImplemented("TinyStack does not support the " + el.Name.Local + " element.")
			}
		case xml.EndElement:
			depth--
		}
	}
}

// Write encodes a successful response. HEAD responses carry headers only.
func Write(w http.ResponseWriter, b *catalog.Binding, output any, head bool) {
	resp := rest.Encode(w, b, output)
	body := resp.Payload
	if resp.Document || resp.Structure != nil {
		var encoded bytes.Buffer
		var err error
		if resp.Structure != nil {
			// A structure payload is the body, named after its member.
			err = xml.NewEncoder(&encoded).EncodeElement(
				resp.Structure,
				xml.StartElement{Name: xml.Name{Local: resp.StructureName}},
			)
		} else {
			err = xml.NewEncoder(&encoded).Encode(output)
		}

		if err != nil {
			WriteError(w, awserror.From(err), "", "", head)
			return
		}

		// encoding/xml invents namespace prefixes; XML Schema instance
		// attributes, such as a grantee's xsi:type, take their usual prefix.
		xsi := bytes.ReplaceAll(encoded.Bytes(), []byte("_XMLSchema-instance"), []byte("xsi"))
		body = append([]byte(xml.Header), xsi...)
		w.Header().Set("Content-Type", "application/xml")
	}

	rest.Send(w, resp.Status, body, head)
}

// WriteError encodes a REST XML error: S3's unwrapped <Error> element, or,
// when namespace is set, the <ErrorResponse> in namespace that wraps the
// errors of other REST XML services such as Route 53. HEAD responses carry no
// body, so clients infer the error from the status code.
func WriteError(w http.ResponseWriter, e *awserror.Error, requestID, namespace string, head bool) {
	var b bytes.Buffer
	b.WriteString(xml.Header)
	if namespace != "" {
		b.WriteString(`<ErrorResponse xmlns="`)
		_ = xml.EscapeText(&b, []byte(namespace))
		b.WriteString(`"><Error><Type>` + e.Fault() + "</Type><Code>")
	} else {
		b.WriteString("<Error><Code>")
	}

	_ = xml.EscapeText(&b, []byte(e.Code))
	b.WriteString("</Code><Message>")
	_ = xml.EscapeText(&b, []byte(e.Message))
	if namespace != "" {
		b.WriteString("</Message></Error><RequestId>")
		_ = xml.EscapeText(&b, []byte(requestID))
		b.WriteString("</RequestId></ErrorResponse>")
	} else {
		b.WriteString("</Message><RequestId>")
		_ = xml.EscapeText(&b, []byte(requestID))
		b.WriteString("</RequestId></Error>")
	}
	w.Header().Set("Content-Type", "application/xml")
	w.Header().Del("Content-Length")
	w.WriteHeader(e.StatusCode)
	if !head {
		_, _ = w.Write(b.Bytes())
	}
}

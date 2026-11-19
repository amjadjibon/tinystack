package tabular

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"regexp"
	"slices"
	"strings"
)

var xmlRowTag = regexp.MustCompile(`^[\p{L}_][\p{L}\p{N}\p{M}_.-]*$`)

// ValidXMLRowTag reports whether a classifier names an unqualified XML element.
func ValidXMLRowTag(tag string) bool { return xmlRowTag.MatchString(tag) }

type xmlField struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []xmlField `xml:",any"`
}

func xmlFields(row *xmlField) map[string]string {
	types := map[string]string{}
	repeated := map[string]bool{}
	for _, a := range row.Attrs {
		types["_"+strings.ToLower(a.Name.Local)] = scalarType(a.Value)
	}

	for i := range row.Children {
		child := &row.Children[i]
		n := strings.ToLower(child.XMLName.Local)
		t := xmlFieldType(child)
		if old, ok := types[n]; ok {
			t = mergeType(old, t)
			repeated[n] = true
		}

		types[n] = t
	}

	for n := range repeated {
		types[n] = "array<" + nonemptyXMLType(types[n]) + ">"
	}

	if text := strings.TrimSpace(row.Text); text != "" {
		types["_value"] = scalarType(text)
	}

	return types
}

func xmlFieldType(field *xmlField) string {
	if len(field.Children) == 0 && len(field.Attrs) == 0 {
		return scalarType(strings.TrimSpace(field.Text))
	}

	types := xmlFields(field)
	var fields []string
	for _, name := range slices.Sorted(maps.Keys(types)) {
		fields = append(fields, name+":"+nonemptyXMLType(types[name]))
	}

	return "struct<" + strings.Join(fields, ",") + ">"
}

func nonemptyXMLType(t string) string {
	if t == "" {
		return hiveString
	}

	return t
}

func classifyXML(body []byte, rowTag string) (*Classification, error) {
	d := xml.NewDecoder(bytes.NewReader(body))
	types := map[string]string{}
	c := &Classification{Kind: "xml"}
	for {
		token, err := d.Token()
		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return nil, err
		}

		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != rowTag || bytes.HasSuffix(body[:d.InputOffset()], []byte("/>")) {
			continue
		}

		row := xmlField{}
		if err := d.DecodeElement(&row, &start); err != nil {
			return nil, err
		}

		for name, kind := range xmlFields(&row) {
			types[name] = mergeType(types[name], kind)
		}

		c.Records++
	}

	if len(types) == 0 {
		return nil, fmt.Errorf("no XML row data")
	}

	for _, name := range slices.Sorted(maps.Keys(types)) {
		c.Columns = append(c.Columns, Column{Name: name, Type: nonemptyXMLType(types[name])})
	}

	return c, nil
}

// Package xmlmap implements Smithy REST XML string maps.
package xmlmap

import (
	"encoding/xml"
	"slices"
)

// Strings preserves map semantics in callers and emits deterministic XML entries.
type Strings map[string]string

func (m Strings) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}

	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	slices.Sort(keys)
	for _, k := range keys {
		entry := struct {
			Key   string `xml:"key"`
			Value string `xml:"value"`
		}{k, m[k]}
		if err := e.EncodeElement(entry, xml.StartElement{Name: xml.Name{Local: "entry"}}); err != nil {
			return err
		}
	}

	return e.EncodeToken(start.End())
}

func (m *Strings) UnmarshalXML(d *xml.Decoder, start xml.StartElement) error {
	var entries struct {
		Entries []struct {
			Key   string `xml:"key"`
			Value string `xml:"value"`
		} `xml:"entry"`
	}
	if err := d.DecodeElement(&entries, &start); err != nil {
		return err
	}

	*m = make(Strings, len(entries.Entries))
	for _, entry := range entries.Entries {
		(*m)[entry.Key] = entry.Value
	}

	return nil
}

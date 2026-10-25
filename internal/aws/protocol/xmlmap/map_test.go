package xmlmap

import (
	"encoding/xml"
	"testing"
)

func TestStringsXML(t *testing.T) {
	input := struct {
		Endpoints Strings  `xml:"Endpoints"`
		XMLName   xml.Name `xml:"Result"`
	}{Endpoints: Strings{"ipv6": "six", "ipv4": "four"}}
	body, err := xml.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}

	var output struct {
		Endpoints Strings `xml:"Endpoints"`
	}

	if err := xml.Unmarshal(body, &output); err != nil {
		t.Fatal(err)
	}

	if output.Endpoints["ipv4"] != "four" || output.Endpoints["ipv6"] != "six" {
		t.Fatalf("%s: %+v", body, output)
	}
}

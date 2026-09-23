package main

import (
	"encoding/json"
	"testing"
)

func TestRESTBindingFixture(t *testing.T) {
	var m model
	err := json.Unmarshal([]byte(`{
  "shapes": {
    "fixture#Upload": {
      "type": "operation",
      "input": {
        "target": "fixture#Input"
      },
      "output": {
        "target": "fixture#Output"
      },
      "traits": {
        "smithy.api#http": {
          "method": "PUT",
          "uri": "/objects/{Key}",
          "code": 201
        }
      }
    },
    "fixture#Input": {
      "type": "structure",
      "members": {
        "Key": {
          "target": "smithy.api#String",
          "traits": {
            "smithy.api#httpLabel": {}
          }
        },
        "Body": {
          "target": "smithy.api#Blob",
          "traits": {
            "smithy.api#httpPayload": {}
          }
        },
        "Checksum": {
          "target": "smithy.api#String",
          "traits": {
            "smithy.api#httpHeader": "X-Checksum"
          }
        }
      }
    },
    "fixture#Output": {
      "type": "structure",
      "members": {
        "ETag": {
          "target": "smithy.api#String",
          "traits": {
            "smithy.api#httpHeader": "ETag"
          }
        }
      }
    }
  }
}`), &m)
	if err != nil {
		t.Fatal(err)
	}

	b, err := compileBinding(m, "fixture#Upload", "restJson1", "")
	if err != nil {
		t.Fatal(err)
	}

	if b.Method != "PUT" || b.URI != "/objects/{Key}" || b.StatusCode != 201 || b.Input["Key"].Location != "label" ||
		b.Input["Body"].Location != "payload" ||
		b.Input["Checksum"].Name != "X-Checksum" ||
		b.Output["ETag"].Location != "header" {
		t.Fatalf("lost wire metadata: %#v", b)
	}

	if _, err = compileBinding(m, "fixture#Upload", "unknown", ""); err == nil {
		t.Fatal("unsupported protocol accepted")
	}
}

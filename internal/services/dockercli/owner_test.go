package dockercli

import (
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage/memory"
)

func TestRuntimeOwnerPersistsAndIsolatesStores(t *testing.T) {
	store := memory.New(nil, nil)
	owner, err := RuntimeOwner(store, "aws/test/runtime-owner")
	if err != nil || owner == "" {
		t.Fatal(owner, err)
	}

	recovered, err := RuntimeOwner(store, "aws/test/runtime-owner")
	if err != nil || recovered != owner {
		t.Fatal("owner changed on recovery", recovered, err)
	}

	foreign, err := RuntimeOwner(memory.New(nil, nil), "aws/test/runtime-owner")
	if err != nil || foreign == owner || foreign == "" {
		t.Fatal("stores share an owner", foreign, err)
	}
}

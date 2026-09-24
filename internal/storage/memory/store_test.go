package memory_test

import (
	"testing"

	"github.com/amjadjibon/tinystack/internal/storage"
	"github.com/amjadjibon/tinystack/internal/storage/memory"
	"github.com/amjadjibon/tinystack/internal/storage/storagetest"
)

func TestContract(t *testing.T) {
	storagetest.Run(t, func(*testing.T) storage.Store { return &memory.Store{} })
}

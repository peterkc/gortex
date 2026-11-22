package search

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestVectorBackend_EmptySearch(t *testing.T) {
	v := NewDelegatedVector(4, &publicationTestDelegate{}, 0, 0)
	results := v.Search([]float32{1, 0, 0, 0}, 5)
	assert.Nil(t, results)
}

func TestVectorBackend_Dims(t *testing.T) {
	v := NewDelegatedVector(384, &publicationTestDelegate{}, 0, 0)
	assert.Equal(t, 384, v.Dims())
}

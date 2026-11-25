package storage

import "testing"

func TestContractProjectionCursorRoundTripAndEquality(t *testing.T) {
	values := [5]uint64{^uint64(0), 2, 3, 4, 5}
	generation := int64(-6)
	cursor := NewContractProjectionCursor(values[0], values[1], values[2], values[3], values[4], generation)
	instance, analysis, input, bindings, admin, gotGeneration := cursor.Values()
	if got := [5]uint64{instance, analysis, input, bindings, admin}; got != values || gotGeneration != generation {
		t.Fatalf("cursor tuple = %v, %d; want %v, %d", got, gotGeneration, values, generation)
	}
	same := NewContractProjectionCursor(values[0], values[1], values[2], values[3], values[4], generation)
	if cursor != same {
		t.Fatal("equal scalar tuples compare unequal")
	}
	if NewContractProjectionCursor(0, 0, 0, 0, 0, 0) != (ContractProjectionCursor{}) {
		t.Fatal("zero tuple changed")
	}
	for index := 0; index < 6; index++ {
		changed := values
		changedGeneration := generation
		if index < 5 {
			changed[index]++
		} else {
			changedGeneration++
		}
		other := NewContractProjectionCursor(changed[0], changed[1], changed[2], changed[3], changed[4], changedGeneration)
		if cursor == other {
			t.Fatalf("changing scalar %d did not change equality", index)
		}
	}
}

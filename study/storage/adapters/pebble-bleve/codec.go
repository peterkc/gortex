package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"

	"gxstudy/dataset"
)

// Fixed-width headers point into a trailing string/payload area. Only the
// open-ended metadata map uses JSON; graph records and references do not.
func encodeFields(numbers []uint64, fields ...string) []byte {
	header := 8*len(numbers) + 16*len(fields)
	size := header
	for _, field := range fields {
		size += len(field)
	}
	raw := make([]byte, size)
	for i, value := range numbers {
		binary.LittleEndian.PutUint64(raw[i*8:], value)
	}
	offset := header
	for i, field := range fields {
		at := 8*len(numbers) + 16*i
		binary.LittleEndian.PutUint64(raw[at:], uint64(offset))
		binary.LittleEndian.PutUint64(raw[at+8:], uint64(len(field)))
		copy(raw[offset:], field)
		offset += len(field)
	}
	return raw
}
func decodeFields(raw []byte, numbers, fields int) ([]uint64, []string, error) {
	header := 8*numbers + 16*fields
	if len(raw) < header {
		return nil, nil, fmt.Errorf("truncated graph record")
	}
	values := make([]uint64, numbers)
	for i := range values {
		values[i] = binary.LittleEndian.Uint64(raw[8*i:])
	}
	text := make([]string, fields)
	end := uint64(header)
	for i := range text {
		at := 8*numbers + 16*i
		offset, size := binary.LittleEndian.Uint64(raw[at:]), binary.LittleEndian.Uint64(raw[at+8:])
		if offset != end || offset > uint64(len(raw)) || size > uint64(len(raw))-offset {
			return nil, nil, fmt.Errorf("invalid graph payload offset")
		}
		end = offset + size
		text[i] = string(raw[offset:end])
	}
	if end != uint64(len(raw)) {
		return nil, nil, fmt.Errorf("unbound graph payload bytes")
	}
	return values, text, nil
}
func encodeNode(n dataset.Node) ([]byte, error) {
	payload, err := json.Marshal(n.Payload)
	if err != nil {
		return nil, err
	}
	return encodeFields([]uint64{uint64(n.StartLine), uint64(n.EndLine)}, n.ID, n.Kind, n.Name, n.QualName, n.FilePath, n.Language, string(payload)), nil
}
func decodeNode(raw []byte) (n dataset.Node, err error) {
	values, text, err := decodeFields(raw, 2, 7)
	if err != nil {
		return n, err
	}
	n = dataset.Node{ID: text[0], Kind: text[1], Name: text[2], QualName: text[3], FilePath: text[4], Language: text[5], StartLine: int(int64(values[0])), EndLine: int(int64(values[1]))}
	err = json.Unmarshal([]byte(text[6]), &n.Payload)
	return n, err
}
func encodeEdge(e dataset.Edge, from, to uint64) ([]byte, error) {
	payload, err := json.Marshal(e.Payload)
	if err != nil {
		return nil, err
	}
	cross := uint64(0)
	if e.CrossRepo {
		cross = 1
	}
	return encodeFields([]uint64{from, to, math.Float64bits(e.Confidence), uint64(e.Line), cross}, e.Kind, e.FilePath, string(payload)), nil
}
func decodeEdge(raw []byte, resolve func(uint64) (string, error)) (e dataset.Edge, err error) {
	values, text, err := decodeFields(raw, 5, 3)
	if err != nil {
		return e, err
	}
	e.From, err = resolve(values[0])
	if err != nil {
		return e, err
	}
	e.To, err = resolve(values[1])
	if err != nil {
		return e, err
	}
	if values[4] > 1 {
		return e, fmt.Errorf("invalid cross-repository flag")
	}
	e.Kind, e.FilePath, e.Line, e.Confidence, e.CrossRepo = text[0], text[1], int(int64(values[3])), math.Float64frombits(values[2]), values[4] == 1
	err = json.Unmarshal([]byte(text[2]), &e.Payload)
	return e, err
}
func localBytes(id uint64) []byte {
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, id)
	return raw
}
func parseLocal(raw []byte) (uint64, error) {
	if len(raw) != 8 {
		return 0, fmt.Errorf("invalid local ID width")
	}
	return binary.BigEndian.Uint64(raw), nil
}

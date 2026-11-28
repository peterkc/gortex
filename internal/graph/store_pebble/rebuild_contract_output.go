package store_pebble

import (
	"encoding/binary"
	"errors"
	"iter"
	"slices"

	"github.com/cockroachdb/pebble/v2"
	"github.com/zzet/gortex/internal/contracts"
	"github.com/zzet/gortex/internal/graph"
)

// WithContractOutput freezes selected rows before PrepareContracts can change
// the working graph. It uses the same private, bounded record scratch. Each
// replay yields one physical row per batch; coverage counts only omitted rows.
func (r *RebuildContractRecords) WithContractOutput(collect func(func(RebuildContractBatch) error) error,
	use func(iter.Seq2[RebuildContractBatch, error], ContractCoverage) error,
) (err error) {
	if err := r.ready(); err != nil {
		return err
	}
	prefix := rebuildContractRecordKey('u')
	occupied, err := rebuildGuardHasPrefix(r.ctx, r.build.pending, prefix)
	if err != nil {
		return err
	}
	if occupied || collect == nil || use == nil {
		return invalid("contract output scratch is occupied or missing owner")
	}
	if err := r.build.putRebuildScratch(append(prefix, 'a'), nil, goldBufferBytes, "contract output"); err != nil {
		return err
	}
	defer func() {
		err = errors.Join(err, r.build.pending.DeleteRange(prefix, prefixEnd(prefix), pebble.NoSync))
	}()
	rows := append(prefix, 'r')
	var count uint64
	var coverage ContractCoverage
	put := func(tag byte, raw []byte) error {
		if count == ^uint64(0) {
			return invalid("contract output ordinal overflow")
		}
		key := binary.BigEndian.AppendUint64(slices.Clone(rows), count)
		key = append(key, tag)
		if err := r.build.putRebuildScratch(key, raw, goldBufferBytes, "contract output"); err != nil {
			return err
		}
		count++
		return nil
	}
	var collectErr error
	collecting := true
	err = collect(func(batch RebuildContractBatch) error {
		if !collecting {
			return invalid("inactive contract output collector")
		}
		if collectErr == nil {
			collectErr = r.ctx.Err()
		}
		if collectErr == nil {
			collectErr = encodeContractOutput(batch, put, &coverage)
		}
		return collectErr
	})
	collecting = false
	if err = errors.Join(err, collectErr, r.ctx.Err()); err != nil {
		return err
	}
	active := true
	defer func() { active = false }()
	var readErr error
	err = use(func(yield func(RebuildContractBatch, error) bool) {
		if !active {
			yield(RebuildContractBatch{}, invalid("inactive contract output reader"))
			return
		}
		if readErr != nil {
			yield(RebuildContractBatch{}, readErr)
			return
		}
		var next uint64
		stopped := errors.New("contract output visitor stopped")
		err := pendingVisit(r.ctx, r.build.pending, rows, func(s row) error {
			key := s.key[len(rows):]
			if len(key) != 9 || binary.BigEndian.Uint64(key) != next {
				return invalid("contract output ordinal changed")
			}
			batch, err := decodeContractOutput(key[8], s.value)
			if err != nil {
				return err
			}
			next++
			if !yield(batch, nil) {
				return stopped
			}
			return nil
		})
		if errors.Is(err, stopped) {
			return
		}
		if err == nil && next != count {
			err = invalid("contract output count changed")
		}
		if err != nil {
			readErr = err
			yield(RebuildContractBatch{}, err)
		}
	}, coverage)
	return errors.Join(err, readErr, r.ctx.Err())
}

func encodeContractOutput(batch RebuildContractBatch, put func(byte, []byte) error, coverage *ContractCoverage) error {
	for _, n := range batch.Nodes {
		if n == nil {
			return invalid("nil contract output node")
		}
		raw, err := encodeNodeRecord(n, appendContractRowMetadata)
		if err != nil {
			return err
		}
		if err := put('n', raw); err != nil {
			return err
		}
	}
	for _, e := range batch.Edges {
		if e == nil {
			return invalid("nil contract output edge")
		}
		raw, err := encodeEdgeRecord(e, appendContractRowMetadata)
		if err != nil {
			return err
		}
		if err := put('e', raw); err != nil {
			return err
		}
	}
	for id, shape := range batch.Shapes {
		raw, err := encodeContractShape(shape)
		if err != nil {
			return err
		}
		if err := put('t', appendBytes(appendMetaString(nil, id), raw)); err != nil {
			return err
		}
	}
	for _, omitted := range batch.Omitted {
		key, err := contractOmissionKey(omitted)
		if err != nil {
			return err
		}
		if err := put('o', key); err != nil {
			return err
		}
		coverage.OmittedRecords++
	}
	return nil
}

func decodeContractOutput(tag byte, raw []byte) (RebuildContractBatch, error) {
	var batch RebuildContractBatch
	switch tag {
	case 'n':
		n, err := decodeNode(raw)
		if err != nil {
			return batch, err
		}
		batch.Nodes = []*graph.Node{n}
	case 'e':
		e, err := decodeEdge(raw)
		if err != nil {
			return batch, err
		}
		batch.Edges = []*graph.Edge{e}
	case 't':
		d := recordCursor{buf: raw}
		id, value := d.str(), d.bytes()
		if err := d.finish(); err != nil {
			return batch, err
		}
		shape, err := decodeContractShape(value)
		if err != nil {
			return batch, err
		}
		batch.Shapes = map[string]*contracts.Shape{id: shape}
	case 'o':
		omitted, err := decodeContractOmission(raw, []byte{contractOmissionFormat})
		if err != nil {
			return batch, err
		}
		batch.Omitted = []RebuildContractOmission{omitted}
	default:
		return batch, invalid("unknown contract output row")
	}
	return batch, nil
}

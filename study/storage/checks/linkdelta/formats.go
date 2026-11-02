package main

import (
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/apache/arrow-go/v18/arrow"
	"github.com/apache/arrow-go/v18/arrow/array"
	"github.com/apache/arrow-go/v18/arrow/ipc"
	"github.com/apache/arrow-go/v18/arrow/memory"
	parquet "github.com/parquet-go/parquet-go"
)

var deltaSchema = arrow.NewSchema([]arrow.Field{
	{Name: "family", Type: arrow.BinaryTypes.String}, {Name: "key", Type: arrow.BinaryTypes.String},
	{Name: "owner", Type: arrow.BinaryTypes.String}, {Name: "source", Type: arrow.BinaryTypes.String},
	{Name: "target", Type: arrow.BinaryTypes.String}, {Name: "name", Type: arrow.BinaryTypes.String},
	{Name: "value", Type: arrow.BinaryTypes.Binary},
}, nil)

func writeDelta(path, format string, rows []row) (err error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	switch format {
	case "arrow":
		w, e := ipc.NewFileWriter(f, ipc.WithSchema(deltaSchema))
		if e != nil {
			return e
		}
		b := array.NewRecordBuilder(memory.DefaultAllocator, deltaSchema)
		defer b.Release()
		for _, r := range rows {
			for i, v := range []string{r.Family, r.Key, r.Owner, r.Source, r.Target, r.Name} {
				b.Field(i).(*array.StringBuilder).Append(v)
			}
			b.Field(6).(*array.BinaryBuilder).Append(r.Value)
		}
		rec := b.NewRecordBatch()
		e = w.Write(rec)
		rec.Release()
		if e = errors.Join(e, w.Close()); e != nil {
			return e
		}
	case "parquet":
		w := parquet.NewGenericWriter[row](f, parquet.MaxRowsPerRowGroup(1024))
		_, e := w.Write(rows)
		if e = errors.Join(e, w.Close()); e != nil {
			return e
		}
	default:
		return fmt.Errorf("unsupported delta format %q", format)
	}
	return f.Sync()
}
func readDelta(path, format string) (rows []row, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	switch format {
	case "arrow":
		r, e := ipc.NewFileReader(f)
		if e != nil {
			return nil, e
		}
		defer func() { err = errors.Join(err, r.Close()) }()
		if !r.Schema().Equal(deltaSchema) {
			return nil, fmt.Errorf("delta Arrow schema mismatch")
		}
		for i := 0; i < r.NumRecords(); i++ {
			rec, e := r.RecordBatch(i)
			if e != nil {
				return nil, e
			}
			for j := 0; j < int(rec.NumRows()); j++ {
				values := make([]string, 6)
				for c := range values {
					if rec.Column(c).IsNull(j) {
						return nil, fmt.Errorf("null delta field")
					}
					values[c] = rec.Column(c).(*array.String).Value(j)
				}
				if rec.Column(6).IsNull(j) {
					return nil, fmt.Errorf("null delta payload")
				}
				value := append([]byte(nil), rec.Column(6).(*array.Binary).Value(j)...)
				rows = append(rows, row{values[0], values[1], values[2], values[3], values[4], values[5], value})
			}
		}
	case "parquet":
		st, e := f.Stat()
		if e != nil {
			return nil, e
		}
		pf, e := parquet.OpenFile(f, st.Size())
		if e != nil {
			return nil, e
		}
		r := parquet.NewGenericReader[row](pf)
		defer func() { err = errors.Join(err, r.Close()) }()
		batch := make([]row, 256)
		for {
			n, e := r.Read(batch)
			rows = append(rows, batch[:n]...)
			if e == io.EOF {
				break
			}
			if e != nil {
				return nil, e
			}
			if n == 0 {
				return nil, io.ErrNoProgress
			}
		}
	default:
		return nil, fmt.Errorf("unsupported delta format %q", format)
	}
	for _, r := range rows {
		switch r.Family {
		case "node", "edge", "file", "symbol", "content", "vector":
			if len(r.Value) == 0 {
				return nil, fmt.Errorf("missing delta payload")
			}
		case "mask_file", "mask_node":
			if r.Key == "" || len(r.Value) != 0 {
				return nil, fmt.Errorf("invalid tombstone")
			}
		default:
			return nil, fmt.Errorf("unknown row family %q", r.Family)
		}
	}
	return rows, nil
}

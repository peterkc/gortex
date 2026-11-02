package adapter

import (
	"fmt"
	"gxstudy/dataset"
)

// ChangeSource preserves the in-memory fixture path and represents runtime
// updates as a file overlay. It never materializes the unchanged repository.
func ChangeSource(base dataset.Dataset, c FileChange) (dataset.Dataset, error) {
	if base.Source == nil {
		return ApplyChange(base, c), nil
	}
	var removed []string
	for n, err := range base.NodeRows() {
		if err != nil {
			return dataset.Dataset{}, err
		}
		if n.FilePath == c.Path {
			removed = append(removed, n.ID)
		}
	}
	// A second overlay is written to disk by Build before it can be updated again.
	if base.Source.Replacement != nil {
		return dataset.Dataset{}, fmt.Errorf("nested disk overlay requires a sealed rebuild")
	}
	d := dataset.Dataset{Repo: base.Repo, Meta: base.Meta, Source: &dataset.DiskSource{Dir: base.Source.Dir, OmitFile: c.Path, RemovedIDs: removed, Replacement: &c.Replacement}}
	counts, err := dataset.CountRows(d)
	if err != nil {
		return dataset.Dataset{}, err
	}
	d.Meta.Counts = counts
	return d, nil
}

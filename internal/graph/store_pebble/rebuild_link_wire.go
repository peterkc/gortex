package store_pebble

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/cockroachdb/pebble/v2/sstable"
)

const linkFrameBytes = 2 << 20
const linkLanguageLimit = 64

// The private pipe carries bounded control frames only. Raw string bytes are
// retained so the request key is exactly the one encoded into its SST prefix.
func readLinkFrame(in io.Reader) (uint64, []byte, error) {
	var header [12]byte
	if _, err := io.ReadFull(in, header[:]); err != nil {
		return 0, nil, err
	}
	id := binary.BigEndian.Uint64(header[:8])
	size := binary.BigEndian.Uint32(header[8:])
	if size > linkFrameBytes {
		return id, nil, errors.New("oversized link pipe frame")
	}
	body := make([]byte, size)
	_, err := io.ReadFull(in, body)
	if err == io.EOF && size != 0 {
		err = io.ErrUnexpectedEOF
	}
	return id, body, err
}

func writeLinkFrame(out io.Writer, id uint64, body []byte) error {
	if len(body) > linkFrameBytes {
		return errors.New("oversized link pipe frame")
	}
	var header [12]byte
	binary.BigEndian.PutUint64(header[:8], id)
	binary.BigEndian.PutUint32(header[8:], uint32(len(body)))
	for _, data := range [][]byte{header[:], body} {
		n, err := out.Write(data)
		if err != nil {
			return err
		}
		if n != len(data) {
			return io.ErrShortWrite
		}
	}
	return nil
}

type linkPositionedRequest struct {
	LinkRequest
	position int
}

func (r LinkRequest) valid() bool {
	return (r.Kind == LinkName || r.Kind == LinkQualName) && r.Key != "" && len(r.Languages) <= linkLanguageLimit
}

func appendLinkRequest(b []byte, r LinkRequest, position int, remaining int) ([]byte, error) {
	// Include fixed fields and a worst-case length prefix for each string
	// before appending anything from the caller to the bounded frame.
	remaining -= 2 + 2*binary.MaxVarintLen64
	for _, s := range [...]string{r.Key, r.RepoPrefix} {
		remaining -= binary.MaxVarintLen64 + len(s)
		if remaining < 0 {
			return nil, errors.New("link request page exceeds wire byte bound")
		}
	}
	for _, s := range r.Languages {
		remaining -= binary.MaxVarintLen64 + len(s)
		if remaining < 0 {
			return nil, errors.New("link request page exceeds wire byte bound")
		}
	}
	b = binary.AppendVarint(b, int64(position))
	b = append(b, byte(r.Kind))
	b = appendMetaString(b, r.Key)
	b = appendMetaString(b, r.RepoPrefix)
	b = appendBool(b, r.AllRepos)
	b = binary.AppendVarint(b, int64(len(r.Languages)))
	for _, s := range r.Languages {
		b = appendMetaString(b, s)
	}
	return b, nil
}

func readLinkRequests(raw []byte) ([]linkPositionedRequest, error) {
	var requests []linkPositionedRequest
	d := recordCursor{buf: raw}
	last := -1
	for len(d.buf) > 0 {
		position := d.integer()
		if position <= last || position >= LinkPageRequests || d.err != nil || len(d.buf) == 0 {
			return nil, errors.New("invalid link request position")
		}
		r := LinkRequest{Kind: LinkKeyKind(d.buf[0])}
		d.buf = d.buf[1:]
		r.Key, r.RepoPrefix, r.AllRepos = d.str(), d.str(), d.boolean()
		count := d.integer()
		if d.err != nil || count < 0 || count > linkLanguageLimit || count > len(d.buf) {
			return nil, errors.New("invalid link language restrictions")
		}
		for range count {
			r.Languages = append(r.Languages, d.str())
		}
		if d.err != nil {
			return nil, d.err
		}
		if !r.valid() {
			return nil, errors.New("invalid link request key")
		}
		last = position
		requests = append(requests, linkPositionedRequest{r, position})
	}
	if len(requests) == 0 {
		return nil, errors.New("empty link request frame")
	}
	return requests, nil
}

func writeLinkReply(out io.Writer, id uint64, stats LinkPageStats, cause error) error {
	if cause != nil {
		return writeLinkFrame(out, id, append([]byte{1}, cause.Error()...))
	}
	body := []byte{0}
	for _, count := range [...]int{stats.ScannedRows, stats.ResultRows, stats.ResultBytes, stats.SpillRows, stats.SpillBytes} {
		body = binary.AppendVarint(body, int64(count))
	}
	return writeLinkFrame(out, id, body)
}

func readLinkReply(body []byte) (LinkPageStats, error) {
	if len(body) > 1 && body[0] == 1 {
		return LinkPageStats{}, errors.New(string(body[1:]))
	}
	if len(body) == 0 || body[0] != 0 {
		return LinkPageStats{}, errors.New("invalid link reply status")
	}
	d := recordCursor{buf: body[1:]}
	s := LinkPageStats{d.integer(), d.integer(), d.integer(), d.integer(), d.integer()}
	if err := d.finish(); err != nil {
		return LinkPageStats{}, err
	}
	if s.ResultRows < 0 || s.ResultRows > linkResultRows || s.ScannedRows < s.ResultRows ||
		s.ResultBytes < s.ResultRows*(linkResultKeyBytes+1) || s.ResultBytes > linkResultBytes ||
		(s.ResultRows == 0 && s.ResultBytes != 0) || s.SpillBytes < 0 ||
		(s.SpillBytes == 0 && s.SpillRows != 0) || (s.SpillBytes > 0 && (s.SpillRows != s.ResultRows || s.SpillRows == 0)) {
		return LinkPageStats{}, errors.New("invalid link reply counts")
	}
	return s, nil
}

// RunRebuildLinkWorker serves one partition until EOF on its private input.
// Its caller must reserve output for the protocol, with diagnostics on stderr.
// Only the coordinator owns cleanup of the partition directory.
func RunRebuildLinkWorker(ctx context.Context, partitionPath string, spillBytes int, input io.Reader, output io.Writer) (err error) {
	id := uint64(0)
	defer func() {
		if err != nil {
			err = errors.Join(err, writeLinkReply(output, id, LinkPageStats{}, err))
		}
	}()
	if spillBytes < 1 || spillBytes > linkResultBytes {
		return errors.New("invalid link result spill threshold")
	}
	table, err := openLinkTable(ctx, partitionPath)
	if err != nil {
		return fmt.Errorf("open candidate partition %q: %w", partitionPath, err)
	}
	defer func() { err = errors.Join(err, table.Close()) }()
	it, err := table.NewIter(sstable.NoTransforms, nil, nil, sstable.TableBlobContext{})
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, it.Close()) }()
	if err := writeLinkReply(output, id, LinkPageStats{}, nil); err != nil {
		return err
	}
	for {
		next, raw, err := readLinkFrame(input)
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if next <= id {
			return fmt.Errorf("non-increasing link request ID %d after %d", next, id)
		}
		id = next
		requests, err := readLinkRequests(raw)
		if err != nil {
			return err
		}
		stats, err := selectLinkPartition(ctx, it, linkReplyPath(partitionPath, id), spillBytes, requests)
		if err != nil {
			return fmt.Errorf("read candidate partition %q: %w", partitionPath, err)
		}
		if err := writeLinkReply(output, id, stats, nil); err != nil {
			return err
		}
	}
}

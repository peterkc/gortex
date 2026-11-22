package store_pebble

import (
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
)

type searchPosting struct {
	Rowid       int64
	NodePresent bool
}

func searchReadPosting(b []byte) (searchPosting, error) {
	if len(b) == 0 || b[0] != 0x59 {
		return searchPosting{}, errors.New("search codec: posting reference format")
	}
	c := recordCursor{buf: b[1:]}
	p := searchPosting{Rowid: int64(c.integer()), NodePresent: c.boolean()}
	if err := c.finish(); err != nil {
		return searchPosting{}, err
	}
	if p.Rowid <= 0 {
		return searchPosting{}, errors.New("search codec: posting rowid")
	}
	return p, nil
}

type searchTerm struct {
	Text string
	Freq int64
}
type searchDoc struct {
	Rowid, Length              int64
	NodePresent                bool
	ID, Repo, Path, Name, Kind string
	Terms                      []searchTerm // Sorted exact FTS5 terms, not Go-retokenized text.
}
type searchStats struct {
	Rows, Tokens, Count int64
	Repos               []string
}

func searchKey(family string, fields ...string) []byte {
	return keyStrings([]byte{familySymbol}, append([]string{family}, fields...)...)
}
func searchPartial(prefix []byte, field string) []byte {
	b := appendKeyString(append([]byte(nil), prefix...), field)
	return b[:len(b)-2] // String terminator is excluded for a literal prefix.
}
func searchPackDoc(d searchDoc) []byte {
	b := []byte{0x51}
	for _, n := range []int64{d.Rowid, d.Length} {
		b = binary.AppendVarint(b, n)
	}
	b = appendBool(b, d.NodePresent)
	for _, s := range []string{d.ID, d.Repo, d.Path, d.Name, d.Kind} {
		b = appendBytes(b, []byte(s))
	}
	b = binary.AppendVarint(b, int64(len(d.Terms)))
	for _, term := range d.Terms {
		b = appendBytes(b, []byte(term.Text))
		b = binary.AppendVarint(b, term.Freq)
	}
	return b
}
func searchReadDoc(b []byte) (searchDoc, error) {
	if len(b) == 0 || b[0] != 0x51 {
		return searchDoc{}, errors.New("search codec: document format")
	}
	c := recordCursor{buf: b[1:]}
	d := searchDoc{Rowid: int64(c.integer()), Length: int64(c.integer()), NodePresent: c.boolean(), ID: c.str(), Repo: c.str(), Path: c.str(), Name: c.str(), Kind: c.str()}
	n := int64(c.integer())
	if n < 0 || n > int64(len(b)) {
		return searchDoc{}, errors.New("search codec: term count")
	}
	var total int64
	for i := int64(0); i < n; i++ {
		t := searchTerm{Text: c.str(), Freq: int64(c.integer())}
		if t.Text == "" || t.Freq <= 0 || t.Freq > math.MaxInt32 || (len(d.Terms) > 0 && d.Terms[len(d.Terms)-1].Text >= t.Text) {
			return searchDoc{}, errors.New("search codec: term order/frequency")
		}
		total += t.Freq
		d.Terms = append(d.Terms, t)
	}
	if err := c.finish(); err != nil {
		return searchDoc{}, err
	}
	if d.Rowid <= 0 || d.Length < 0 || d.Length > math.MaxInt32 || total != d.Length {
		return searchDoc{}, errors.New("search codec: document length")
	}
	return d, nil
}

// The frozen global average makes the per-prefix normalization factor
// query-independent. A block stores the maximum factor for every ASCII prefix.
func searchFactor(freq float64, length int64, avgdl float64) float64 {
	k1 := float64(1.2)
	b := float64(0.75)
	D := float64(int32(length))
	return float64(float64(freq*(k1+float64(1))) / (freq + float64(k1*(float64(1)-b+float64(b*D)/avgdl))))
}
func searchUpper32(factor float64) float32 {
	upper := float32(factor)
	if float64(upper) < factor {
		upper = math.Nextafter32(upper, float32(math.Inf(1)))
	}
	return upper
}

// searchDocBounds prepares conservative bounds once per document. Each posting
// later merges these numbers without redoing prefix resolution or scoring.
func searchDocBounds(d searchDoc, avgdl float64, prefixID func(string) (uint32, error)) ([]byte, error) {
	freqs := make(map[string]int64)
	for _, t := range d.Terms {
		symbolPrefixes(t.Text, func(p string) { freqs[p] += t.Freq })
	}
	out := appendBool(nil, d.NodePresent)
	for _, prefix := range slices.Sorted(maps.Keys(freqs)) {
		id, err := prefixID(prefix)
		if err != nil {
			return nil, err
		}
		if id == 0 {
			return nil, fmt.Errorf("search codec: missing prefix ID for %q", prefix)
		}
		upper := searchUpper32(searchFactor(float64(freqs[prefix]), d.Length, avgdl))
		out = binary.LittleEndian.AppendUint32(out, id)
		out = binary.LittleEndian.AppendUint32(out, math.Float32bits(upper))
	}
	return out, nil
}

// mergeSearchDocBounds consumes one private scratch value while its DB.Get
// handle is open. A document with no ASCII prefixes still has one flag byte.
func mergeSearchDocBounds(raw []byte, maxima map[uint32]float32) (bool, error) {
	if len(raw) == 0 || raw[0] > 1 || (len(raw)-1)%8 != 0 {
		return false, invalid("search document bounds")
	}
	present := raw[0] == 1
	for raw = raw[1:]; len(raw) != 0; raw = raw[8:] {
		id := binary.LittleEndian.Uint32(raw)
		bits := binary.LittleEndian.Uint32(raw[4:])
		if id == 0 || bits&0x80000000 != 0 || bits&0x7f800000 == 0x7f800000 {
			return false, invalid("search document prefix bound")
		}
		if upper := math.Float32frombits(bits); upper > maxima[id] {
			maxima[id] = upper
		}
	}
	return present, nil
}

func searchPackBlock(postings []searchPosting, maxima map[uint32]float32) []byte {
	ids := slices.Sorted(maps.Keys(maxima))
	width := 1
	if len(ids) > 0 && ids[len(ids)-1] > math.MaxUint8 {
		width = 2
	}
	if len(ids) > 0 && ids[len(ids)-1] > math.MaxUint16 {
		width = 4
	}
	b := binary.AppendVarint([]byte{0x59, byte(width)}, int64(len(ids)))
	for _, id := range ids {
		switch width {
		case 1:
			b = append(b, byte(id))
		case 2:
			b = binary.LittleEndian.AppendUint16(b, uint16(id))
		case 4:
			b = binary.LittleEndian.AppendUint32(b, id)
		}
		b = binary.LittleEndian.AppendUint32(b, math.Float32bits(maxima[id]))
	}
	b = binary.AppendVarint(b, int64(len(postings)))
	for _, p := range postings {
		ref := binary.AppendVarint([]byte{0x59}, p.Rowid)
		ref = appendBool(ref, p.NodePresent)
		b = appendBytes(b, ref)
	}
	return b
}

func searchPrefixID(b []byte, width int) uint32 {
	if width == 1 {
		return uint32(b[0])
	}
	if width == 2 {
		return uint32(binary.LittleEndian.Uint16(b))
	}
	return binary.LittleEndian.Uint32(b)
}

type searchBlockQuery struct {
	Phrases   []string
	PrefixIDs []uint32
	IDF       []float64
	Threshold float64
	CanSkip   bool
}

func searchReadBlock(b []byte, q searchBlockQuery, visit func(searchPosting) error) (bool, error) {
	if len(b) < 2 || b[0] != 0x59 {
		return false, errors.New("search codec: posting block format")
	}
	width := int(b[1])
	if width != 1 && width != 2 && width != 4 {
		return false, errors.New("search codec: prefix ID width")
	}
	c := recordCursor{buf: b[2:]}
	nb := int64(c.integer())
	if c.err != nil {
		return false, c.err
	}
	recordSize := width + 4
	if nb < 0 || nb > int64(len(c.buf)/recordSize) {
		return false, errors.New("search codec: block bound count")
	}
	if len(q.Phrases) != len(q.IDF) || len(q.Phrases) != len(q.PrefixIDs) {
		return false, errors.New("search: bound phrase/IDF mismatch")
	}
	bounds := c.buf[:int(nb)*recordSize]
	c.buf = c.buf[len(bounds):]
	var previous uint32
	for offset := 0; offset < len(bounds); offset += recordSize {
		id := searchPrefixID(bounds[offset:], width)
		bits := binary.LittleEndian.Uint32(bounds[offset+width:])
		if id == 0 || id <= previous || bits&0x80000000 != 0 || bits&0x7f800000 == 0x7f800000 {
			return false, errors.New("search codec: block bound")
		}
		previous = id
	}
	upper := float64(0)
	for i, id := range q.PrefixIDs {
		at := sort.Search(int(nb), func(j int) bool {
			return searchPrefixID(bounds[j*recordSize:], width) >= id
		})
		factor := float64(0)
		if at < int(nb) && searchPrefixID(bounds[at*recordSize:], width) == id {
			bits := binary.LittleEndian.Uint32(bounds[at*recordSize+width:])
			factor = float64(math.Float32frombits(bits))
		}
		upper = upper + float64(q.IDF[i]*factor)
	}
	// Each float32 factor rounds upward from searchScore's float64 factor.
	// Nonnegative IEEE multiplication and summation remain monotone. Phrase
	// order/repeats stay. Strict inequality preserves equal-score document ties.
	if q.CanSkip && upper < q.Threshold {
		return true, nil
	}
	n := int64(c.integer())
	if n < 1 || n > 128 {
		return false, errors.New("search codec: posting block length")
	}
	for i := int64(0); i < n; i++ {
		v := c.bytes()
		if c.err != nil {
			return false, c.err
		}
		d, err := searchReadPosting(v)
		if err != nil {
			return false, err
		}
		if err = visit(d); err != nil {
			return false, err
		}
	}
	return false, c.finish()
}

func searchPackStats(s searchStats) []byte {
	b := []byte{0x53}
	for _, n := range []int64{s.Rows, s.Tokens, s.Count, int64(len(s.Repos))} {
		b = binary.AppendVarint(b, n)
	}
	for _, r := range s.Repos {
		b = appendBytes(b, []byte(r))
	}
	return b
}
func searchReadStats(b []byte) (searchStats, error) {
	if len(b) == 0 || b[0] != 0x53 {
		return searchStats{}, errors.New("search codec: stats format")
	}
	c := recordCursor{buf: b[1:]}
	s := searchStats{Rows: int64(c.integer()), Tokens: int64(c.integer()), Count: int64(c.integer())}
	n := int64(c.integer())
	if n < 0 || n > int64(len(b)) {
		return s, errors.New("search codec: repo count")
	}
	for i := int64(0); i < n; i++ {
		s.Repos = append(s.Repos, c.str())
	}
	if err := c.finish(); err != nil {
		return s, err
	}
	if s.Rows < 0 || s.Tokens < 0 || s.Count < 0 {
		return s, errors.New("search codec: negative stats")
	}
	return s, nil
}

func checkSymbolRow(r row) error {
	family, _, err := readKeyString(r.key[1:])
	if err != nil {
		return err
	}
	switch family {
	case "doc":
		_, err = searchReadDoc(r.value)
	case "stats":
		_, err = searchReadStats(r.value)
	case "post":
		_, err = searchReadBlock(r.value, searchBlockQuery{}, func(searchPosting) error { return nil })
	case "exact", "term":
		if len(r.value) == 0 {
			err = invalid("empty symbol reference")
		}
	case "prefix-df":
		c := recordCursor{buf: r.value}
		count := c.integer()
		id := c.integer()
		if count <= 0 || id <= 0 {
			return invalid("zero symbol prefix count or ID")
		}
		err = c.finish()
	default:
		err = invalid("unknown symbol family")
	}
	return err
}

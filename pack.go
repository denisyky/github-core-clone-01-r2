// Git objects, pkt-line framing and packfiles (hand-rolled, zlib + OFS/REF
// deltas, SHA-1). Faithful port of the Node forge's part-1 helpers.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"io"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

func sha1Hex(data []byte) string {
	h := sha1.Sum(data)
	return hex.EncodeToString(h[:])
}

func isOID(v string) bool {
	if len(v) != 40 {
		return false
	}
	for i := 0; i < 40; i++ {
		c := v[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// --- pkt-line ---

func pktLine(payload []byte) []byte {
	n := len(payload) + 4
	out := make([]byte, 0, n)
	out = append(out, hexDigit(byte(n>>12&0xf)), hexDigit(byte(n>>8&0xf)), hexDigit(byte(n>>4&0xf)), hexDigit(byte(n&0xf)))
	return append(out, payload...)
}

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pktLines(items [][]byte) []byte {
	var out []byte
	for _, it := range items {
		if it == nil {
			out = append(out, pktFlush...)
		} else {
			out = append(out, pktLine(it)...)
		}
	}
	return out
}

type pkt struct {
	kind    string // "data" | "flush" | "delim" | "response-end"
	payload []byte
}

func decodePktLines(buf []byte) ([]pkt, error) {
	var packets []pkt
	off := 0
	for off < len(buf) {
		if off+4 > len(buf) {
			return nil, errors.New("truncated pkt-line")
		}
		head := string(buf[off : off+4])
		if head == "0000" {
			packets = append(packets, pkt{kind: "flush"})
			off += 4
			continue
		}
		if head == "0001" {
			packets = append(packets, pkt{kind: "delim"})
			off += 4
			continue
		}
		if head == "0002" {
			packets = append(packets, pkt{kind: "response-end"})
			off += 4
			continue
		}
		l, err := parseHex4(buf[off : off+4])
		if err != nil || l < 4 || off+l > len(buf) {
			return nil, errors.New("bad pkt-line length")
		}
		packets = append(packets, pkt{kind: "data", payload: buf[off+4 : off+l]})
		off += l
	}
	return packets, nil
}

func parseHex4(b []byte) (int, error) {
	n := 0
	for _, c := range b {
		n <<= 4
		switch {
		case c >= '0' && c <= '9':
			n |= int(c - '0')
		case c >= 'a' && c <= 'f':
			n |= int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			n |= int(c-'A') + 10
		default:
			return 0, errors.New("bad hex")
		}
	}
	return n, nil
}

// decodePktLine decodes one packet at offset.
// Returns len consumed; payload is nil for flush/delim/response-end except
// delim, which returns a 1-byte payload (mirrors the Node helper).
func decodePktLine(buf []byte, offset int) (length int, payload []byte) {
	if offset+4 > len(buf) {
		return 0, nil
	}
	head := string(buf[offset : offset+4])
	if head == "0000" {
		return 4, nil
	}
	if head == "0001" {
		return 4, []byte{1}
	}
	if head == "0002" {
		return 4, nil
	}
	l, err := parseHex4(buf[offset : offset+4])
	if err != nil || l < 4 || offset+l > len(buf) {
		panic("bad pkt-line length")
	}
	return l, buf[offset+4 : offset+l]
}

// --- git objects ---

type gitObject struct {
	typ  string
	data []byte
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ))
	h.Write([]byte{' '})
	h.Write([]byte(intStr(len(data))))
	h.Write([]byte{0})
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

type treeEntry struct {
	mode string
	name string
	oid  string
}

func parseTree(data []byte) ([]treeEntry, error) {
	var entries []treeEntry
	off := 0
	for off < len(data) {
		space := bytes.IndexByte(data[off:], ' ')
		if space < 0 {
			return nil, errors.New("bad tree")
		}
		space += off
		nul := bytes.IndexByte(data[space+1:], 0)
		if nul < 0 {
			return nil, errors.New("bad tree")
		}
		nul += space + 1
		if nul+21 > len(data) {
			return nil, errors.New("bad tree")
		}
		entries = append(entries, treeEntry{
			mode: string(data[off:space]),
			name: string(data[space+1 : nul]),
			oid:  hex.EncodeToString(data[nul+1 : nul+21]),
		})
		off = nul + 21
	}
	return entries, nil
}

func compareTreeNames(a, b treeEntry) int {
	an := a.name
	if a.mode == "40000" {
		an += "/"
	}
	bn := b.name
	if b.mode == "40000" {
		bn += "/"
	}
	return bytes.Compare([]byte(an), []byte(bn))
}

func buildTree(entries []treeEntry) []byte {
	sorted := make([]treeEntry, len(entries))
	copy(sorted, entries)
	// insertion sort is fine (trees are small); use sort for larger ones
	sortTreeEntries(sorted)
	var out []byte
	for _, e := range sorted {
		out = append(out, []byte(e.mode+" "+e.name)...)
		out = append(out, 0)
		raw, _ := hex.DecodeString(e.oid)
		out = append(out, raw...)
	}
	return out
}

func sortTreeEntries(es []treeEntry) {
	// simple insertion sort; tree entry counts are small
	for i := 1; i < len(es); i++ {
		for j := i; j > 0 && compareTreeNames(es[j-1], es[j]) > 0; j-- {
			es[j-1], es[j] = es[j], es[j-1]
		}
	}
}

type commitInfo struct {
	tree      string
	parents   []string
	author    string
	committer string
	message   string
}

func parseCommit(data []byte) commitInfo {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	var c commitInfo
	if blank >= 0 {
		c.message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		key := string(line)
		val := ""
		if sp >= 0 {
			key = string(line[:sp])
			val = string(line[sp+1:])
		}
		switch key {
		case "tree":
			c.tree = val
		case "parent":
			c.parents = append(c.parents, val)
		case "author":
			c.author = val
		case "committer":
			c.committer = val
		}
	}
	return c
}

type tagInfo struct {
	object  string
	typ     string
	tag     string
	tagger  string
	message string
}

func parseTag(data []byte) tagInfo {
	blank := bytes.Index(data, []byte("\n\n"))
	headEnd := len(data)
	if blank >= 0 {
		headEnd = blank
	}
	var t tagInfo
	if blank >= 0 {
		t.message = string(data[blank+2:])
	}
	for _, line := range bytes.Split(data[:headEnd], []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		sp := bytes.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		switch string(line[:sp]) {
		case "object":
			t.object = string(line[sp+1:])
		case "type":
			t.typ = string(line[sp+1:])
		case "tag":
			t.tag = string(line[sp+1:])
		case "tagger":
			t.tagger = string(line[sp+1:])
		}
	}
	return t
}

func buildCommitBytes(tree string, parents []string, message, author string, timestamp int64) []byte {
	who := author + " " + int64Str(timestamp) + " +0000"
	var out []byte
	out = append(out, []byte("tree "+tree+"\n")...)
	for _, p := range parents {
		out = append(out, []byte("parent "+p+"\n")...)
	}
	out = append(out, []byte("author "+who+"\n")...)
	out = append(out, []byte("committer "+who+"\n")...)
	out = append(out, '\n')
	msg := message
	if len(msg) == 0 || msg[len(msg)-1] != '\n' {
		msg += "\n"
	}
	out = append(out, []byte(msg)...)
	return out
}

func int64Str(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

type objLookup func(oid string) *gitObject

func collectReachable(roots []string, lookup objLookup) (map[string]bool, error) {
	seen := make(map[string]bool)
	var queue []string
	for _, r := range roots {
		if isOID(r) && r != zeroOID {
			queue = append(queue, r)
		}
	}
	for len(queue) > 0 {
		oid := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[oid] {
			continue
		}
		obj := lookup(oid)
		if obj == nil {
			return nil, errors.New("object " + oid + " is missing from the store")
		}
		seen[oid] = true
		switch obj.typ {
		case "commit":
			c := parseCommit(obj.data)
			queue = append(queue, c.tree)
			queue = append(queue, c.parents...)
		case "tag":
			queue = append(queue, parseTag(obj.data).object)
		case "tree":
			entries, err := parseTree(obj.data)
			if err != nil {
				return nil, err
			}
			for _, e := range entries {
				if e.mode != "160000" {
					queue = append(queue, e.oid)
				}
			}
		}
	}
	return seen, nil
}

func isAncestor(ancestorOid, oid string, lookup objLookup) bool {
	if ancestorOid == oid {
		return true
	}
	seen := make(map[string]bool)
	stack := []string{oid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if cur == ancestorOid {
			return true
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := lookup(cur)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		stack = append(stack, parseCommit(obj.data).parents...)
	}
	return false
}

func mergeBase(aOid, bOid string, lookup objLookup) string {
	if !isOID(aOid) || !isOID(bOid) {
		return ""
	}
	aSet := make(map[string]bool)
	stack := []string{aOid}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if aSet[cur] {
			continue
		}
		aSet[cur] = true
		obj := lookup(cur)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		stack = append(stack, parseCommit(obj.data).parents...)
	}
	queue := []string{bOid}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		if aSet[cur] {
			return cur
		}
		if seen[cur] {
			continue
		}
		seen[cur] = true
		obj := lookup(cur)
		if obj == nil || obj.typ != "commit" {
			continue
		}
		queue = append(queue, parseCommit(obj.data).parents...)
	}
	return ""
}

// --- packfiles ---

var packTypeCode = map[string]int{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var packCodeType = map[int]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func readVarintSize(buf []byte, offset int) (size int64, first byte, next int) {
	b := buf[offset]
	size = int64(b & 0x0f)
	shift := uint(4)
	next = offset + 1
	for b&0x80 != 0 {
		b = buf[next]
		next++
		size |= int64(b&0x7f) << shift
		shift += 7
	}
	return size, buf[offset], next
}

func readOfsDeltaBase(buf []byte, offset int) (baseOffset int64, next int) {
	b := buf[offset]
	baseOffset = int64(b & 0x7f)
	next = offset + 1
	for b&0x80 != 0 {
		b = buf[next]
		next++
		baseOffset = ((baseOffset + 1) << 7) | int64(b&0x7f)
	}
	return baseOffset, next
}

func readDeltaVarint(buf []byte, offset int) (value int64, next int) {
	var result int64
	var shift uint
	next = offset
	for {
		b := buf[next]
		next++
		result |= int64(b&0x7f) << shift
		shift += 7
		if b&0x80 == 0 {
			break
		}
	}
	return result, next
}

func applyDelta(base, delta []byte) ([]byte, error) {
	off := 0
	src, next := readDeltaVarint(delta, off)
	off = next
	tgt, next := readDeltaVarint(delta, off)
	off = next
	if src != int64(len(base)) {
		return nil, errors.New("delta base size mismatch")
	}
	var out [][]byte
	total := int64(0)
	for off < len(delta) {
		op := delta[off]
		off++
		if op&0x80 != 0 {
			var cpOff int64
			var cpSize int64
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					cpOff |= int64(delta[off]) << (8 * uint(i))
					off++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					cpSize |= int64(delta[off]) << (8 * uint(i))
					off++
				}
			}
			if cpSize == 0 {
				cpSize = 0x10000
			}
			if cpOff+cpSize > int64(len(base)) {
				return nil, errors.New("delta copy out of range")
			}
			out = append(out, base[cpOff:cpOff+cpSize])
			total += cpSize
		} else if op != 0 {
			if off+int(op) > len(delta) {
				return nil, errors.New("delta insert out of range")
			}
			out = append(out, delta[off:off+int(op)])
			total += int64(op)
			off += int(op)
		} else {
			return nil, errors.New("delta opcode 0")
		}
	}
	var result []byte
	for _, p := range out {
		result = append(result, p...)
	}
	if int64(len(result)) != tgt || total != tgt {
		return nil, errors.New("delta target size mismatch")
	}
	return result, nil
}

// countReader counts bytes pulled from the underlying reader and implements
// io.ByteReader so flate reads it directly (no bufio over-read).
type countReader struct {
	r *bytes.Reader
	n int64
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countReader) ReadByte() (byte, error) {
	b, err := c.r.ReadByte()
	if err == nil {
		c.n++
	}
	return b, err
}

func inflateZlib(buf []byte) ([]byte, error) {
	zr, err := zlib.NewReader(bytes.NewReader(buf))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// inflateAt inflates one zlib stream starting at offset and reports the
// offset just past the stream (fast path: the counting reader measures
// exactly how many input bytes flate consumed). Falls back to a binary
// search for the stream end when the fast path is implausible.
func inflateAt(buf []byte, offset int) (data []byte, next int, err error) {
	cr := &countReader{r: bytes.NewReader(buf[offset:])}
	zr, zerr := zlib.NewReader(cr)
	if zerr == nil {
		data, rerr := io.ReadAll(zr)
		zr.Close()
		if rerr == nil && cr.n > 0 && offset+int(cr.n) <= len(buf) {
			return data, offset + int(cr.n), nil
		}
	}
	// Fallback: binary search for the smallest end that inflates.
	end := len(buf)
	best, berr := inflateZlib(buf[offset:end])
	if berr != nil {
		return nil, 0, errors.New("pack entry does not inflate")
	}
	_ = best
	lo := offset + 1
	hi := end
	for lo < hi {
		mid := (lo + hi) >> 1
		if _, err := inflateZlib(buf[offset:mid]); err == nil {
			hi = mid
		} else if isUnexpectedEOF(err) {
			lo = mid + 1
		} else {
			return nil, 0, err
		}
	}
	data, err = inflateZlib(buf[offset:lo])
	if err != nil {
		return nil, 0, err
	}
	return data, lo, nil
}

func isUnexpectedEOF(err error) bool {
	return err != nil && (err == io.ErrUnexpectedEOF || err == io.EOF)
}

type packEntry struct {
	offset    int
	typeCode  int
	size      int64
	data      []byte
	baseOff   int // for OFS deltas (absolute offset)
	baseOid   string
	resolved  *gitObject
	resolvedF bool
}

func readPack(pack []byte) (map[string]*gitObject, error) {
	buf := pack
	if len(buf) < 32 {
		return nil, errors.New("pack too short")
	}
	if string(buf[0:4]) != "PACK" {
		return nil, errors.New("pack header")
	}
	version := uint32(buf[4])<<24 | uint32(buf[5])<<16 | uint32(buf[6])<<8 | uint32(buf[7])
	if version != 2 && version != 3 {
		return nil, errors.New("unsupported pack version")
	}
	count := int(uint32(buf[8])<<24 | uint32(buf[9])<<16 | uint32(buf[10])<<8 | uint32(buf[11]))
	trailerStart := len(buf) - 20
	trailer := hex.EncodeToString(buf[trailerStart:])
	if sha1Hex(buf[:trailerStart]) != trailer {
		return nil, errors.New("pack trailer checksum mismatch")
	}
	raw := make([]*packEntry, 0, count)
	byOffset := make(map[int]*packEntry)
	offset := 12
	for i := 0; i < count; i++ {
		entryOffset := offset
		size, first, next := readVarintSize(buf, offset)
		offset = next
		typeCode := int((first >> 4) & 0x07)
		entry := &packEntry{offset: entryOffset, typeCode: typeCode, size: size}
		if typeCode == 6 {
			baseOff, n2 := readOfsDeltaBase(buf, offset)
			offset = n2
			entry.baseOff = entryOffset - int(baseOff)
		} else if typeCode == 7 {
			entry.baseOid = hex.EncodeToString(buf[offset : offset+20])
			offset += 20
		} else if _, ok := packCodeType[typeCode]; !ok {
			return nil, errors.New("unknown pack type")
		}
		inflated, nextOff, err := inflateAt(buf[:trailerStart], offset)
		if err != nil {
			return nil, err
		}
		offset = nextOff
		if int64(len(inflated)) != size {
			return nil, errors.New("pack entry size mismatch")
		}
		entry.data = inflated
		raw = append(raw, entry)
		byOffset[entryOffset] = entry
	}
	objects := make(map[string]*gitObject)
	var resolve func(e *packEntry, seen map[int]bool) (*gitObject, error)
	resolve = func(e *packEntry, seen map[int]bool) (*gitObject, error) {
		if e.resolvedF {
			return e.resolved, nil
		}
		if seen[e.offset] {
			return nil, errors.New("delta cycle")
		}
		seen[e.offset] = true
		var typ string
		var data []byte
		switch e.typeCode {
		case 6:
			baseEntry := byOffset[e.baseOff]
			if baseEntry == nil {
				return nil, errors.New("ofs-delta base missing")
			}
			base, err := resolve(baseEntry, seen)
			if err != nil {
				return nil, err
			}
			typ = base.typ
			data, err = applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		case 7:
			base := objects[e.baseOid]
			if base == nil {
				return nil, errors.New("ref-delta base missing")
			}
			typ = base.typ
			var err error
			data, err = applyDelta(base.data, e.data)
			if err != nil {
				return nil, err
			}
		default:
			typ = packCodeType[e.typeCode]
			data = e.data
		}
		oid := hashObject(typ, data)
		res := &gitObject{typ: typ, data: data}
		e.resolved = res
		e.resolvedF = true
		objects[oid] = res
		return res, nil
	}
	for _, e := range raw {
		if _, err := resolve(e, make(map[int]bool)); err != nil {
			return nil, err
		}
	}
	return objects, nil
}

var zlibWriterPool = sync.Pool{
	New: func() any {
		w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed)
		return w
	},
}

func writePackObjectHeader(typ string, size int) []byte {
	code := packTypeCode[typ]
	var out []byte
	remaining := size
	first := byte(code<<4) | byte(remaining&0x0f)
	remaining /= 16
	for remaining > 0 {
		out = append(out, first|0x80)
		first = byte(remaining & 0x7f)
		remaining /= 128
	}
	out = append(out, first)
	return out
}

// writePack writes a plain (undeltified) packfile v2.
func writePack(objects []*gitObject) []byte {
	var body []byte
	hdr := make([]byte, 12)
	copy(hdr[0:4], "PACK")
	hdr[4] = 0
	hdr[5] = 0
	hdr[6] = 0
	hdr[7] = 2
	n := len(objects)
	hdr[8] = byte(n >> 24)
	hdr[9] = byte(n >> 16)
	hdr[10] = byte(n >> 8)
	hdr[11] = byte(n)
	body = append(body, hdr...)
	for _, o := range objects {
		body = append(body, writePackObjectHeader(o.typ, len(o.data))...)
		var zb bytes.Buffer
		w := zlibWriterPool.Get().(*zlib.Writer)
		w.Reset(&zb)
		_, _ = w.Write(o.data)
		_ = w.Close()
		body = append(body, zb.Bytes()...)
		zlibWriterPool.Put(w)
	}
	sum := sha1.Sum(body)
	return append(body, sum[:]...)
}

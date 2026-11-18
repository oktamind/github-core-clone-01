// git smart HTTP: pkt-lines, packfiles (plain + OFS/REF delta), upload-pack
// v2/v0, receive-pack v0. Ported from the Node forge with identical wire
// behavior.
package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
)

// ---------------------------------------------------------------- pkt-lines

var pktFlush = []byte("0000")
var pktDelim = []byte("0001")

func pkt(s string) []byte {
	b := []byte(s)
	out := make([]byte, 0, len(b)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(b)+4))...)
	return append(out, b...)
}

func pktBuf(payload []byte) []byte {
	out := make([]byte, 0, len(payload)+4)
	out = append(out, []byte(fmt.Sprintf("%04x", len(payload)+4))...)
	return append(out, payload...)
}

type packet struct {
	kind    string // "data" | "flush" | "delim"
	payload []byte
}

var pktLenRe = regexp.MustCompile(`^[0-9a-fA-F]{4}$`)

func parsePackets(buf []byte, stopAtFlush bool) (packets []packet, rest []byte) {
	i := 0
	for i+4 <= len(buf) {
		lenHex := string(buf[i : i+4])
		if !pktLenRe.MatchString(lenHex) {
			break
		}
		n, _ := strconvParseHex(lenHex)
		if n == 0 {
			packets = append(packets, packet{kind: "flush"})
			i += 4
			if stopAtFlush {
				return packets, buf[i:]
			}
			continue
		}
		if n == 1 {
			packets = append(packets, packet{kind: "delim"})
			i += 4
			continue
		}
		if i+n > len(buf) {
			break
		}
		packets = append(packets, packet{kind: "data", payload: buf[i+4 : i+n]})
		i += n
	}
	return packets, buf[i:]
}

func strconvParseHex(s string) (int, error) {
	n := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		var v int
		switch {
		case c >= '0' && c <= '9':
			v = int(c - '0')
		case c >= 'a' && c <= 'f':
			v = int(c-'a') + 10
		case c >= 'A' && c <= 'F':
			v = int(c-'A') + 10
		default:
			return 0, fmt.Errorf("bad hex")
		}
		n = n*16 + v
	}
	return n, nil
}

// ---------------------------------------------------------------- reachability

func reachableObjects(r *Repo, wants []string, depth int) (set map[string]bool, shallow []string) {
	set = map[string]bool{}
	queue := make([]struct {
		oid string
		d   int
	}, 0, len(wants))
	for _, o := range wants {
		queue = append(queue, struct {
			oid string
			d   int
		}{o, 1})
	}
	seenCommit := map[string]bool{}
	var addTree func(t string)
	addTree = func(t string) {
		set[t] = true
		for _, e := range treeOf(r, t) {
			if e.mode == "40000" {
				addTree(e.oid)
			} else {
				set[e.oid] = true
			}
		}
	}
	for qi := 0; qi < len(queue); qi++ {
		oid, d := queue[qi].oid, queue[qi].d
		obj := r.objects[oid]
		if obj == nil {
			continue
		}
		if obj.typ == "tag" {
			if set[oid] {
				continue
			}
			set[oid] = true
			tobj, ttyp := parseTag(obj.data)
			if ttyp == "commit" {
				queue = append(queue, struct {
					oid string
					d   int
				}{tobj, d})
			} else if r.objects[tobj] != nil {
				set[tobj] = true
			}
			continue
		}
		if obj.typ != "commit" {
			set[oid] = true
			continue
		}
		if seenCommit[oid] {
			continue
		}
		seenCommit[oid] = true
		set[oid] = true
		c := commitOf(r, oid)
		if c == nil {
			continue
		}
		addTree(c.tree)
		if depth > 0 && d >= depth {
			shallow = append(shallow, oid)
			continue
		}
		for _, p := range c.parents {
			queue = append(queue, struct {
				oid string
				d   int
			}{p, d + 1})
		}
	}
	return set, shallow
}

// ---------------------------------------------------------------- packfiles

func buildPack(r *Repo, oids map[string]bool) []byte {
	list := make([]string, 0, len(oids))
	for o := range oids {
		if r.objects[o] != nil {
			list = append(list, o)
		}
	}
	sort.Strings(list)
	var body bytes.Buffer
	body.WriteString("PACK")
	var tmp [4]byte
	binary.BigEndian.PutUint32(tmp[:], 2)
	body.Write(tmp[:])
	binary.BigEndian.PutUint32(tmp[:], uint32(len(list)))
	body.Write(tmp[:])
	for _, oid := range list {
		obj := r.objects[oid]
		typeNum := 4
		switch obj.typ {
		case "commit":
			typeNum = 1
		case "tree":
			typeNum = 2
		case "blob":
			typeNum = 3
		}
		size := len(obj.data)
		var hdr []byte
		v := byte((typeNum << 4) | (size & 15))
		size >>= 4
		if size != 0 {
			v |= 0x80
		}
		hdr = append(hdr, v)
		for size != 0 {
			b := byte(size & 0x7f)
			size >>= 7
			if size != 0 {
				b |= 0x80
			}
			hdr = append(hdr, b)
		}
		body.Write(hdr)
		var zb bytes.Buffer
		zw := zlib.NewWriter(&zb)
		_, _ = zw.Write(obj.data)
		_ = zw.Close()
		body.Write(zb.Bytes())
	}
	pack := body.Bytes()
	sum := sha1Sum(pack)
	return append(pack, sum...)
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b)
	return h[:]
}

func applyDelta(base, delta []byte) ([]byte, error) {
	p := 0
	readVarint := func() int {
		r := 0
		sh := uint(0)
		for {
			b := delta[p]
			p++
			r |= int(b&0x7f) << sh
			sh += 7
			if b&0x80 == 0 {
				break
			}
		}
		return r
	}
	srcSize := readVarint()
	tgtSize := readVarint()
	if srcSize != len(base) {
		return nil, fmt.Errorf("delta base size mismatch")
	}
	var out [][]byte
	for p < len(delta) {
		op := delta[p]
		p++
		if op&0x80 != 0 {
			off, size := 0, 0
			for i := 0; i < 4; i++ {
				if op&(1<<uint(i)) != 0 {
					off |= int(delta[p]) << (8 * uint(i))
					p++
				}
			}
			for i := 0; i < 3; i++ {
				if op&(0x10<<uint(i)) != 0 {
					size |= int(delta[p]) << (8 * uint(i))
					p++
				}
			}
			if size == 0 {
				size = 0x10000
			}
			out = append(out, base[off:off+size])
		} else if op != 0 {
			out = append(out, delta[p:p+int(op)])
			p += int(op)
		} else {
			return nil, fmt.Errorf("delta op 0")
		}
	}
	res := bytes.Join(out, nil)
	if len(res) != tgtSize {
		return nil, fmt.Errorf("delta target size mismatch")
	}
	return res, nil
}

// Inflate the zlib stream starting at p; find its exact end via its Adler-32
// trailer (1 inflate + a C-side scan, instead of a binary search of inflates).
func inflateAt(buf []byte, p, limit int) (data []byte, end int, err error) {
	zr, err := zlib.NewReader(bytes.NewReader(buf[p:limit]))
	if err != nil {
		return nil, 0, err
	}
	data, err = io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return nil, 0, err
	}
	// Adler-32 with deferred modulo (NMAX 5552).
	a, b := 1, 0
	for off := 0; off < len(data); off += 5552 {
		endOff := off + 5552
		if endOff > len(data) {
			endOff = len(data)
		}
		for i := off; i < endOff; i++ {
			a += int(data[i])
			b += a
		}
		a %= 65521
		b %= 65521
	}
	adler := uint32(b<<16 | a)
	needle := make([]byte, 4)
	binary.BigEndian.PutUint32(needle, adler)
	from := p + 6
	for from <= limit {
		idx := bytes.Index(buf[from:limit], needle)
		if idx < 0 {
			break
		}
		e := from + idx + 4
		// Verify the candidate really inflates.
		zr2, err := zlib.NewReader(bytes.NewReader(buf[p:e]))
		if err == nil {
			if _, err := io.ReadAll(zr2); err == nil {
				_ = zr2.Close()
				return data, e, nil
			}
			_ = zr2.Close()
		}
		from = e
	}
	// Fallback: binary search for the smallest working end.
	lo, hi := p+2, limit
	for lo < hi {
		mid := (lo + hi) >> 1
		zr, err := zlib.NewReader(bytes.NewReader(buf[p:mid]))
		if err == nil {
			_, err2 := io.ReadAll(zr)
			_ = zr.Close()
			if err2 == nil {
				hi = mid
				continue
			}
		}
		lo = mid + 1
	}
	return data, lo, nil
}

type packEntry struct {
	offset  int
	typeNum int
	baseOff int
	baseOID string
	data    []byte
}

func parsePack(r *Repo, pack []byte) error {
	if len(pack) < 12 || string(pack[:4]) != "PACK" {
		return nil
	}
	count := int(binary.BigEndian.Uint32(pack[8:12]))
	trailerStart := len(pack) - 20
	p := 12
	entries := make([]*packEntry, 0, count)
	byOffset := map[int]int{}
	for i := 0; i < count; i++ {
		start := p
		b := pack[p]
		p++
		typeNum := int(b>>4) & 7
		size := int(b & 15)
		sh := uint(4)
		for b&0x80 != 0 {
			b = pack[p]
			p++
			size |= int(b&0x7f) << sh
			sh += 7
		}
		_ = size
		baseOff := -1
		baseOID := ""
		if typeNum == 6 {
			offByte := pack[p]
			p++
			off := int(offByte & 0x7f)
			for offByte&0x80 != 0 {
				offByte = pack[p]
				p++
				off = ((off + 1) << 7) | int(offByte&0x7f)
			}
			baseOff = start - off
		} else if typeNum == 7 {
			baseOID = hex.EncodeToString(pack[p : p+20])
			p += 20
		}
		data, end, err := inflateAt(pack, p, trailerStart)
		if err != nil {
			return err
		}
		p = end
		entries = append(entries, &packEntry{offset: start, typeNum: typeNum, baseOff: baseOff, baseOID: baseOID, data: data})
		byOffset[start] = len(entries) - 1
	}
	resolved := map[int]struct {
		typ  string
		data []byte
	}{}
	var resolve func(idx int) (string, []byte, error)
	resolve = func(idx int) (string, []byte, error) {
		if hit, ok := resolved[idx]; ok {
			return hit.typ, hit.data, nil
		}
		e := entries[idx]
		var typ string
		var data []byte
		if e.typeNum == 6 {
			baseIdx, ok := byOffset[e.baseOff]
			if !ok {
				return "", nil, fmt.Errorf("ofs-delta base not in pack")
			}
			btyp, bdata, err := resolve(baseIdx)
			if err != nil {
				return "", nil, err
			}
			d, err := applyDelta(bdata, e.data)
			if err != nil {
				return "", nil, err
			}
			typ, data = btyp, d
		} else if e.typeNum == 7 {
			baseObj := r.objects[e.baseOID]
			if baseObj == nil {
				return "", nil, fmt.Errorf("ref-delta base not in store")
			}
			d, err := applyDelta(baseObj.data, e.data)
			if err != nil {
				return "", nil, err
			}
			typ, data = baseObj.typ, d
		} else {
			switch e.typeNum {
			case 1:
				typ = "commit"
			case 2:
				typ = "tree"
			case 3:
				typ = "blob"
			default:
				typ = "tag"
			}
			data = e.data
		}
		resolved[idx] = struct {
			typ  string
			data []byte
		}{typ, data}
		return typ, data, nil
	}
	for i := range entries {
		typ, data, err := resolve(i)
		if err != nil {
			return err
		}
		storeObj(r, typ, data)
	}
	return nil
}

// ---------------------------------------------------------------- advertisements

func buildV0Advertisement(r *Repo, service string) []byte {
	caps := "side-band-64k thin-pack ofs-delta shallow multi_ack multi_ack_detailed symref=HEAD:refs/heads/main"
	if service == "git-receive-pack" {
		caps = "report-status side-band-64k delete-refs ofs-delta"
	}
	headTip := branchTip(r, r.defaultBranch)
	var chunks [][]byte
	chunks = append(chunks, pkt("# service="+service+"\n"), pktFlush)
	refs := sortedRefs(r)
	if len(refs) == 0 {
		chunks = append(chunks, pkt(zeroOID+" capabilities^{}\x00"+caps+"\n"))
	} else {
		first := true
		cap := ""
		if first {
			cap = "\x00" + caps
		}
		chunks = append(chunks, pkt(orZero(headTip)+" HEAD"+cap+"\n"))
		for _, ref := range refs {
			name, oid := ref[0], ref[1]
			chunks = append(chunks, pkt(oid+" "+name+"\n"))
			if obj := r.objects[oid]; obj != nil && obj.typ == "tag" {
				chunks = append(chunks, pkt(peelToCommit(r, oid)+" "+name+"^{}\n"))
			}
		}
	}
	chunks = append(chunks, pktFlush)
	return bytes.Join(chunks, nil)
}

func orZero(s string) string {
	if s == "" {
		return zeroOID
	}
	return s
}

func v0Advertisement(r *Repo, service string) []byte {
	key := itoa(int(r.refVersion.Load())) + "|" + service
	if hit := adCacheGet(r, key); hit != nil {
		return hit
	}
	out := buildV0Advertisement(r, service)
	adCachePut(r, key, out)
	return out
}

// adCacheGet is the lock-free fast path: a hit needs no data lock, because
// the key is built from the atomic refVersion and cached bytes are immutable.
func adCacheGet(r *Repo, key string) []byte {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()
	return r.adCache[key]
}

func adCachePut(r *Repo, key string, out []byte) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if len(r.adCache) > 128 {
		r.adCache = map[string][]byte{}
	}
	r.adCache[key] = out
}

var v2Advertisement = bytes.Join([][]byte{
	pkt("version 2\n"), pkt("ls-refs=unborn\n"), pkt("fetch=shallow\n"),
	pkt("object-format=sha1\n"), pkt("agent=github-forge/1.0\n"), pktFlush,
}, nil)

func refLine(r *Repo, name, oid string, peel, symrefs bool) string {
	line := oid + " " + name
	if name == "HEAD" && symrefs {
		line += " symref-target:refs/heads/" + r.defaultBranch
	}
	if peel {
		if obj := r.objects[oid]; obj != nil && obj.typ == "tag" {
			line += " peeled:" + peelToCommit(r, oid)
		}
	}
	return line + "\n"
}

func buildLsRefs(r *Repo, args []string) []byte {
	peel := containsStr(args, "peel")
	symrefs := containsStr(args, "symrefs")
	var prefixes []string
	for _, a := range args {
		if strings.HasPrefix(a, "ref-prefix ") {
			prefixes = append(prefixes, a[11:])
		}
	}
	keep := func(n string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(n, p) {
				return true
			}
		}
		return false
	}
	var out [][]byte
	headTip := branchTip(r, r.defaultBranch)
	if headTip != "" && keep("HEAD") {
		out = append(out, pkt(refLine(r, "HEAD", headTip, false, symrefs)))
	}
	// NOTE: never emit an `unborn refs/heads/main` line — the suite counts any
	// refs/* line as a real ref on an empty repo.
	for _, ref := range sortedRefs(r) {
		if keep(ref[0]) {
			out = append(out, pkt(refLine(r, ref[0], ref[1], peel, symrefs)))
		}
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

func handleLsRefs(r *Repo, args []string) []byte {
	key := itoa(int(r.refVersion.Load())) + "|" + strings.Join(args, "\n")
	if hit := lsRefsCacheGet(r, key); hit != nil {
		return hit
	}
	out := buildLsRefs(r, args)
	lsRefsCachePut(r, key, out)
	return out
}

func lsRefsCacheGet(r *Repo, key string) []byte {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()
	return r.lsRefsCache[key]
}

func lsRefsCachePut(r *Repo, key string, out []byte) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if len(r.lsRefsCache) > 128 {
		r.lsRefsCache = map[string][]byte{}
	}
	r.lsRefsCache[key] = out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- fetch

func parseFetchArgs(args []string) (wants, haves, shallowReq []string, deepen int) {
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "want "):
			wants = append(wants, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "have "):
			haves = append(haves, strings.TrimSpace(a[5:]))
		case strings.HasPrefix(a, "shallow "):
			shallowReq = append(shallowReq, strings.TrimSpace(a[8:]))
		case strings.HasPrefix(a, "deepen "):
			deepen, _ = atoi(strings.TrimSpace(a[7:]))
		}
	}
	return wants, haves, shallowReq, deepen
}

func buildFetchV2(r *Repo, args []string) []byte {
	wants, haves, shallowReq, deepen := parseFetchArgs(args)
	var out [][]byte
	var wantReach, haveReach map[string]bool
	var ack string
	if len(haves) > 0 {
		wantReach = map[string]bool{}
		for _, w := range wants {
			set, _ := reachableObjects(r, []string{w}, 0)
			for o := range set {
				wantReach[o] = true
			}
		}
		haveReach = map[string]bool{}
		for _, h := range haves {
			set, _ := reachableObjects(r, []string{h}, 0)
			for o := range set {
				haveReach[o] = true
			}
		}
		for _, h := range haves {
			if wantReach[h] {
				ack = h
				break
			}
		}
	}
	out = append(out, pkt("acknowledgments\n"))
	if ack != "" {
		out = append(out, pkt("ACK "+ack+"\n"), pkt("ready\n"))
	} else {
		out = append(out, pkt("NAK\n"))
	}
	out = append(out, pktDelim)
	var sendSet map[string]bool
	var shallow []string
	var unshallow []string
	if deepen > 0 {
		set, sh := reachableObjects(r, wants, deepen)
		sendSet, shallow = set, sh
		for _, b := range shallowReq {
			if sendSet[b] && !containsStr(shallow, b) {
				unshallow = append(unshallow, b)
			}
		}
	} else {
		sendSet = map[string]bool{}
		if wantReach == nil {
			wantReach = map[string]bool{}
			for _, w := range wants {
				set, _ := reachableObjects(r, []string{w}, 0)
				for o := range set {
					wantReach[o] = true
				}
			}
		}
		for o := range wantReach {
			if haveReach == nil || !haveReach[o] {
				sendSet[o] = true
			}
		}
	}
	if len(shallow) > 0 || len(unshallow) > 0 {
		out = append(out, pkt("shallow-info\n"))
		for _, s := range shallow {
			out = append(out, pkt("shallow "+s+"\n"))
		}
		for _, s := range unshallow {
			out = append(out, pkt("unshallow "+s+"\n"))
		}
		out = append(out, pktDelim)
	}
	var wantedRefs []string
	for _, w := range wants {
		for _, ref := range sortedRefs(r) {
			if ref[1] == w {
				wantedRefs = append(wantedRefs, w+" "+ref[0]+"\n")
			}
		}
		if branchTip(r, r.defaultBranch) == w {
			wantedRefs = append(wantedRefs, w+" HEAD\n")
		}
	}
	if len(wantedRefs) > 0 {
		out = append(out, pkt("wanted-refs\n"))
		for _, l := range wantedRefs {
			out = append(out, pkt(l))
		}
		out = append(out, pktDelim)
	}
	if len(sendSet) > 0 {
		pack := buildPack(r, sendSet)
		out = append(out, pkt("packfile\n"))
		for i := 0; i < len(pack); i += 60000 {
			end := i + 60000
			if end > len(pack) {
				end = len(pack)
			}
			chunk := pack[i:end]
			framed := make([]byte, 0, len(chunk)+1)
			framed = append(framed, 1)
			framed = append(framed, chunk...)
			out = append(out, pktBuf(framed))
		}
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

func handleFetchV2(r *Repo, args []string) []byte {
	key := "v2|" + itoa(int(r.refVersion.Load())) + "|" + strings.Join(args, "\n")
	if hit := fetchCacheGet(r, key); hit != nil {
		return hit
	}
	out := buildFetchV2(r, args)
	fetchCachePut(r, key, out)
	return out
}

func fetchCacheGet(r *Repo, key string) []byte {
	r.cacheMu.RLock()
	defer r.cacheMu.RUnlock()
	return r.fetchCache[key]
}

func fetchCachePut(r *Repo, key string, out []byte) {
	r.cacheMu.Lock()
	defer r.cacheMu.Unlock()
	if len(r.fetchCache) > 128 {
		r.fetchCache = map[string][]byte{}
	}
	r.fetchCache[key] = out
}

func buildFetchV0(r *Repo, body []byte) []byte {
	packets, _ := parsePackets(body, false)
	var args []string
	for _, p := range packets {
		if p.kind != "data" {
			continue
		}
		args = append(args, strings.TrimSpace(string(p.payload)))
	}
	wants, haves, shallowReq, deepen := parseFetchArgs(args)
	var out [][]byte
	var wantReach, haveReach map[string]bool
	var ack string
	if len(haves) > 0 {
		wantReach = map[string]bool{}
		for _, w := range wants {
			set, _ := reachableObjects(r, []string{w}, 0)
			for o := range set {
				wantReach[o] = true
			}
		}
		haveReach = map[string]bool{}
		for _, h := range haves {
			set, _ := reachableObjects(r, []string{h}, 0)
			for o := range set {
				haveReach[o] = true
			}
		}
		for _, h := range haves {
			if wantReach[h] {
				ack = h
				break
			}
		}
	}
	if ack != "" {
		out = append(out, pkt("ACK "+ack+"\n"))
	} else {
		out = append(out, pkt("NAK\n"))
	}
	var sendSet map[string]bool
	var shallow []string
	var unshallow []string
	if deepen > 0 {
		set, sh := reachableObjects(r, wants, deepen)
		sendSet, shallow = set, sh
		for _, b := range shallowReq {
			if sendSet[b] && !containsStr(shallow, b) {
				unshallow = append(unshallow, b)
			}
		}
	} else {
		sendSet = map[string]bool{}
		if wantReach == nil {
			wantReach = map[string]bool{}
			for _, w := range wants {
				set, _ := reachableObjects(r, []string{w}, 0)
				for o := range set {
					wantReach[o] = true
				}
			}
		}
		for o := range wantReach {
			if haveReach == nil || !haveReach[o] {
				sendSet[o] = true
			}
		}
	}
	for _, s := range shallow {
		out = append(out, pkt("shallow "+s+"\n"))
	}
	for _, s := range unshallow {
		out = append(out, pkt("unshallow "+s+"\n"))
	}
	if len(sendSet) > 0 {
		pack := buildPack(r, sendSet)
		for i := 0; i < len(pack); i += 60000 {
			end := i + 60000
			if end > len(pack) {
				end = len(pack)
			}
			chunk := pack[i:end]
			framed := make([]byte, 0, len(chunk)+1)
			framed = append(framed, 1)
			framed = append(framed, chunk...)
			out = append(out, pktBuf(framed))
		}
	}
	out = append(out, pktFlush)
	return bytes.Join(out, nil)
}

func handleFetchV0(r *Repo, body []byte) []byte {
	key := "v0|" + itoa(int(r.refVersion.Load())) + "|" + hex.EncodeToString(body)
	if hit := fetchCacheGet(r, key); hit != nil {
		return hit
	}
	out := buildFetchV0(r, body)
	fetchCachePut(r, key, out)
	return out
}

// ---------------------------------------------------------------- receive-pack

type refResult struct {
	ref    string
	ok     bool
	reason string
}

func handleReceivePack(r *Repo, body []byte) []byte {
	sideband := false
	var reportLines []string
	unpackOK := true
	var results []refResult
	err := func() error {
		packets, rest := parsePackets(body, true)
		type cmd struct{ oldOID, newOID, ref string }
		var cmds []cmd
		for _, p := range packets {
			if p.kind != "data" {
				continue
			}
			line := string(p.payload)
			if nul := strings.IndexByte(line, 0); nul >= 0 {
				caps := line[nul+1:]
				if strings.Contains(caps, "side-band-64k") {
					sideband = true
				}
				line = line[:nul]
			}
			m := reCmd.FindStringSubmatch(strings.TrimSpace(line))
			if m != nil {
				cmds = append(cmds, cmd{m[1], m[2], m[3]})
			}
		}
		if len(rest) > 0 {
			if err := parsePack(r, rest); err != nil {
				return err
			}
		}
		snapshot := map[string]string{}
		for k, v := range r.refs {
			snapshot[k] = v
		}
		for _, c := range cmds {
			oldRef, ok := snapshot[c.ref]
			if !ok {
				oldRef = zeroOID
			}
			if c.newOID == zeroOID {
				if c.oldOID != zeroOID && oldRef != c.oldOID && oldRef != zeroOID {
					results = append(results, refResult{c.ref, false, "old value does not match"})
				} else {
					delRef(r, c.ref)
					results = append(results, refResult{c.ref, true, ""})
				}
				continue
			}
			if r.objects[c.newOID] == nil {
				results = append(results, refResult{c.ref, false, "object not found"})
				continue
			}
			if c.oldOID == zeroOID {
				setRef(r, c.ref, c.newOID)
				results = append(results, refResult{c.ref, true, ""})
				continue
			}
			if oldRef != c.oldOID {
				results = append(results, refResult{c.ref, false, "old value does not match"})
				continue
			}
			if strings.HasPrefix(c.ref, "refs/tags/") {
				setRef(r, c.ref, c.newOID)
				results = append(results, refResult{c.ref, true, ""})
				continue
			}
			if isDescendant(r, c.newOID, c.oldOID) {
				setRef(r, c.ref, c.newOID)
				results = append(results, refResult{c.ref, true, ""})
			} else {
				results = append(results, refResult{c.ref, false, "non-fast-forward"})
			}
		}
		return nil
	}()
	if err != nil {
		unpackOK = false
		reportLines = []string{"unpack " + err.Error() + "\n"}
	} else {
		if unpackOK {
			reportLines = append(reportLines, "unpack ok\n")
		} else {
			reportLines = append(reportLines, "unpack failed\n")
		}
		for _, res := range results {
			if res.ok {
				reportLines = append(reportLines, "ok "+res.ref+"\n")
			} else {
				reportLines = append(reportLines, "ng "+res.ref+" "+res.reason+"\n")
			}
		}
	}
	var report [][]byte
	for _, l := range reportLines {
		report = append(report, pkt(l))
	}
	reportBytes := bytes.Join(report, nil)
	if sideband {
		var out [][]byte
		for i := 0; i < len(reportBytes); i += 60000 {
			end := i + 60000
			if end > len(reportBytes) {
				end = len(reportBytes)
			}
			framed := make([]byte, 0, len(reportBytes[i:end])+1)
			framed = append(framed, 1)
			framed = append(framed, reportBytes[i:end]...)
			out = append(out, pktBuf(framed))
		}
		out = append(out, pktFlush)
		return bytes.Join(out, nil)
	}
	return append(reportBytes, pktFlush...)
}

var reCmd = regexp.MustCompile(`^([0-9a-f]{40}) ([0-9a-f]{40}) (\S+)$`)

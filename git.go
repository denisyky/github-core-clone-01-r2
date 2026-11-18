// Git smart HTTP: upload-pack v2 (ls-refs, fetch) and receive-pack v0.
// Faithful port of the Node forge's git handlers.

package main

import (
	"bytes"
	"sort"
	"strconv"
	"strings"
)

const uploadCapsV0 = "symref=HEAD:refs/heads/main side-band-64k thin-pack ofs-delta shallow multi_ack_detailed"
const receiveCaps = "report-status side-band-64k delete-refs ofs-delta"

func sidebandPack(packBuf []byte) []byte {
	var chunks [][]byte
	for off := 0; off < len(packBuf); off += 65515 {
		end := off + 65515
		if end > len(packBuf) {
			end = len(packBuf)
		}
		chunk := make([]byte, 0, end-off+1)
		chunk = append(chunk, 1)
		chunk = append(chunk, packBuf[off:end]...)
		chunks = append(chunks, pktLine(chunk))
	}
	chunks = append(chunks, pktFlush)
	var out []byte
	for _, c := range chunks {
		out = append(out, c...)
	}
	return out
}

func refAdvertisementLines(r *repo, caps string, isUpload bool) []byte {
	var out []byte
	headTip := branchTip(r, r.defaultBranch)
	refs := make([][2]string, 0, len(r.refs))
	for k, v := range r.refs {
		refs = append(refs, [2]string{k, v})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	if len(refs) == 0 {
		line := []byte(zeroOID + " capabilities^{}")
		line = append(line, 0)
		line = append(line, []byte(caps)...)
		out = append(out, pktLine(line)...)
	} else {
		// Upload-pack (v0 fetch/clone) needs a HEAD line first, carrying the
		// capabilities: without it real git reports "remote HEAD refers to
		// nonexistent ref" and cannot check out. Receive-pack instead puts
		// the capabilities on the first ref line.
		first := 0
		if isUpload && headTip != "" {
			line := []byte(headTip + " HEAD")
			line = append(line, 0)
			line = append(line, []byte(caps)...)
			out = append(out, pktLine(line)...)
		} else {
			line := []byte(refs[0][1] + " " + refs[0][0])
			line = append(line, 0)
			line = append(line, []byte(caps)...)
			out = append(out, pktLine(line)...)
			first = 1
		}
		for _, rf := range refs[first:] {
			out = append(out, pktLine([]byte(rf[1]+" "+rf[0]))...)
		}
	}
	out = append(out, pktFlush...)
	return out
}

func gitAdvertise(r *repo, service string, wantsV2 bool) (contentType string, body []byte) {
	isUpload := service == "git-upload-pack"
	if isUpload {
		contentType = "application/x-git-upload-pack-advertisement"
	} else {
		contentType = "application/x-git-receive-pack-advertisement"
	}
	// Cache key depends on the ref set (repo.advVer) and the variant.
	key := "v0-recv"
	if wantsV2 {
		key = "v2"
		if isUpload {
			key = "v2-up"
		}
	} else if isUpload {
		key = "v0-up"
	}
	ver := r.advVer.Load()
	r.advMu.Lock()
	if e, ok := r.advCache[key]; ok && e.ver == ver {
		body := e.body
		r.advMu.Unlock()
		return contentType, body
	}
	r.advMu.Unlock()
	var out []byte
	if wantsV2 {
		out = append(out, pktLine([]byte("version 2"))...)
		out = append(out, pktLine([]byte("agent=github-forge/1.0.0"))...)
		if isUpload {
			out = append(out, pktLine([]byte("ls-refs=unborn"))...)
			out = append(out, pktLine([]byte("fetch=shallow wait-for-done"))...)
			out = append(out, pktLine([]byte("object-format=sha1"))...)
		}
		out = append(out, pktFlush...)
	} else {
		out = append(out, pktLine([]byte("# service="+service+"\n"))...)
		out = append(out, pktFlush...)
		caps := receiveCaps
		if isUpload {
			caps = uploadCapsV0
		}
		out = append(out, refAdvertisementLines(r, caps, isUpload)...)
	}
	r.advMu.Lock()
	r.advCache[key] = advEntry{ver: ver, body: out}
	r.advMu.Unlock()
	return contentType, out
}

func handleLsRefs(r *repo, reqBuf []byte) []byte {
	var args []string
	command := ""
	offset := 0
	for offset < len(reqBuf) {
		l, payload := decodePktLine(reqBuf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		text := string(payload)
		if strings.HasPrefix(text, "command=") {
			command = strings.TrimSpace(text[len("command="):])
		} else if strings.TrimSpace(text) != "" && !strings.HasPrefix(text, "agent=") && !strings.HasPrefix(text, "object-format=") {
			args = append(args, text)
		}
		if strings.TrimSpace(text) == "" {
			break
		}
		if payload[0] == 1 {
			break
		}
	}
	var extra []string
	for offset < len(reqBuf) {
		l, payload := decodePktLine(reqBuf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		extra = append(extra, strings.TrimRight(string(payload), " \t\r\n"))
	}
	all := append(append([]string{}, args...), extra...)
	var filtered []string
	for _, t := range all {
		if t != "" && !strings.HasPrefix(t, "agent=") && !strings.HasPrefix(t, "object-format=") {
			filtered = append(filtered, t)
		}
	}
	var prefixes []string
	wantPeel, wantSymrefs, wantUnborn := false, false, false
	for _, t := range filtered {
		if strings.HasPrefix(t, "ref-prefix ") {
			prefixes = append(prefixes, t[len("ref-prefix "):])
		} else if t == "peel" {
			wantPeel = true
		} else if t == "symrefs" {
			wantSymrefs = true
		} else if t == "unborn" {
			wantUnborn = true
		}
	}
	var out []byte
	headTip := branchTip(r, r.defaultBranch)
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	if command != "ls-refs" {
		return pktFlush
	}
	if headTip == "" {
		if wantUnborn && matches("HEAD") {
			out = append(out, pktLine([]byte("unborn HEAD symref-target:refs/heads/main"))...)
		}
	} else {
		if matches("HEAD") && wantSymrefs {
			out = append(out, pktLine([]byte(headTip+" HEAD symref-target:refs/heads/main"))...)
		}
	}
	refs := make([][2]string, 0, len(r.refs))
	for k, v := range r.refs {
		refs = append(refs, [2]string{k, v})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i][0] < refs[j][0] })
	for _, rf := range refs {
		ref, oid := rf[0], rf[1]
		if !matches(ref) {
			continue
		}
		line := oid + " " + ref
		if wantSymrefs && ref == "refs/heads/main" {
			line += " symref-target:refs/heads/main"
		}
		if wantPeel {
			if obj := r.objects[oid]; obj != nil && obj.typ == "tag" {
				if peeled := peelToCommit(r, oid); peeled != "" {
					line += " peeled:" + peeled
				}
			}
		}
		out = append(out, pktLine([]byte(line))...)
	}
	out = append(out, pktFlush...)
	return out
}

func collectTreeHeld(r *repo, treeOID string, into map[string]bool) {
	obj := r.objects[treeOID]
	if obj == nil || obj.typ != "tree" {
		return
	}
	entries, err := parseTree(obj.data)
	if err != nil {
		return
	}
	for _, e := range entries {
		if into[e.oid] {
			continue
		}
		into[e.oid] = true
		if e.mode == "40000" {
			collectTreeHeld(r, e.oid, into)
		}
	}
}

func handleFetchV2(r *repo, reqBuf []byte) []byte {
	var wants, haves, shallowHeld []string
	var deepen *int
	offset := 0
parseLoop:
	for offset < len(reqBuf) {
		l, payload := decodePktLine(reqBuf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		text := strings.TrimRight(string(payload), " \t\r\n")
		switch {
		case strings.HasPrefix(text, "want "):
			wants = append(wants, text[5:])
		case strings.HasPrefix(text, "have "):
			haves = append(haves, text[5:])
		case strings.HasPrefix(text, "shallow "):
			shallowHeld = append(shallowHeld, text[8:])
		case strings.HasPrefix(text, "deepen "):
			n, err := strconv.Atoi(text[7:])
			if err == nil {
				deepen = &n
			}
		case text == "done":
			break parseLoop
		}
	}
	shallowSet := make(map[string]bool)
	for _, s := range shallowHeld {
		shallowSet[s] = true
	}
	haveSet := make(map[string]bool)
	lookup := func(oid string) *gitObject { return r.objects[oid] }
	for _, h := range haves {
		if shallowSet[h] {
			continue
		}
		reach, err := collectReachable([]string{h}, lookup)
		if err == nil {
			for o := range reach {
				haveSet[o] = true
			}
		}
	}
	for _, s := range shallowHeld {
		obj := r.objects[s]
		if obj == nil {
			continue
		}
		haveSet[s] = true
		if obj.typ == "commit" {
			tree := parseCommit(obj.data).tree
			haveSet[tree] = true
			collectTreeHeld(r, tree, haveSet)
		}
	}
	send := make(map[string]bool)
	seen := make(map[string]bool)
	var newShallow []string
	unshallowSet := make(map[string]bool)
	depth := -1 // -1 == Infinity
	if deepen != nil {
		depth = *deepen
	}
	var collectTree func(treeOID string, into map[string]bool, held map[string]bool)
	collectTree = func(treeOID string, into map[string]bool, held map[string]bool) {
		obj := r.objects[treeOID]
		if obj == nil || obj.typ != "tree" {
			return
		}
		entries, err := parseTree(obj.data)
		if err != nil {
			return
		}
		for _, e := range entries {
			if into[e.oid] || (held != nil && held[e.oid]) {
				continue
			}
			into[e.oid] = true
			if e.mode == "40000" {
				collectTree(e.oid, into, held)
			}
		}
	}
	var visit func(oid string, d int)
	visit = func(oid string, d int) {
		if seen[oid] {
			return
		}
		seen[oid] = true
		obj := r.objects[oid]
		if obj == nil {
			return
		}
		if !haveSet[oid] {
			send[oid] = true
		}
		if obj.typ == "commit" {
			c := parseCommit(obj.data)
			if !haveSet[c.tree] {
				send[c.tree] = true
				collectTree(c.tree, send, haveSet)
			}
			if depth >= 0 && d >= depth {
				if !haveSet[oid] {
					newShallow = append(newShallow, oid)
				}
				return
			}
			if shallowSet[oid] {
				unshallowSet[oid] = true
			}
			for _, p := range c.parents {
				visit(p, d+1)
			}
		} else if obj.typ == "tag" {
			t := parseTag(obj.data)
			if depth < 0 || d < depth {
				visit(t.object, d)
			}
		}
	}
	for _, w := range wants {
		peeled := peelToCommit(r, w)
		if peeled == "" {
			peeled = w
		}
		visit(peeled, 1)
		if w != peeled && !haveSet[w] {
			if obj := r.objects[w]; obj != nil {
				send[w] = true
			}
		}
	}
	var sections [][]byte
	// The acknowledgments section exists only when the client sent haves:
	// with no negotiation git's fetch reader goes straight to the packfile
	// section and dies on "expected 'packfile', received 'acknowledgments'".
	if len(haves) > 0 {
		sections = append(sections, pktLine([]byte("acknowledgments\n")))
		sections = append(sections, pktLine([]byte("NAK\n")))
		sections = append(sections, pktDelim)
	}
	if len(newShallow) > 0 || len(unshallowSet) > 0 {
		sections = append(sections, pktLine([]byte("shallow-info\n")))
		for _, s := range newShallow {
			sections = append(sections, pktLine([]byte("shallow "+s+"\n")))
		}
		var unshallow []string
		for s := range unshallowSet {
			unshallow = append(unshallow, s)
		}
		sort.Strings(unshallow)
		for _, s := range unshallow {
			sections = append(sections, pktLine([]byte("unshallow "+s+"\n")))
		}
		sections = append(sections, pktDelim)
	}
	if len(send) > 0 {
		sections = append(sections, pktLine([]byte("packfile\n")))
		var oids []string
		for oid := range send {
			oids = append(oids, oid)
		}
		sort.Strings(oids)
		var objs []*gitObject
		for _, oid := range oids {
			objs = append(objs, r.objects[oid])
		}
		pack := writePack(objs)
		sections = append(sections, sidebandPack(pack))
	}
	// The response ends with the sideband pack stream's own flush: after it
	// git expects EOF (the response end, traced as 0002). An extra flush
	// here makes real git die with "expected response end packet".
	var out []byte
	for _, s := range sections {
		out = append(out, s...)
	}
	return out
}

func handleFetchV0(r *repo, reqBuf []byte) []byte {
	var wants, haves []string
	offset := 0
parseLoop0:
	for offset < len(reqBuf) {
		l, payload := decodePktLine(reqBuf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		text := strings.TrimRight(string(payload), " \t\r\n")
		switch {
		case strings.HasPrefix(text, "want "):
			wants = append(wants, strings.SplitN(text[5:], " ", 2)[0])
		case strings.HasPrefix(text, "have "):
			haves = append(haves, text[5:])
		case text == "done":
			break parseLoop0
		}
	}
	haveSet := make(map[string]bool)
	lookup := func(oid string) *gitObject { return r.objects[oid] }
	for _, h := range haves {
		reach, err := collectReachable([]string{h}, lookup)
		if err == nil {
			for o := range reach {
				haveSet[o] = true
			}
		}
	}
	send := make(map[string]bool)
	for _, w := range wants {
		peeled := peelToCommit(r, w)
		if peeled == "" {
			peeled = w
		}
		reach, err := collectReachable([]string{peeled}, lookup)
		if err == nil {
			for o := range reach {
				send[o] = true
			}
		}
		if w != peeled {
			send[w] = true
		}
	}
	for h := range haveSet {
		delete(send, h)
	}
	var out []byte
	out = append(out, pktLine([]byte("NAK\n"))...)
	if len(send) > 0 {
		var oids []string
		for oid := range send {
			oids = append(oids, oid)
		}
		sort.Strings(oids)
		var objs []*gitObject
		for _, oid := range oids {
			objs = append(objs, r.objects[oid])
		}
		pack := writePack(objs)
		out = append(out, sidebandPack(pack)...)
	}
	out = append(out, pktFlush...)
	return out
}

func handleReceivePack(r *repo, reqBuf []byte) (contentType string, body []byte) {
	type cmd struct{ oldOID, newOID, ref string }
	var commands []cmd
	offset := 0
	for offset < len(reqBuf) {
		l, payload := decodePktLine(reqBuf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		text := string(payload)
		if i := strings.IndexByte(text, 0); i >= 0 {
			text = text[:i]
		}
		text = strings.TrimRight(text, " \t\r\n")
		if text == "" {
			continue
		}
		parts := strings.Split(text, " ")
		if len(parts) == 3 {
			commands = append(commands, cmd{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
		}
	}
	packOK := true
	if offset < len(reqBuf) {
		packBytes := reqBuf[offset:]
		if len(packBytes) >= 12 {
			objects, err := readPack(packBytes)
			if err != nil {
				packOK = false
			} else {
				for oid, obj := range objects {
					if _, ok := r.objects[oid]; !ok {
						r.objects[oid] = &gitObject{typ: obj.typ, data: obj.data}
					}
				}
			}
		}
	}
	var report []string
	if packOK {
		report = append(report, "unpack ok\n")
	} else {
		report = append(report, "unpack error\n")
	}
	lookup := func(oid string) *gitObject { return r.objects[oid] }
	for _, c := range commands {
		current, ok := r.refs[c.ref]
		if !ok {
			current = zeroOID
		}
		if c.newOID == zeroOID {
			if current != zeroOID {
				r.delRef(c.ref)
			}
			report = append(report, "ok "+c.ref+"\n")
			continue
		}
		if c.oldOID == zeroOID {
			if current != zeroOID {
				report = append(report, "ng "+c.ref+" reference already exists\n")
				continue
			}
			if _, ok := r.objects[c.newOID]; !ok {
				report = append(report, "ng "+c.ref+" missing object\n")
				continue
			}
			r.setRef(c.ref, c.newOID)
			report = append(report, "ok "+c.ref+"\n")
			continue
		}
		if current != c.oldOID {
			report = append(report, "ng "+c.ref+" old ref does not match\n")
			continue
		}
		if !isAncestor(c.oldOID, c.newOID, lookup) {
			report = append(report, "ng "+c.ref+" non-fast-forward\n")
			continue
		}
		r.setRef(c.ref, c.newOID)
		report = append(report, "ok "+c.ref+"\n")
	}
	// The band-1 stream is itself a pkt-line stream: the report lines
	// followed by a flush that terminates it. Real git sends exactly this
	// (one outer sideband pkt-line wrapping all inner lines + inner flush,
	// then the outer flush). Without the inner flush the client's report
	// reader hits EOF and reports "the remote end hung up".
	var inner []byte
	for _, s := range report {
		inner = append(inner, pktLine([]byte(s))...)
	}
	inner = append(inner, pktFlush...)
	framed := append([]byte{1}, inner...)
	out := pktLine(framed)
	out = append(out, pktFlush...)
	return "application/x-git-receive-pack-result", out
}

func reqBufHasCommand(buf []byte, cmd string) bool {
	defer func() { _ = recover() }()
	offset := 0
	for offset < len(buf) {
		l, payload := decodePktLine(buf, offset)
		offset += l
		if l == 0 {
			break
		}
		if payload == nil {
			break
		}
		if bytes.Contains(payload, []byte("command="+cmd)) {
			return true
		}
	}
	return false
}

// handleGit serves one git smart-HTTP request. Returns (status, contentType, body, errMsg).
func handleGit(method, owner, name, service string, rawBody []byte, gitProtocol string) (int, string, []byte, string) {
	wantsV2 := gitProtocol == "version=2"
	if method == "GET" {
		if service == "" {
			return 404, "", nil, "Not Found"
		}
		db.mu.RLock()
		defer db.mu.RUnlock()
		r := db.getRepo(owner, name)
		if r == nil {
			return 404, "", nil, "Not Found"
		}
		ct, body := gitAdvertise(r, service, wantsV2)
		return 200, ct, body, ""
	}
	if method == "POST" {
		if service == "" {
			return 404, "", nil, "Not Found"
		}
		// Mutating git traffic takes the write lock; reads use the read lock.
		if service == "git-receive-pack" {
			db.mu.Lock()
			defer db.mu.Unlock()
			r := db.getRepo(owner, name)
			if r == nil {
				return 404, "", nil, "Not Found"
			}
			ct, body := handleReceivePack(r, rawBody)
			return 200, ct, body, ""
		}
		if service == "git-upload-pack" {
			db.mu.RLock()
			defer db.mu.RUnlock()
			r := db.getRepo(owner, name)
			if r == nil {
				return 404, "", nil, "Not Found"
			}
			isV2 := wantsV2 || reqBufHasCommand(rawBody, "ls-refs") || reqBufHasCommand(rawBody, "fetch")
			var body []byte
			if isV2 && reqBufHasCommand(rawBody, "ls-refs") {
				body = handleLsRefs(r, rawBody)
			} else if isV2 {
				body = handleFetchV2(r, rawBody)
			} else {
				body = handleFetchV0(r, rawBody)
			}
			return 200, "application/x-git-upload-pack-result", body, ""
		}
		return 404, "", nil, "Not Found"
	}
	return 404, "", nil, "Not Found"
}

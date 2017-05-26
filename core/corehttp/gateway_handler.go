package corehttp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	gopath "path"
	"runtime/debug"
	"strings"
	"time"

	"github.com/ipfs/go-ipfs/core"
	coreiface "github.com/ipfs/go-ipfs/core/coreapi/interface"
	"github.com/ipfs/go-ipfs/dagutils"

	"gx/ipfs/QmPSBJL4momYnE7DcUyk2DVhD6rH488ZmHBGLbxNdhU44K/go-humanize"
	chunker "gx/ipfs/QmR4QQVkBZsZENRjYFVi8dEtPL3daZRNKk24m4r6WKJHNm/go-ipfs-chunker"
	"gx/ipfs/QmR8BauakNcBa3RbE4nbQu76PDiJgoQgz8AJdhJuiU4TAw/go-cid"
	ipld "gx/ipfs/QmRL22E4paat7ky7vx9MLpR97JHHbFPrg3ytFQw6qp1y1s/go-ipld-format"
	ft "gx/ipfs/QmSMJ4rZbCJaih3y82Ebq7BZqK6vU2FHsKcWKQiE1DPTpS/go-unixfs"
	"gx/ipfs/QmSMJ4rZbCJaih3y82Ebq7BZqK6vU2FHsKcWKQiE1DPTpS/go-unixfs/importer"
	"gx/ipfs/QmTiRqrF5zkdZyrdsL5qndG1UbeWi8k8N2pYxCtXWrahR2/go-libp2p-routing"
	"gx/ipfs/QmWqh9oob7ZHQRwU5CdTqpnC8ip8BEkFNrwXRxeNo5Y7vA/go-path"
	"gx/ipfs/QmWqh9oob7ZHQRwU5CdTqpnC8ip8BEkFNrwXRxeNo5Y7vA/go-path/resolver"
	"gx/ipfs/QmXWZCd8jfaHmt4UDSnjKmGcrQMw95bDGWqEeVLVJjoANX/go-ipfs-files"
	dag "gx/ipfs/Qmb2UEG2TAeVrEJSjqsZF7Y2he7wRDkrdt6c3bECxwZf4k/go-merkledag"
	"gx/ipfs/QmekxXDhCxCJRNuzmHreuaT3BsuJcsjcXWNrtV9C8DRHtd/go-multibase"
)

const (
	ipfsPathPrefix = "/ipfs/"
	ipnsPathPrefix = "/ipns/"
)

// gatewayHandler is a HTTP handler that serves IPFS objects (accessible by default at /ipfs/<path>)
// (it serves requests like GET /ipfs/QmVRzPKPzNtSrEzBFm2UZfxmPAgnaLke4DMcerbsGGSaFe/link)
type gatewayHandler struct {
	node   *core.IpfsNode
	config GatewayConfig
	api    coreiface.CoreAPI
}

func newGatewayHandler(n *core.IpfsNode, c GatewayConfig, api coreiface.CoreAPI) *gatewayHandler {
	i := &gatewayHandler{
		node:   n,
		config: c,
		api:    api,
	}
	return i
}

// TODO(cryptix):  find these helpers somewhere else
func (i *gatewayHandler) newDagFromReader(r io.Reader) (ipld.Node, error) {
	// TODO(cryptix): change and remove this helper once PR1136 is merged
	// return ufs.AddFromReader(i.node, r.Body)
	return importer.BuildDagFromReader(
		i.node.DAG,
		chunker.DefaultSplitter(r))
}

func (i *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// the hour is a hard fallback, we don't expect it to happen, but just in case
	ctx, cancel := context.WithTimeout(r.Context(), time.Hour)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			log.Error("A panic occurred in the gateway handler!")
			log.Error(r)
			debug.PrintStack()
		}
	}()

	if i.config.Writable {
		switch r.Method {
		case "POST":
			i.postHandler(ctx, w, r)
			return
		case "PUT":
			i.putHandler(w, r)
			return
		case "DELETE":
			i.deleteHandler(w, r)
			return
		}
	}

	if r.Method == "GET" || r.Method == "HEAD" {
		i.getOrHeadHandler(ctx, w, r)
		return
	}

	if r.Method == "OPTIONS" {
		i.optionsHandler(w, r)
		return
	}

	errmsg := "Method " + r.Method + " not allowed: "
	if !i.config.Writable {
		w.WriteHeader(http.StatusMethodNotAllowed)
		errmsg = errmsg + "read only access"
	} else {
		w.WriteHeader(http.StatusBadRequest)
		errmsg = errmsg + "bad request for " + r.URL.Path
	}
	fmt.Fprint(w, errmsg)
}

func (i *gatewayHandler) optionsHandler(w http.ResponseWriter, r *http.Request) {
	/*
		OPTIONS is a noop request that is used by the browsers to check
		if server accepts cross-site XMLHttpRequest (indicated by the presence of CORS headers)
		https://developer.mozilla.org/en-US/docs/Web/HTTP/Access_control_CORS#Preflighted_requests
	*/
	i.addUserHeaders(w) // return all custom headers (including CORS ones, if set)
}

func (i *gatewayHandler) getOrHeadHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	escapedURLPath := r.URL.EscapedPath()

	// If the gateway is behind a reverse proxy and mounted at a sub-path,
	// the prefix header can be set to signal this sub-path.
	// It will be prepended to links in directory listings and the index.html redirect.
	prefix := ""
	if prfx := r.Header.Get("X-Ipfs-Gateway-Prefix"); len(prfx) > 0 {
		for _, p := range i.config.PathPrefixes {
			if prfx == p || strings.HasPrefix(prfx, p+"/") {
				prefix = prfx
				break
			}
		}
	}

	// IPNSHostnameOption might have constructed an IPNS path using the Host header.
	// In this case, we need the original path for constructing redirects
	// and links that match the requested URL.
	// For example, http://example.net would become /ipns/example.net, and
	// the redirects and links would end up as http://example.net/ipns/example.net
	originalUrlPath := prefix + urlPath
	ipnsHostname := false
	if hdr := r.Header.Get("X-Ipns-Original-Path"); len(hdr) > 0 {
		originalUrlPath = prefix + hdr
		ipnsHostname = true
	}

	parsedPath, err := coreiface.ParsePath(urlPath)
	if err != nil {
		webError(w, "invalid ipfs path", err, http.StatusBadRequest)
		return
	}

	// Resolve path to the final DAG node for the ETag
	resolvedPath, err := i.api.ResolvePath(ctx, parsedPath)
	if err == coreiface.ErrOffline && !i.node.OnlineMode() {
		webError(w, "ipfs resolve -r "+escapedURLPath, err, http.StatusServiceUnavailable)
		return
	} else if err != nil {
		webError(w, "ipfs resolve -r "+escapedURLPath, err, http.StatusNotFound)
		return
	}

	dr, err := i.api.Unixfs().Get(ctx, resolvedPath)
	if err != nil {
		webError(w, "ipfs cat "+escapedURLPath, err, http.StatusNotFound)
		return
	}

	defer dr.Close()

	// Check etag send back to us
	etag := "\"" + resolvedPath.Cid().String() + "\""
	if r.Header.Get("If-None-Match") == etag || r.Header.Get("If-None-Match") == "W/"+etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("X-IPFS-Path", urlPath)
	w.Header().Set("Etag", etag)

	// Suborigin header, sandboxes apps from each other in the browser (even
	// though they are served from the same gateway domain).
	//
	// Omitted if the path was treated by IPNSHostnameOption(), for example
	// a request for http://example.net/ would be changed to /ipns/example.net/,
	// which would turn into an incorrect Suborigin header.
	// In this case the correct thing to do is omit the header because it is already
	// handled correctly without a Suborigin.
	//
	// NOTE: This is not yet widely supported by browsers.
	if !ipnsHostname {
		// e.g.: 1="ipfs", 2="QmYuNaKwY...", ...
		pathComponents := strings.SplitN(urlPath, "/", 4)

		var suboriginRaw []byte
		cidDecoded, err := cid.Decode(pathComponents[2])
		if err != nil {
			// component 2 doesn't decode with cid, so it must be a hostname
			suboriginRaw = []byte(strings.ToLower(pathComponents[2]))
		} else {
			suboriginRaw = cidDecoded.Bytes()
		}

		base32Encoded, err := multibase.Encode(multibase.Base32, suboriginRaw)
		if err != nil {
			internalWebError(w, err)
			return
		}

		suborigin := pathComponents[1] + "000" + strings.ToLower(base32Encoded)
		w.Header().Set("Suborigin", suborigin)
	}

	// set these headers _after_ the error, for we may just not have it
	// and dont want the client to cache a 500 response...
	// and only if it's /ipfs!
	// TODO: break this out when we split /ipfs /ipns routes.
	modtime := time.Now()

	if f, ok := dr.(files.File); ok {
		if strings.HasPrefix(urlPath, ipfsPathPrefix) {
			w.Header().Set("Cache-Control", "public, max-age=29030400, immutable")

			// set modtime to a really long time ago, since files are immutable and should stay cached
			modtime = time.Unix(1, 0)
		}

		urlFilename := r.URL.Query().Get("filename")
		var name string
		if urlFilename != "" {
			w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename*=UTF-8''%s", url.PathEscape(urlFilename)))
			name = urlFilename
		} else {
			name = getFilename(urlPath)
		}
		i.serveFile(w, r, name, modtime, f)
		return
	}
	dir, ok := dr.(files.Directory)
	if !ok {
		internalWebError(w, fmt.Errorf("unsupported file type"))
		return
	}

	idx, err := i.api.Unixfs().Get(ctx, coreiface.Join(resolvedPath, "index.html"))
	switch err.(type) {
	case nil:
		dirwithoutslash := urlPath[len(urlPath)-1] != '/'
		goget := r.URL.Query().Get("go-get") == "1"
		if dirwithoutslash && !goget {
			// See comment above where originalUrlPath is declared.
			http.Redirect(w, r, originalUrlPath+"/", 302)
			return
		}

		f, ok := idx.(files.File)
		if !ok {
			internalWebError(w, files.ErrNotReader)
			return
		}

		// write to request
		http.ServeContent(w, r, "index.html", modtime, f)
		return
	case resolver.ErrNoLink:
		// no index.html; noop
	default:
		internalWebError(w, err)
		return
	}

	if r.Method == "HEAD" {
		return
	}

	// storage for directory listing
	var dirListing []directoryItem
	dirit := dir.Entries()
	for dirit.Next() {
		// See comment above where originalUrlPath is declared.
		s, err := dirit.Node().Size()
		if err != nil {
			internalWebError(w, err)
			return
		}

		di := directoryItem{humanize.Bytes(uint64(s)), dirit.Name(), gopath.Join(originalUrlPath, dirit.Name())}
		dirListing = append(dirListing, di)
	}
	if dirit.Err() != nil {
		internalWebError(w, dirit.Err())
		return
	}

	// construct the correct back link
	// https://github.com/ipfs/go-ipfs/issues/1365
	var backLink string = prefix + urlPath

	// don't go further up than /ipfs/$hash/
	pathSplit := path.SplitList(backLink)
	switch {
	// keep backlink
	case len(pathSplit) == 3: // url: /ipfs/$hash

	// keep backlink
	case len(pathSplit) == 4 && pathSplit[3] == "": // url: /ipfs/$hash/

	// add the correct link depending on wether the path ends with a slash
	default:
		if strings.HasSuffix(backLink, "/") {
			backLink += "./.."
		} else {
			backLink += "/.."
		}
	}

	// strip /ipfs/$hash from backlink if IPNSHostnameOption touched the path.
	if ipnsHostname {
		backLink = prefix + "/"
		if len(pathSplit) > 5 {
			// also strip the trailing segment, because it's a backlink
			backLinkParts := pathSplit[3 : len(pathSplit)-2]
			backLink += path.Join(backLinkParts) + "/"
		}
	}

	var hash string
	if !strings.HasPrefix(originalUrlPath, ipfsPathPrefix) {
		hash = resolvedPath.Cid().String()
	}

	// See comment above where originalUrlPath is declared.
	tplData := listingTemplateData{
		Listing:  dirListing,
		Path:     originalUrlPath,
		BackLink: backLink,
		Hash:     hash,
	}
	err = listingTemplate.Execute(w, tplData)
	if err != nil {
		internalWebError(w, err)
		return
	}
}

type sizeReadSeeker interface {
	Size() (int64, error)

	io.ReadSeeker
}

type sizeSeeker struct {
	sizeReadSeeker
}

func (s *sizeSeeker) Seek(offset int64, whence int) (int64, error) {
	if whence == io.SeekEnd && offset == 0 {
		return s.Size()
	}

	return s.sizeReadSeeker.Seek(offset, whence)
}

func (i *gatewayHandler) serveFile(w http.ResponseWriter, req *http.Request, name string, modtime time.Time, content io.ReadSeeker) {
	if sp, ok := content.(sizeReadSeeker); ok {
		content = &sizeSeeker{
			sizeReadSeeker: sp,
		}
	}

	http.ServeContent(w, req, name, modtime, content)
}

func (i *gatewayHandler) postHandler(ctx context.Context, w http.ResponseWriter, r *http.Request) {
	p, err := i.api.Unixfs().Add(ctx, files.NewReaderFile(r.Body))
	if err != nil {
		internalWebError(w, err)
		return
	}

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", p.Cid().String())
	http.Redirect(w, r, p.String(), http.StatusCreated)
}

func (i *gatewayHandler) putHandler(w http.ResponseWriter, r *http.Request) {
	// TODO(cryptix): move me to ServeHTTP and pass into all handlers
	ctx, cancel := context.WithCancel(i.node.Context())
	defer cancel()

	rootPath, err := path.ParsePath(r.URL.Path)
	if err != nil {
		webError(w, "putHandler: IPFS path not valid", err, http.StatusBadRequest)
		return
	}

	if rootPath[len(rootPath)-1] == '/' {
		webError(w, "putHandler: can't PUT a directory", err, http.StatusBadRequest)
		return
	}

	rsegs := rootPath.Segments()
	if rsegs[0] == ipnsPathPrefix {
		webError(w, "putHandler: updating named entries not supported", errors.New("WritableGateway: ipns put not supported"), http.StatusBadRequest)
		return
	}

	var newnode ipld.Node
	if rsegs[len(rsegs)-1] == "QmUNLLsPACCz1vLxQVkXqqLX5R1X345qqfHbsf67hvA3Nn" {
		newnode = ft.EmptyDirNode()
	} else {
		putNode, err := i.newDagFromReader(r.Body)
		if err != nil {
			webError(w, "putHandler: Could not create DAG from request", err, http.StatusInternalServerError)
			return
		}
		newnode = putNode
	}

	var newPath string
	if len(rsegs) > 1 {
		newPath = path.Join(rsegs[2:])
	}

	c, err := cid.Decode(rsegs[1])
	if err != nil {
		webError(w, "putHandler: bad input path", err, http.StatusBadRequest)
		return
	}

	rnode, err := i.node.DAG.Get(ctx, c)
	if err != nil {
		webError(w, "putHandler: Could not create DAG from request", err, http.StatusInternalServerError)
		return
	}

	pbnd, ok := rnode.(*dag.ProtoNode)
	if !ok {
		webError(w, "Cannot read non protobuf nodes through gateway", dag.ErrNotProtobuf, http.StatusBadRequest)
		return
	}

	e := dagutils.NewDagEditor(pbnd, i.node.DAG)
	err = e.InsertNodeAtPath(ctx, newPath, newnode, ft.EmptyDirNode)
	if err != nil {
		webError(w, "putHandler: InsertNodeAtPath failed", err, http.StatusInternalServerError)
		return
	}

	nnode, err := e.Finalize(ctx, i.node.DAG)
	if err != nil {
		webError(w, "putHandler: could not get node", err, http.StatusInternalServerError)
		return
	}

	newcid := nnode.Cid()

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", newcid.String())
	http.Redirect(w, r, gopath.Join(ipfsPathPrefix, newcid.String(), newPath), http.StatusCreated)
}

func (i *gatewayHandler) deleteHandler(w http.ResponseWriter, r *http.Request) {
	urlPath := r.URL.Path
	ctx, cancel := context.WithCancel(i.node.Context())
	defer cancel()

	p, err := path.ParsePath(urlPath)
	if err != nil {
		webError(w, "failed to parse path", err, http.StatusBadRequest)
		return
	}

	c, components, err := path.SplitAbsPath(p)
	if err != nil {
		webError(w, "Could not split path", err, http.StatusInternalServerError)
		return
	}

	tctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	rootnd, err := i.node.Resolver.DAG.Get(tctx, c)
	if err != nil {
		webError(w, "Could not resolve root object", err, http.StatusBadRequest)
		return
	}

	pathNodes, err := i.node.Resolver.ResolveLinks(tctx, rootnd, components[:len(components)-1])
	if err != nil {
		webError(w, "Could not resolve parent object", err, http.StatusBadRequest)
		return
	}

	pbnd, ok := pathNodes[len(pathNodes)-1].(*dag.ProtoNode)
	if !ok {
		webError(w, "Cannot read non protobuf nodes through gateway", dag.ErrNotProtobuf, http.StatusBadRequest)
		return
	}

	// TODO(cyrptix): assumes len(pathNodes) > 1 - not found is an error above?
	err = pbnd.RemoveNodeLink(components[len(components)-1])
	if err != nil {
		webError(w, "Could not delete link", err, http.StatusBadRequest)
		return
	}

	var newnode *dag.ProtoNode = pbnd
	for j := len(pathNodes) - 2; j >= 0; j-- {
		if err := i.node.DAG.Add(ctx, newnode); err != nil {
			webError(w, "Could not add node", err, http.StatusInternalServerError)
			return
		}

		pathpb, ok := pathNodes[j].(*dag.ProtoNode)
		if !ok {
			webError(w, "Cannot read non protobuf nodes through gateway", dag.ErrNotProtobuf, http.StatusBadRequest)
			return
		}

		newnode, err = pathpb.UpdateNodeLink(components[j], newnode)
		if err != nil {
			webError(w, "Could not update node links", err, http.StatusInternalServerError)
			return
		}
	}

	if err := i.node.DAG.Add(ctx, newnode); err != nil {
		webError(w, "Could not add root node", err, http.StatusInternalServerError)
		return
	}

	// Redirect to new path
	ncid := newnode.Cid()

	i.addUserHeaders(w) // ok, _now_ write user's headers.
	w.Header().Set("IPFS-Hash", ncid.String())
	http.Redirect(w, r, gopath.Join(ipfsPathPrefix+ncid.String(), path.Join(components[:len(components)-1])), http.StatusCreated)
}

func (i *gatewayHandler) addUserHeaders(w http.ResponseWriter) {
	for k, v := range i.config.Headers {
		w.Header()[k] = v
	}
}

func webError(w http.ResponseWriter, message string, err error, defaultCode int) {
	if _, ok := err.(resolver.ErrNoLink); ok {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == routing.ErrNotFound {
		webErrorWithCode(w, message, err, http.StatusNotFound)
	} else if err == context.DeadlineExceeded {
		webErrorWithCode(w, message, err, http.StatusRequestTimeout)
	} else {
		webErrorWithCode(w, message, err, defaultCode)
	}
}

func webErrorWithCode(w http.ResponseWriter, message string, err error, code int) {
	w.WriteHeader(code)

	fmt.Fprintf(w, "%s: %s\n", message, err)
	if code >= 500 {
		log.Warningf("server error: %s: %s", err)
	}
}

// return a 500 error and log
func internalWebError(w http.ResponseWriter, err error) {
	webErrorWithCode(w, "internalWebError", err, http.StatusInternalServerError)
}

func getFilename(s string) string {
	if (strings.HasPrefix(s, ipfsPathPrefix) || strings.HasPrefix(s, ipnsPathPrefix)) && strings.Count(gopath.Clean(s), "/") <= 2 {
		// Don't want to treat ipfs.io in /ipns/ipfs.io as a filename.
		return ""
	}
	return gopath.Base(s)
}

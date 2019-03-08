// Package ais provides core functionality for the AIStore object storage.
/*
 * Copyright (c) 2018, NVIDIA CORPORATION. All rights reserved.
 */
package ais

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NVIDIA/aistore/3rdparty/glog"
	"github.com/NVIDIA/aistore/3rdparty/golang/mux"
	"github.com/NVIDIA/aistore/cluster"
	"github.com/NVIDIA/aistore/cmn"
	"github.com/NVIDIA/aistore/stats"
	"github.com/NVIDIA/aistore/stats/statsd"
	"github.com/OneOfOne/xxhash"
	jsoniter "github.com/json-iterator/go"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

const ( //  h.call(timeout)
	defaultTimeout = time.Duration(-1)
	longTimeout    = time.Duration(0)
)

var (
	// Translates the various query values for URLParamBckProvider for cluster use
	bckProviderMap = map[string]string{
		// Cloud values
		cmn.CloudBs:        cmn.CloudBs,
		cmn.ProviderAmazon: cmn.CloudBs,
		cmn.ProviderGoogle: cmn.CloudBs,

		// Local values
		cmn.LocalBs:     cmn.LocalBs,
		cmn.ProviderAIS: cmn.LocalBs,

		// unset
		"": "",
	}
)

type (
	metric = statsd.Metric // type alias

	// callResult contains http response
	callResult struct {
		si      *cluster.Snode
		outjson []byte
		err     error
		errstr  string
		status  int
	}

	// reqArgs specifies http request that we want to send
	reqArgs struct {
		method string      // GET, POST, ...
		header http.Header // request headers
		base   string      // base URL: http://xyz.abc
		path   string      // path URL: /x/y/z
		query  url.Values  // query: ?x=y&y=z
		body   []byte      // body for POST, PUT, ...
	}

	// callArgs contains arguments for a peer-to-peer control plane call
	callArgs struct {
		req     reqArgs
		timeout time.Duration
		si      *cluster.Snode
	}

	// bcastCallArgs contains arguments for an intra-cluster broadcast call
	bcastCallArgs struct {
		req     reqArgs
		network string // on of the cmn.KnownNetworks
		timeout time.Duration
		nodes   []cluster.NodeMap
	}

	networkHandler struct {
		r   string           // resource
		h   http.HandlerFunc // handler
		net []string
	}

	// SmapVoteMsg contains the cluster map and a bool representing whether or not a vote is currently happening.
	SmapVoteMsg struct {
		VoteInProgress bool      `json:"vote_in_progress"`
		Smap           *smapX    `json:"smap"`
		BucketMD       *bucketMD `json:"bucketmd"`
	}

	// actionMsgInternal is an extended ActionMsg with extra information for node <=> node control plane communications
	actionMsgInternal struct {
		cmn.ActionMsg
		BMDVersion  int64 `json:"bmdversion"`
		SmapVersion int64 `json:"smapversion"`

		// special field: used when new target is registered to primary proxy
		// storage targets make use of NewDaemonID,
		// to figure out whether to rebalance the cluster, and how to execute the rebalancing
		NewDaemonID string `json:"newdaemonid"`
	}
)

//===========
//
// interfaces
//
//===========
const initialBucketListSize = 128

type cloudif interface {
	listbucket(ctx context.Context, bucket string, msg *cmn.GetMsg) (jsbytes []byte, errstr string, errcode int)
	headbucket(ctx context.Context, bucket string) (bucketprops cmn.SimpleKVs, errstr string, errcode int)
	getbucketnames(ctx context.Context) (buckets []string, errstr string, errcode int)
	//
	headobject(ctx context.Context, bucket string, objname string) (objmeta cmn.SimpleKVs, errstr string, errcode int)
	//
	getobj(ctx context.Context, fqn, bucket, objname string) (props *cluster.LOM, errstr string, errcode int)
	putobj(ctx context.Context, file *os.File, bucket, objname string, cksum cmn.CksumProvider) (version string, errstr string, errcode int)
	deleteobj(ctx context.Context, bucket, objname string) (errstr string, errcode int)
}

func (u reqArgs) url() string {
	url := strings.TrimSuffix(u.base, "/")
	if !strings.HasPrefix(u.path, "/") {
		url += "/"
	}
	url += u.path
	query := u.query.Encode()
	if query != "" {
		url += "?" + query
	}
	return url
}

//===========
//
// generic bad-request http handler
//
//===========

// Copies headers from original request(from client) to
// a new one(inter-cluster call)
func copyHeaders(src http.Header, dst *http.Header) {
	for k, values := range src {
		for _, v := range values {
			dst.Add(k, v)
		}
	}
}

//===========================================================================
//
// http runner
//
//===========================================================================
type glogwriter struct {
}

func (r *glogwriter) Write(p []byte) (int, error) {
	n := len(p)
	s := string(p[:n])
	glog.Errorln(s)

	stacktrace := debug.Stack()
	n1 := len(stacktrace)
	s1 := string(stacktrace[:n1])
	glog.Errorln(s1)
	return n, nil
}

type netServer struct {
	s   *http.Server
	mux *mux.ServeMux
}

// Override muxer ServeHTTP to support proxying HTTPS requests. Clients
// initiates all HTTPS requests with CONNECT method instead of GET/PUT etc
func (server *netServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		server.mux.ServeHTTP(w, r)
		return
	}

	// TODO: add support for caching HTTPS requests
	destConn, err := net.DialTimeout("tcp", r.Host, 10*time.Second)
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	// First, send that everything is OK. Trying to write a header after
	// hijacking generates a warning and nothing works
	w.WriteHeader(http.StatusOK)
	// Second, hijack the connection. A kind of man-in-the-middle attack
	// Since this moment this function is responsible of HTTP connection
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "Client does not support hijacking", http.StatusInternalServerError)
		return
	}
	clientConn, _, err := hijacker.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
	}

	// Third, start transparently sending data between source and destination
	// by creating a tunnel between them
	transfer := func(destination io.WriteCloser, source io.ReadCloser) {
		io.Copy(destination, source)
		source.Close()
		destination.Close()
	}

	// NOTE: it looks like double closing both connections.
	// Need to check how the tunnel works
	go transfer(destConn, clientConn)
	go transfer(clientConn, destConn)
}

type httprunner struct {
	cmn.Named
	publicServer          *netServer
	intraControlServer    *netServer
	intraDataServer       *netServer
	glogger               *log.Logger
	si                    *cluster.Snode
	httpclient            *http.Client // http client for intra-cluster comm
	httpclientLongTimeout *http.Client // http client for long-wait intra-cluster comm
	keepalive             keepaliver
	smapowner             *smapowner
	smaplisteners         *smaplisteners
	bmdowner              *bmdowner
	xactions              *xactions
	statsif               stats.Tracker
	statsdC               statsd.Client
}

func (server *netServer) listenAndServe(addr string, logger *log.Logger) error {
	config := cmn.GCO.Get()

	// Optimization: use "slow" HTTP handler only if the cluster works in Cloud
	// reverse proxy mode. Without the optimization every HTTP request would
	// waste time by getting and reading global configuration, string
	// comparison and branching
	var httpHandler http.Handler = server.mux
	if config.Net.HTTP.RevProxy == cmn.RevProxyCloud {
		httpHandler = server
	}

	if config.Net.HTTP.UseHTTPS {
		server.s = &http.Server{Addr: addr, Handler: httpHandler, ErrorLog: logger}
		if err := server.s.ListenAndServeTLS(config.Net.HTTP.Certificate, config.Net.HTTP.Key); err != nil {
			if err != http.ErrServerClosed {
				glog.Errorf("Terminated server with err: %v", err)
				return err
			}
		}
	} else {
		// Support for h2c is transparent using h2c.NewHandler, which implements a lightweight
		// wrapper around server.mux.ServeHTTP to check for an h2c connection.
		server.s = &http.Server{Addr: addr, Handler: h2c.NewHandler(httpHandler, &http2.Server{}), ErrorLog: logger}
		if err := server.s.ListenAndServe(); err != nil {
			if err != http.ErrServerClosed {
				glog.Errorf("Terminated server with err: %v", err)
				return err
			}
		}
	}

	return nil
}

func (server *netServer) shutdown() {
	contextwith, cancel := context.WithTimeout(context.Background(), cmn.GCO.Get().Timeout.Default)
	if err := server.s.Shutdown(contextwith); err != nil {
		glog.Infof("Stopped server, err: %v", err)
	}
	cancel()
}

func (h *httprunner) registerNetworkHandlers(networkHandlers []networkHandler) {
	config := cmn.GCO.Get()

	for _, nh := range networkHandlers {
		if nh.r == "/" {
			if cmn.StringInSlice(cmn.NetworkPublic, nh.net) {
				h.registerPublicNetHandler("/", cmn.InvalidHandler)
			}
			if config.Net.UseIntraControl {
				h.registerIntraControlNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
			}
			if config.Net.UseIntraData {
				h.registerIntraDataNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
			}
			continue
		}

		if cmn.StringInSlice(cmn.NetworkPublic, nh.net) {
			h.registerPublicNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)

			if config.Net.UseIntraControl && cmn.StringInSlice(cmn.NetworkIntraControl, nh.net) {
				h.registerIntraControlNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
			}
			if config.Net.UseIntraData && cmn.StringInSlice(cmn.NetworkIntraData, nh.net) {
				h.registerIntraDataNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
			}
		} else if cmn.StringInSlice(cmn.NetworkIntraControl, nh.net) {
			h.registerIntraControlNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)

			if config.Net.UseIntraData && cmn.StringInSlice(cmn.NetworkIntraData, nh.net) {
				h.registerIntraDataNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
			}
		} else if cmn.StringInSlice(cmn.NetworkIntraData, nh.net) {
			h.registerIntraDataNetHandler(cmn.URLPath(cmn.Version, nh.r), nh.h)
		}
	}
}

func (h *httprunner) registerPublicNetHandler(path string, handler func(http.ResponseWriter, *http.Request)) {
	h.publicServer.mux.HandleFunc(path, handler)
	if !strings.HasSuffix(path, "/") {
		h.publicServer.mux.HandleFunc(path+"/", handler)
	}
}

func (h *httprunner) registerIntraControlNetHandler(path string, handler func(http.ResponseWriter, *http.Request)) {
	h.intraControlServer.mux.HandleFunc(path, handler)
	if !strings.HasSuffix(path, "/") {
		h.intraControlServer.mux.HandleFunc(path+"/", handler)
	}
}

func (h *httprunner) registerIntraDataNetHandler(path string, handler func(http.ResponseWriter, *http.Request)) {
	h.intraDataServer.mux.HandleFunc(path, handler)
	if !strings.HasSuffix(path, "/") {
		h.intraDataServer.mux.HandleFunc(path+"/", handler)
	}
}

func (h *httprunner) init(s stats.Tracker, isproxy bool) {
	h.statsif = s

	config := cmn.GCO.Get()
	h.httpclient = cmn.NewClient(cmn.ClientArgs{
		Timeout:  config.Timeout.Default,
		UseHTTPS: config.Net.HTTP.UseHTTPS,
	})
	h.httpclientLongTimeout = cmn.NewClient(cmn.ClientArgs{
		Timeout:  config.Timeout.DefaultLong,
		UseHTTPS: config.Net.HTTP.UseHTTPS,
	})

	h.publicServer = &netServer{
		mux: mux.NewServeMux(),
	}
	h.intraControlServer = h.publicServer // by default intra control net is the same as public
	if config.Net.UseIntraControl {
		h.intraControlServer = &netServer{
			mux: mux.NewServeMux(),
		}
	}
	h.intraDataServer = h.publicServer // by default intra data net is the same as public
	if config.Net.UseIntraData {
		h.intraDataServer = &netServer{
			mux: mux.NewServeMux(),
		}
	}

	h.smaplisteners = newSmapListeners()
	h.smapowner = &smapowner{listeners: h.smaplisteners}
	h.bmdowner = &bmdowner{}
	h.xactions = newXs() // extended actions
}

// initSI initializes this cluster.Snode
func (h *httprunner) initSI() {
	var s string
	config := cmn.GCO.Get()
	allowLoopback, _ := strconv.ParseBool(os.Getenv("ALLOW_LOOPBACK"))
	addrList, err := getLocalIPv4List(allowLoopback)
	if err != nil {
		glog.Fatalf("FATAL: %v", err)
	}

	ipAddr, err := getipv4addr(addrList, config.Net.IPv4)
	if err != nil {
		glog.Fatalf("Failed to get PUBLIC IPv4/hostname: %v", err)
	}
	if config.Net.IPv4 != "" {
		s = " (config: " + config.Net.IPv4 + ")"
	}
	glog.Infof("PUBLIC (user) access: [%s:%d]%s", ipAddr, config.Net.L4.Port, s)

	ipAddrIntraControl := net.IP{}
	if config.Net.UseIntraControl {
		ipAddrIntraControl, err = getipv4addr(addrList, config.Net.IPv4IntraControl)
		if err != nil {
			glog.Fatalf("Failed to get INTRA-CONTROL IPv4/hostname: %v", err)
		}
		s = ""
		if config.Net.IPv4IntraControl != "" {
			s = " (config: " + config.Net.IPv4IntraControl + ")"
		}
		glog.Infof("INTRA-CONTROL access: [%s:%d]%s", ipAddrIntraControl, config.Net.L4.PortIntraControl, s)
	}

	ipAddrIntraData := net.IP{}
	if config.Net.UseIntraData {
		ipAddrIntraData, err = getipv4addr(addrList, config.Net.IPv4IntraData)
		if err != nil {
			glog.Fatalf("Failed to get INTRA-DATA IPv4/hostname: %v", err)
		}
		s = ""
		if config.Net.IPv4IntraData != "" {
			s = " (config: " + config.Net.IPv4IntraData + ")"
		}
		glog.Infof("INTRA-DATA access: [%s:%d]%s", ipAddrIntraData, config.Net.L4.PortIntraData, s)
	}

	publicAddr := &net.TCPAddr{
		IP:   ipAddr,
		Port: config.Net.L4.Port,
	}
	intraControlAddr := &net.TCPAddr{
		IP:   ipAddrIntraControl,
		Port: config.Net.L4.PortIntraControl,
	}
	intraDataAddr := &net.TCPAddr{
		IP:   ipAddrIntraData,
		Port: config.Net.L4.PortIntraData,
	}

	daemonID := os.Getenv("AIS_DAEMONID")
	if daemonID == "" {
		cs := xxhash.ChecksumString32S(publicAddr.String(), cluster.MLCG32)
		daemonID = strconv.Itoa(int(cs & 0xfffff))
		if cmn.TestingEnv() {
			daemonID += ":" + config.Net.L4.PortStr
		}
	}

	h.si = newSnode(daemonID, config.Net.HTTP.Proto, publicAddr, intraControlAddr, intraDataAddr)
}

func (h *httprunner) run() error {
	config := cmn.GCO.Get()

	// a wrapper to glog http.Server errors - otherwise
	// os.Stderr would be used, as per golang.org/pkg/net/http/#Server
	h.glogger = log.New(&glogwriter{}, "net/http err: ", 0)

	if config.Net.UseIntraControl || config.Net.UseIntraData {
		var errCh chan error
		if config.Net.UseIntraControl && config.Net.UseIntraData {
			errCh = make(chan error, 3)
		} else {
			errCh = make(chan error, 2)
		}

		if config.Net.UseIntraControl {
			go func() {
				addr := h.si.IntraControlNet.NodeIPAddr + ":" + h.si.IntraControlNet.DaemonPort
				errCh <- h.intraControlServer.listenAndServe(addr, h.glogger)
			}()
		}

		if config.Net.UseIntraData {
			go func() {
				addr := h.si.IntraDataNet.NodeIPAddr + ":" + h.si.IntraDataNet.DaemonPort
				errCh <- h.intraDataServer.listenAndServe(addr, h.glogger)
			}()
		}

		go func() {
			addr := h.si.PublicNet.NodeIPAddr + ":" + h.si.PublicNet.DaemonPort
			errCh <- h.publicServer.listenAndServe(addr, h.glogger)
		}()

		return <-errCh
	}

	// When only public net is configured listen on *:port
	addr := ":" + h.si.PublicNet.DaemonPort
	return h.publicServer.listenAndServe(addr, h.glogger)
}

// stop gracefully
func (h *httprunner) stop(err error) {
	config := cmn.GCO.Get()
	glog.Infof("Stopping %s, err: %v", h.Getname(), err)

	h.statsdC.Close()
	if h.publicServer.s == nil {
		return
	}

	wg := &sync.WaitGroup{}
	wg.Add(1)
	go func() {
		h.publicServer.shutdown()
		wg.Done()
	}()

	if config.Net.UseIntraControl {
		wg.Add(1)
		go func() {
			h.intraControlServer.shutdown()
			wg.Done()
		}()
	}

	if config.Net.UseIntraData {
		wg.Add(1)
		go func() {
			h.intraDataServer.shutdown()
			wg.Done()
		}()
	}

	wg.Wait()
}

//=================================
//
// intra-cluster IPC, control plane
//
//=================================
// call another target or a proxy
// optionally, include a json-encoded body
func (h *httprunner) call(args callArgs) callResult {
	var (
		request  *http.Request
		response *http.Response
		sid      = "unknown"
		outjson  []byte
		err      error
		errstr   string
		status   int
	)

	if args.si != nil {
		sid = args.si.DaemonID
	}

	cmn.Assert(args.si != nil || args.req.base != "") // either we have si or base
	if args.req.base == "" && args.si != nil {
		args.req.base = args.si.IntraControlNet.DirectURL // by default use intra-cluster control network
	}

	url := args.req.url()
	if len(args.req.body) == 0 {
		request, err = http.NewRequest(args.req.method, url, nil)
	} else {
		request, err = http.NewRequest(args.req.method, url, bytes.NewBuffer(args.req.body))
		if err == nil {
			request.Header.Set("Content-Type", "application/json")
		}
	}

	if err != nil {
		errstr = fmt.Sprintf("Unexpected failure to create http request %s %s, err: %v", args.req.method, url, err)
		return callResult{args.si, outjson, err, errstr, status}
	}

	copyHeaders(args.req.header, &request.Header)
	switch args.timeout {
	case defaultTimeout:
		response, err = h.httpclient.Do(request)
	case longTimeout:
		response, err = h.httpclientLongTimeout.Do(request)
	default:
		contextwith, cancel := context.WithTimeout(context.Background(), args.timeout)
		defer cancel() // timeout => context.deadlineExceededError
		newRequest := request.WithContext(contextwith)
		copyHeaders(args.req.header, &newRequest.Header)
		if args.timeout > h.httpclient.Timeout {
			response, err = h.httpclientLongTimeout.Do(newRequest)
		} else {
			response, err = h.httpclient.Do(newRequest)
		}
	}
	if err != nil {
		if response != nil && response.StatusCode > 0 {
			errstr = fmt.Sprintf("Failed to http-call %s (%s %s): status %s, err %v", sid, args.req.method, url, response.Status, err)
			status = response.StatusCode
			return callResult{args.si, outjson, err, errstr, status}
		}

		errstr = fmt.Sprintf("Failed to http-call %s (%s %s): err %v", sid, args.req.method, url, err)
		return callResult{args.si, outjson, err, errstr, status}
	}

	if outjson, err = ioutil.ReadAll(response.Body); err != nil {
		errstr = fmt.Sprintf("Failed to http-call %s (%s %s): read response err: %v", sid, args.req.method, url, err)
		if err == io.EOF {
			trailer := response.Trailer.Get("Error")
			if trailer != "" {
				errstr = fmt.Sprintf("Failed to http-call %s (%s %s): err: %v, trailer: %s", sid, args.req.method, url, err, trailer)
			}
		}

		response.Body.Close()
		return callResult{args.si, outjson, err, errstr, status}
	}
	response.Body.Close()

	// err == nil && bad status: response.Body contains the error message
	if response.StatusCode >= http.StatusBadRequest {
		err = fmt.Errorf("%s, status code: %d", outjson, response.StatusCode)
		errstr = err.Error()
		status = response.StatusCode
		return callResult{args.si, outjson, err, errstr, status}
	}

	if sid != "unknown" {
		h.keepalive.heardFrom(sid, false /* reset */)
	}

	return callResult{args.si, outjson, err, errstr, status}
}

//
// broadcast
//

func (h *httprunner) broadcastTo(path string, query url.Values, method string, body []byte,
	smap *smapX, timeout time.Duration, network string, to int) chan callResult {
	var nodes []cluster.NodeMap

	switch to {
	case cluster.Targets:
		nodes = []cluster.NodeMap{smap.Tmap}
	case cluster.Proxies:
		nodes = []cluster.NodeMap{smap.Pmap}
	case cluster.AllNodes:
		nodes = []cluster.NodeMap{smap.Pmap, smap.Tmap}
	default:
		cmn.Assert(false)
	}
	if !cmn.NetworkIsKnown(network) {
		cmn.AssertMsg(false, "unknown network '"+network+"'")
	}
	bcastArgs := bcastCallArgs{
		req: reqArgs{
			method: method,
			path:   path,
			query:  query,
			body:   body,
		},
		network: network,
		timeout: timeout,
		nodes:   nodes,
	}
	return h.broadcast(bcastArgs)
}

// NOTE: 'u' has only the path and query part, host portion will be set by this method.
func (h *httprunner) broadcast(bcastArgs bcastCallArgs) chan callResult {
	nodeCount := 0
	for _, nodeMap := range bcastArgs.nodes {
		nodeCount += len(nodeMap)
	}
	if nodeCount == 0 {
		ch := make(chan callResult)
		close(ch)
		glog.Warningf("node count zero in [%+v] bcast", bcastArgs.req)
		return ch
	}
	ch := make(chan callResult, nodeCount)
	wg := &sync.WaitGroup{}

	for _, nodeMap := range bcastArgs.nodes {
		for sid, serverInfo := range nodeMap {
			if sid == h.si.DaemonID {
				continue
			}
			wg.Add(1)
			go func(di *cluster.Snode) {
				args := callArgs{
					si:      di,
					req:     bcastArgs.req,
					timeout: bcastArgs.timeout,
				}
				args.req.base = di.URL(bcastArgs.network)

				res := h.call(args)
				ch <- res
				wg.Done()
			}(serverInfo)
		}
	}

	wg.Wait()
	close(ch)
	return ch
}

func (h *httprunner) newActionMsgInternalStr(msgStr string, smap *smapX, bmdowner *bucketMD) *actionMsgInternal {
	return h.newActionMsgInternal(&cmn.ActionMsg{Value: msgStr}, smap, bmdowner)
}

func (h *httprunner) newActionMsgInternal(actionMsg *cmn.ActionMsg, smap *smapX, bmdowner *bucketMD) *actionMsgInternal {
	msgInt := &actionMsgInternal{ActionMsg: *actionMsg}
	if smap != nil {
		msgInt.SmapVersion = smap.Version
	} else {
		msgInt.SmapVersion = h.smapowner.Get().Version
	}
	if bmdowner != nil {
		msgInt.BMDVersion = bmdowner.Version
	} else {
		msgInt.BMDVersion = h.bmdowner.Get().Version
	}
	return msgInt
}

//=============================
//
// http request parsing helpers
//
//=============================

// remove validated fields and return the resulting slice
func (h *httprunner) checkRESTItems(w http.ResponseWriter, r *http.Request, itemsAfter int, splitAfter bool, items ...string) ([]string, error) {
	items, err := cmn.MatchRESTItems(r.URL.Path, itemsAfter, splitAfter, items...)
	if err != nil {
		s := err.Error()
		if _, file, line, ok := runtime.Caller(1); ok {
			f := filepath.Base(file)
			s += fmt.Sprintf("(%s, #%d)", f, line)
		}
		h.invalmsghdlr(w, r, s, http.StatusBadRequest)
		return nil, errors.New(s)
	}

	return items, nil
}

// NOTE: must be the last error-generating-and-handling call in the http handler
//       writes http body and header
//       calls invalmsghdlr() on err
func (h *httprunner) writeJSON(w http.ResponseWriter, r *http.Request, jsbytes []byte, tag string) (ok bool) {
	w.Header().Set("Content-Type", "application/json")
	var err error
	if _, err = w.Write(jsbytes); err == nil {
		ok = true
		return
	}
	if isSyscallWriteError(err) {
		// apparently, cannot write to this w: broken-pipe and similar
		glog.Errorf("isSyscallWriteError: %v", err)
		s := "isSyscallWriteError: " + r.Method + " " + r.URL.Path
		if _, file, line, ok2 := runtime.Caller(1); ok2 {
			f := filepath.Base(file)
			s += fmt.Sprintf("(%s, #%d)", f, line)
		}
		glog.Errorln(s)
		h.statsif.AddErrorHTTP(r.Method, 1)
		return
	}
	errstr := fmt.Sprintf("%s: Failed to write json, err: %v", tag, err)
	if _, file, line, ok := runtime.Caller(1); ok {
		f := filepath.Base(file)
		errstr += fmt.Sprintf("(%s, #%d)", f, line)
	}
	h.invalmsghdlr(w, r, errstr)
	return
}

func (h *httprunner) validatebckname(w http.ResponseWriter, r *http.Request, bucket string) bool {
	if strings.Contains(bucket, string(filepath.Separator)) {
		s := fmt.Sprintf("Invalid bucket name %s (contains '/')", bucket)
		if _, file, line, ok := runtime.Caller(1); ok {
			f := filepath.Base(file)
			s += fmt.Sprintf("(%s, #%d)", f, line)
		}
		h.invalmsghdlr(w, r, s)
		return false
	}
	return true
}

func validCloudProvider(bckProvider, cloudProvider string) bool {
	return bckProvider == cloudProvider || bckProvider == cmn.CloudBs
}

func (h *httprunner) validateBckProvider(bckProvider, bucket string) (isLocal bool, errstr string) {
	bckProvider = strings.ToLower(bckProvider)
	config := cmn.GCO.Get()
	val, ok := bckProviderMap[bckProvider]
	if !ok {
		errstr = fmt.Sprintf("Invalid value %s for %s", bckProvider, cmn.URLParamBckProvider)
		return
	}

	// Get bucket names
	if bucket == "*" {
		return
	}

	bckIsLocal := h.bmdowner.get().IsLocal(bucket)
	switch val {
	case cmn.LocalBs:
		// Check if local bucket does exist
		if !bckIsLocal {
			errstr = fmt.Sprintf("Local bucket %s %s", bucket, cmn.DoesNotExist)
			return
		}
		isLocal = true
	case cmn.CloudBs:
		// Check if user does have the associated cloud
		if !validCloudProvider(bckProvider, config.CloudProvider) {
			errstr = fmt.Sprintf("Cluster cloud provider '%s', mis-match bucket provider '%s'",
				config.CloudProvider, bckProvider)
			return
		}
		isLocal = false
	default:
		isLocal = bckIsLocal
	}

	return
}

//=========================
//
// common http req handlers
//
//==========================
func (h *httprunner) httpdaeget(w http.ResponseWriter, r *http.Request) {
	var (
		jsbytes []byte
		err     error
		getWhat = r.URL.Query().Get(cmn.URLParamWhat)
	)
	switch getWhat {
	case cmn.GetWhatConfig:
		jsbytes, err = jsoniter.Marshal(cmn.GCO.Get())
		cmn.AssertNoErr(err)
	case cmn.GetWhatSmap:
		jsbytes, err = jsoniter.Marshal(h.smapowner.get())
		cmn.AssertNoErr(err)
	case cmn.GetWhatBucketMeta:
		jsbytes, err = jsoniter.Marshal(h.bmdowner.get())
		cmn.AssertNoErr(err)
	case cmn.GetWhatSmapVote:
		_, xx := h.xactions.findL(cmn.ActElection)
		vote := xx != nil
		msg := SmapVoteMsg{VoteInProgress: vote, Smap: h.smapowner.get(), BucketMD: h.bmdowner.get()}
		jsbytes, err = jsoniter.Marshal(msg)
		cmn.AssertNoErr(err)
	case cmn.GetWhatDaemonInfo:
		jsbytes, err = jsoniter.Marshal(h.si)
		cmn.AssertNoErr(err)
	default:
		s := fmt.Sprintf("Invalid GET /daemon request: unrecognized what=%s", getWhat)
		h.invalmsghdlr(w, r, s)
		return
	}
	h.writeJSON(w, r, jsbytes, "httpdaeget-"+getWhat)
}

//=================
//
// http err + spec message + code + stats
//
//=================

func (h *httprunner) invalmsghdlr(w http.ResponseWriter, r *http.Request, msg string, errCode ...int) {
	cmn.InvalidHandlerDetailed(w, r, msg, errCode...)
	h.statsif.AddErrorHTTP(r.Method, 1)
}

//=====================
//
// metasync Rx handlers
//
//=====================
func (h *httprunner) extractSmap(payload cmn.SimpleKVs) (newsmap *smapX, msgInt *actionMsgInternal, errstr string) {
	if _, ok := payload[smaptag]; !ok {
		return
	}
	newsmap, msgInt = &smapX{}, &actionMsgInternal{}
	smapvalue := payload[smaptag]
	msgvalue := ""
	if err := jsoniter.Unmarshal([]byte(smapvalue), newsmap); err != nil {
		errstr = fmt.Sprintf("Failed to unmarshal new smap, value (%+v, %T), err: %v", smapvalue, smapvalue, err)
		return
	}
	if _, ok := payload[smaptag+actiontag]; ok {
		msgvalue = payload[smaptag+actiontag]
		if err := jsoniter.Unmarshal([]byte(msgvalue), msgInt); err != nil {
			errstr = fmt.Sprintf("Failed to unmarshal action message, value (%+v, %T), err: %v", msgvalue, msgvalue, err)
			return
		}
	}
	localsmap := h.smapowner.get()
	myver := localsmap.version()
	if newsmap.version() == myver {
		newsmap = nil
		return
	}
	if !newsmap.isValid() {
		errstr = fmt.Sprintf("Invalid Smap v%d - lacking or missing the primary", newsmap.version())
		newsmap = nil
		return
	}
	if newsmap.version() < myver {
		if h.si != nil && localsmap.GetTarget(h.si.DaemonID) == nil {
			errstr = fmt.Sprintf("%s: Attempt to downgrade Smap v%d to v%d", h.si, myver, newsmap.version())
			newsmap = nil
			return
		}
		if h.si != nil && localsmap.GetTarget(h.si.DaemonID) != nil {
			glog.Errorf("target %s: receive Smap v%d < v%d local - proceeding anyway",
				h.si.DaemonID, newsmap.version(), localsmap.version())
		} else {
			errstr = fmt.Sprintf("Attempt to downgrade Smap v%d to v%d", myver, newsmap.version())
			return
		}
	}
	s := ""
	if msgInt.Action != "" {
		s = ", action " + msgInt.Action
	}
	glog.Infof("receive Smap v%d (local v%d), ntargets %d%s", newsmap.version(), localsmap.version(), newsmap.CountTargets(), s)
	return
}

func (h *httprunner) extractbucketmd(payload cmn.SimpleKVs) (newbucketmd *bucketMD, msgInt *actionMsgInternal, errstr string) {
	if _, ok := payload[bucketmdtag]; !ok {
		return
	}
	newbucketmd, msgInt = &bucketMD{}, &actionMsgInternal{}
	bmdvalue := payload[bucketmdtag]
	msgvalue := ""
	if err := jsoniter.Unmarshal([]byte(bmdvalue), newbucketmd); err != nil {
		errstr = fmt.Sprintf("Failed to unmarshal new %s, value (%+v, %T), err: %v", bmdTermName, bmdvalue, bmdvalue, err)
		return
	}
	if _, ok := payload[bucketmdtag+actiontag]; ok {
		msgvalue = payload[bucketmdtag+actiontag]
		if err := jsoniter.Unmarshal([]byte(msgvalue), msgInt); err != nil {
			errstr = fmt.Sprintf("Failed to unmarshal action message, value (%+v, %T), err: %v", msgvalue, msgvalue, err)
			return
		}
	}
	myver := h.bmdowner.get().version()
	if newbucketmd.version() <= myver {
		if newbucketmd.version() < myver {
			errstr = fmt.Sprintf("Attempt to downgrade %s v%d to v%d", bmdTermName, myver, newbucketmd.version())
		}
		newbucketmd = nil
	}
	return
}

func (h *httprunner) extractRevokedTokenList(payload cmn.SimpleKVs) (*TokenList, string) {
	bytes, ok := payload[tokentag]
	if !ok {
		return nil, ""
	}

	msgInt := actionMsgInternal{}
	if _, ok := payload[tokentag+actiontag]; ok {
		msgvalue := payload[tokentag+actiontag]
		if err := jsoniter.Unmarshal([]byte(msgvalue), &msgInt); err != nil {
			errstr := fmt.Sprintf(
				"Failed to unmarshal action message, value (%+v, %T), err: %v",
				msgvalue, msgvalue, err)
			return nil, errstr
		}
	}

	tokenList := &TokenList{}
	if err := jsoniter.Unmarshal([]byte(bytes), tokenList); err != nil {
		return nil, fmt.Sprintf(
			"Failed to unmarshal blocked token list, value (%+v, %T), err: %v",
			bytes, bytes, err)
	}

	s := ""
	if msgInt.Action != "" {
		s = ", action " + msgInt.Action
	}
	glog.Infof("received TokenList ntokens %d%s", len(tokenList.Tokens), s)

	return tokenList, ""
}

// ================================== Background =========================================
//
// Generally, AIStore clusters can be deployed with an arbitrary numbers of proxies.
// Each proxy/gateway provides full access to the clustered objects and collaborates with
// all other proxies to perform majority-voted HA failovers.
//
// Not all proxies are equal though.
//
// Two out of all proxies can be designated via configuration as "original" and
// "discovery." The "original" (located at the configurable "original_url") is expected
// to be the primary at cluster (initial) deployment time.
//
// Later on, when and if some HA event triggers an automated failover, the role of the
// primary may be (automatically) assumed by a different proxy/gateway, with the
// corresponding update getting synchronized across all running nodes.
// A new node, however, could potentially experience a problem when trying to join the
// cluster simply because its configuration would still be referring to the old primary.
// The added "discovery_url" is precisely intended to address this scenario.
//
// Here's how a node joins a AIStore cluster:
// - first, there's the primary proxy/gateway referenced by the current cluster map (Smap)
//   or - during the cluster deployment time - by the the configured "primary_url"
//   (see setup/config.sh)
//
// - if that one fails, the new node goes ahead and tries the alternatives:
// 	- config.Proxy.DiscoveryURL ("discovery_url")
// 	- config.Proxy.OriginalURL ("original_url")
// - but only if those are defined and different from the previously tried.
//
// ================================== Background =========================================
func (h *httprunner) join(isproxy bool, query url.Values) (res callResult) {
	url, psi := h.getPrimaryURLAndSI()
	res = h.registerToURL(url, psi, defaultTimeout, isproxy, query, false)
	if res.err == nil {
		return
	}
	config := cmn.GCO.Get()
	if config.Proxy.DiscoveryURL != "" && config.Proxy.DiscoveryURL != url {
		glog.Errorf("%s: (register => %s: %v - retrying => %s...)", h.si, url, res.err, config.Proxy.DiscoveryURL)
		resAlt := h.registerToURL(config.Proxy.DiscoveryURL, psi, defaultTimeout, isproxy, query, false)
		if resAlt.err == nil {
			res = resAlt
			return
		}
	}
	if config.Proxy.OriginalURL != "" && config.Proxy.OriginalURL != url &&
		config.Proxy.OriginalURL != config.Proxy.DiscoveryURL {
		glog.Errorf("%s: (register => %s: %v - retrying => %s...)", h.si, url, res.err, config.Proxy.OriginalURL)
		resAlt := h.registerToURL(config.Proxy.OriginalURL, psi, defaultTimeout, isproxy, query, false)
		if resAlt.err == nil {
			res = resAlt
			return
		}
	}
	return
}

func (h *httprunner) registerToURL(url string, psi *cluster.Snode, timeout time.Duration, isproxy bool, query url.Values,
	keepalive bool) (res callResult) {
	info, err := jsoniter.Marshal(h.si)
	cmn.AssertNoErr(err)

	path := cmn.URLPath(cmn.Version, cmn.Cluster)
	if isproxy {
		path += cmn.URLPath(cmn.Proxy)
	}
	if keepalive {
		path += cmn.URLPath(cmn.Keepalive)
	}

	callArgs := callArgs{
		si: psi,
		req: reqArgs{
			method: http.MethodPost,
			base:   url,
			path:   path,
			query:  query,
			body:   info,
		},
		timeout: timeout,
	}
	for rcount := 0; rcount < 2; rcount++ {
		res = h.call(callArgs)
		if res.err == nil {
			if !keepalive {
				glog.Infof("%s: registered => %s/%s", h.si, url, path)
			}
			return
		}
		if cmn.IsErrConnectionRefused(res.err) {
			glog.Errorf("%s: (register => %s/%s: connection refused)", h.si, url, path)
		} else {
			glog.Errorf("%s: (register => %s/%s: %v)", h.si, url, path, res.err)
		}
	}
	return
}

// getPrimaryURLAndSI is a helper function to return primary proxy's URL and daemon info
// if Smap is not yet synced, use the primary proxy from the config
// smap lock is acquired to avoid race between this function and other smap access (for example,
// receiving smap during metasync)
func (h *httprunner) getPrimaryURLAndSI() (url string, proxysi *cluster.Snode) {
	config := cmn.GCO.Get()
	smap := h.smapowner.get()
	if smap == nil || smap.ProxySI == nil {
		url, proxysi = config.Proxy.PrimaryURL, nil
		return
	}
	if smap.ProxySI.DaemonID != "" {
		url, proxysi = smap.ProxySI.IntraControlNet.DirectURL, smap.ProxySI
		return
	}
	url, proxysi = config.Proxy.PrimaryURL, smap.ProxySI
	return
}

//
// StatsD client using 8125 (default) StatsD port - https://github.com/etsy/statsd
//
func (h *httprunner) initStatsD(daemonStr string) (err error) {
	suffix := strings.Replace(h.si.DaemonID, ":", "_", -1)
	h.statsdC, err = statsd.New("localhost", 8125, daemonStr+"."+suffix)
	if err != nil {
		glog.Infof("Failed to connect to StatsD daemon: %v", err)
	}
	return
}

func isReplicationPUT(r *http.Request) (isreplica bool, replicasrc string) {
	replicasrc = r.Header.Get(cmn.HeaderObjReplicSrc)
	return replicasrc != "", replicasrc
}

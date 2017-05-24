/*
 * Copyright (C) 2017 Dgraph Labs, Inc. and Contributors
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package main

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/gob"
	"encoding/json"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"path"
	"regexp"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/context"
	"golang.org/x/net/trace"
	"google.golang.org/grpc"

	"github.com/dgraph-io/dgraph/gql"
	"github.com/dgraph-io/dgraph/group"
	"github.com/dgraph-io/dgraph/mutation"
	"github.com/dgraph-io/dgraph/posting"
	"github.com/dgraph-io/dgraph/protos"
	"github.com/dgraph-io/dgraph/query"
	"github.com/dgraph-io/dgraph/rdf"
	"github.com/dgraph-io/dgraph/schema"
	"github.com/dgraph-io/dgraph/store"
	"github.com/dgraph-io/dgraph/tok"
	"github.com/dgraph-io/dgraph/types"
	"github.com/dgraph-io/dgraph/worker"
	"github.com/dgraph-io/dgraph/x"
	"github.com/pkg/errors"
	"github.com/soheilhy/cmux"
)

var (
	gconf      = flag.String("group_conf", "", "group configuration file")
	postingDir = flag.String("p", "p", "Directory to store posting lists.")
	walDir     = flag.String("w", "w", "Directory to store raft write-ahead logs.")
	port       = flag.Int("port", 8080, "Port to run server on.")
	bindall    = flag.Bool("bindall", false,
		"Use 0.0.0.0 instead of localhost to bind to all addresses on local machine.")
	nomutations    = flag.Bool("nomutations", false, "Don't allow mutations on this server.")
	tracing        = flag.Float64("trace", 0.0, "The ratio of queries to trace.")
	cpuprofile     = flag.String("cpu", "", "write cpu profile to file")
	memprofile     = flag.String("mem", "", "write memory profile to file")
	dumpSubgraph   = flag.String("dumpsg", "", "Directory to save subgraph for testing, debugging")
	finishCh       = make(chan struct{}) // channel to wait for all pending reqs to finish.
	shutdownCh     = make(chan struct{}) // channel to signal shutdown.
	pendingQueries = make(chan struct{}, 10000*runtime.NumCPU())
	// TLS configurations
	tlsEnabled       = flag.Bool("tls.on", false, "Use TLS connections with clients.")
	tlsCert          = flag.String("tls.cert", "", "Certificate file path.")
	tlsKey           = flag.String("tls.cert_key", "", "Certificate key file path.")
	tlsKeyPass       = flag.String("tls.cert_key_passphrase", "", "Certificate key passphrase.")
	tlsClientAuth    = flag.String("tls.client_auth", "", "Enable TLS client authentication")
	tlsClientCACerts = flag.String("tls.ca_certs", "", "CA Certs file path.")
	tlsSystemCACerts = flag.Bool("tls.use_system_ca", false, "Include System CA into CA Certs.")
	tlsMinVersion    = flag.String("tls.min_version", "TLS11", "TLS min version.")
	tlsMaxVersion    = flag.String("tls.max_version", "TLS12", "TLS max version.")
)

var mutationNotAllowedErr = x.Errorf("Mutations are forbidden on this server.")

func stopProfiling() {
	// Stop the CPU profiling that was initiated.
	if len(*cpuprofile) > 0 {
		pprof.StopCPUProfile()
	}

	// Write memory profile before exit.
	if len(*memprofile) > 0 {
		f, err := os.Create(*memprofile)
		if err != nil {
			log.Println(err)
		}
		pprof.WriteHeapProfile(f)
		f.Close()
	}
}

func addCorsHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers",
		"Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token,"+
			"X-Auth-Token, Cache-Control, X-Requested-With")
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Connection", "close")
}

func isMutationAllowed(ctx context.Context) bool {
	if !*nomutations {
		return true
	}
	shareAllowed, ok := ctx.Value("_share_").(bool)
	if !ok || !shareAllowed {
		return false
	}
	return true
}

func enrichSchema(updates []*protos.SchemaUpdate) error {
	for _, schema := range updates {
		typ := types.TypeID(schema.ValueType)
		if typ == types.UidID {
			continue
		}
		if len(schema.Tokenizer) == 0 && schema.Directive == protos.SchemaUpdate_INDEX {
			schema.Tokenizer = []string{tok.Default(typ).Name()}
		} else if len(schema.Tokenizer) > 0 && schema.Directive != protos.SchemaUpdate_INDEX {
			return x.Errorf("Tokenizers present without indexing on attr %s", schema.Predicate)
		}
		// check for valid tokeniser types and duplicates
		var seen = make(map[string]bool)
		var seenSortableTok bool
		for _, t := range schema.Tokenizer {
			tokenizer, has := tok.GetTokenizer(t)
			if !has {
				return x.Errorf("Invalid tokenizer %s", t)
			}
			if tokenizer.Type() != typ {
				return x.Errorf("Tokenizer: %s isn't valid for predicate: %s of type: %s",
					tokenizer.Name(), schema.Predicate, typ.Name())
			}
			if _, ok := seen[tokenizer.Name()]; !ok {
				seen[tokenizer.Name()] = true
			} else {
				return x.Errorf("Duplicate tokenizers present for attr %s", schema.Predicate)
			}
			if tokenizer.IsSortable() {
				if seenSortableTok {
					return x.Errorf("More than one sortable index encountered for: %v",
						schema.Predicate)
				}
				seenSortableTok = true
			}
		}
	}
	return nil
}

// This function is used to run mutations for the requests received from the
// http client.
func healthCheck(w http.ResponseWriter, r *http.Request) {
	if worker.HealthCheck() {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("OK"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
}

// parseQueryAndMutation handles the cases where the query parsing code can hang indefinitely.
// We allow 1 second for parsing the query; and then give up.
func parseQueryAndMutation(ctx context.Context, r gql.Request) (res gql.Result, err error) {
	x.Trace(ctx, "Query received: %v", r.Str)
	errc := make(chan error, 1)

	go func() {
		var err error
		res, err = gql.Parse(r)
		errc <- err
	}()

	child, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()

	select {
	case <-child.Done():
		return res, child.Err()
	case err := <-errc:
		if err != nil {
			x.TraceError(ctx, x.Wrapf(err, "Error while parsing query"))
			return res, err
		}
		x.Trace(ctx, "Query parsed")
	}
	return res, nil
}

type executeResult struct {
	subgraphs   []*query.SubGraph
	schemaNode  []*protos.SchemaNode
	allocations map[string]uint64
}

type invalidRequestError struct {
	err error
}

func (e *invalidRequestError) Error() string {
	return "invalid request: " + e.err.Error()
}

type internalError struct {
	err error
}

func (e *internalError) Error() string {
	return "internal error: " + e.err.Error()
}

func executeQuery(ctx context.Context, res gql.Result, l *query.Latency) (executeResult, error) {
	var er executeResult
	var err error
	// If we have mutations that don't depend on query, run them first.
	var vars map[string]query.VarValue
	var m protos.Mutations

	var depSet, indepSet, depDel, indepDel gql.NQuads
	if res.Mutation != nil {
		if res.Mutation.HasOps() && !isMutationAllowed(ctx) {
			return er, x.Wrap(&invalidRequestError{err: mutationNotAllowedErr})
		}

		if len(res.Mutation.Schema) > 0 {
			if m.Schema, err = schema.Parse(res.Mutation.Schema); err != nil {
				return er, x.Wrapf(&invalidRequestError{err: err}, "failed to parse schema")
			}
			if err = enrichSchema(m.Schema); err != nil {
				return er, x.Wrapf(&internalError{err: err}, "failed to parse schema")
			}
		}

		depSet, indepSet = gql.WrapNQ(res.Mutation.Set, protos.DirectedEdge_SET).
			Partition(gql.IsDependent)
		depDel, indepDel = gql.WrapNQ(res.Mutation.Del, protos.DirectedEdge_DEL).
			Partition(gql.IsDependent)

		nquads := indepSet.Add(indepDel)
		if !nquads.IsEmpty() {
			var mr mutation.MaterializedMutation
			if mr, err = mutation.Materialize(ctx, nquads, vars); err != nil {
				return er, x.Wrapf(&internalError{err: err}, "failed to convert NQuads to edges")
			}
			m.Edges, er.allocations = mr.Edges, mr.NewUids
			for i := range m.Edges {
				m.Edges[i].Op = nquads.Types[i]
			}
		}
		if err = mutation.ApplyMutations(ctx, &m); err != nil {
			return er, x.Wrapf(&internalError{err: err}, "failed to apply mutations")
		}
	}

	if res.Schema != nil {
		if er.schemaNode, err = worker.GetSchemaOverNetwork(ctx, res.Schema); err != nil {
			return er, x.Wrapf(&internalError{err: err}, "error while fetching schema")
		}
	}

	if len(res.Query) == 0 {
		return er, nil
	}

	er.subgraphs, vars, err = query.ProcessQuery(ctx, res, l)
	if err != nil {
		return er, x.Wrap(&internalError{err: err})
	}

	nquads := depSet.Add(depDel)
	if !nquads.IsEmpty() {
		var mr mutation.MaterializedMutation
		if mr, err = mutation.Materialize(ctx, nquads, vars); err != nil {
			return er, x.Wrapf(&invalidRequestError{err: err}, "Failed to convert NQuads to edges")
		}
		if len(mr.NewUids) > 0 {
			return er, x.Wrapf(&invalidRequestError{err: err},
				"adding nodes when using variables is not allowed")
		}
		m := protos.Mutations{Edges: mr.Edges}
		if err := mutation.ApplyMutations(ctx, &m); err != nil {
			return er, x.Wrapf(&internalError{err: err}, "Failed to apply mutations with variables")
		}
	}
	return er, nil
}

func queryHandler(w http.ResponseWriter, r *http.Request) {
	if !worker.HealthCheck() {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	// Add a limit on how many pending queries can be run in the system.
	pendingQueries <- struct{}{}
	defer func() { <-pendingQueries }()

	addCorsHeaders(w)
	if r.Method == "OPTIONS" {
		return
	}
	if r.Method != "POST" {
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return
	}

	// Lets add the value of the debug query parameter to the context.
	ctx := context.WithValue(context.Background(), "debug", r.URL.Query().Get("debug"))

	if rand.Float64() < *tracing {
		tr := trace.New("Dgraph", "Query")
		defer tr.Finish()
		ctx = trace.NewContext(ctx, tr)
	}

	invalidRequest := func(err error, msg string) {
		x.TraceError(ctx, err)
		x.SetStatus(w, x.ErrorInvalidRequest, "Invalid request encountered.")
	}

	var l query.Latency
	l.Start = time.Now()
	defer r.Body.Close()
	req, err := ioutil.ReadAll(r.Body)
	q := string(req)
	if err != nil || len(q) == 0 {
		invalidRequest(err, "Error while reading query")
		return
	}

	res, err := parseQueryAndMutation(ctx, gql.Request{
		Str:       q,
		Variables: map[string]string{},
		Http:      true,
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// set timeout if schema mutation not present
	if res.Mutation == nil || len(res.Mutation.Schema) == 0 {
		// If schema mutation is not present
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Minute)
		defer cancel()
	}

	var er executeResult
	if er, err = executeQuery(ctx, res, &l); err != nil {
		switch errors.Cause(err).(type) {
		case *invalidRequestError:
			invalidRequest(err, err.Error())
		default: // internalError or other
			x.TraceError(ctx, x.Wrap(err))
			x.SetStatus(w, x.Error, err.Error())
		}
		return
	}

	newUids := convertUidsToHex(er.allocations)

	if len(res.Query) == 0 {
		mp := map[string]interface{}{}
		if res.Mutation != nil {
			mp["code"] = x.Success
			mp["message"] = "Done"
			mp["uids"] = newUids
		}
		// Either Schema or query can be specified
		if res.Schema != nil {
			mp["schema"] = er.schemaNode
		}
		if js, err := json.Marshal(mp); err == nil {
			w.Write(js)
		} else {
			x.TraceError(ctx, err)
			x.SetStatus(w, x.Error, "Internal error.")
			return
		}
		return
	}

	if len(*dumpSubgraph) > 0 {
		for _, sg := range er.subgraphs {
			x.Checkf(os.MkdirAll(*dumpSubgraph, 0700), *dumpSubgraph)
			s := time.Now().Format("20060102.150405.000000.gob")
			filename := path.Join(*dumpSubgraph, s)
			f, err := os.Create(filename)
			x.Checkf(err, filename)
			enc := gob.NewEncoder(f)
			x.Check(enc.Encode(sg))
			x.Checkf(f.Close(), filename)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	var addLatency bool
	// If there is an error parsing, then addLatency would remain false.
	addLatency, _ = strconv.ParseBool(r.URL.Query().Get("latency"))
	debug, _ := strconv.ParseBool(r.URL.Query().Get("debug"))
	addLatency = addLatency || debug
	err = query.ToJson(&l, er.subgraphs, w, newUids, addLatency)
	if err != nil {
		// since we performed w.Write in ToJson above,
		// calling WriteHeader with 500 code will be ignored.
		x.TraceError(ctx, x.Wrapf(err, "Error while converting to Json"))
		x.SetStatus(w, x.Error, err.Error())
		return
	}
	x.Trace(ctx, "Latencies: Total: %v Parsing: %v Process: %v Json: %v",
		time.Since(l.Start), l.Parsing, l.Processing, l.Json)
}

// convert the new UIDs to hex string.
func convertUidsToHex(m map[string]uint64) (res map[string]string) {
	res = make(map[string]string)
	for k, v := range m {
		res[k] = fmt.Sprintf("%#x", v)
	}
	return
}

// shareHandler allows to share a query between users.
func shareHandler(w http.ResponseWriter, r *http.Request) {
	var allocIds map[string]uint64

	w.Header().Set("Content-Type", "application/json")
	addCorsHeaders(w)
	if r.Method != "POST" {
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return
	}

	ctx := context.Background()

	defer r.Body.Close()
	rawQuery, err := ioutil.ReadAll(r.Body)
	if err != nil || len(rawQuery) == 0 {
		x.TraceError(ctx, x.Wrapf(err, "Error while reading the stringified query payload"))
		x.SetStatus(w, x.ErrorInvalidRequest, "Invalid request encountered.")
		return
	}

	// Generate mutation with query and hash
	queryHash := sha256.Sum256(rawQuery)
	mutationString := fmt.Sprintf("<_:share> <_share_> %q . \n <_:share> <_share_hash_> \"%x\" .",
		rawQuery, queryHash)
	mu := gql.Mutation{}
	if mu.Set, err = rdf.ConvertToNQuads(mutationString); err != nil {
		x.TraceError(ctx, err)
		x.SetStatus(w, x.Error, err.Error())
		return
	}

	if allocIds, err = mutation.ConvertAndApply(ctx, &protos.Mutation{Set: mu.Set}); err != nil {
		x.TraceError(ctx, x.Wrapf(err, "Error while handling mutations"))
		x.SetStatus(w, x.Error, err.Error())
		return
	}

	allocIdsStr := convertUidsToHex(allocIds)

	payload := map[string]interface{}{
		"code":    x.Success,
		"message": "Done",
		"uids":    allocIdsStr,
	}

	if res, err := json.Marshal(payload); err == nil {
		w.Write(res)
	} else {
		x.SetStatus(w, "Error", "Unable to marshal map")
	}
}

// storeStatsHandler outputs some basic stats for data store.
func storeStatsHandler(w http.ResponseWriter, r *http.Request) {
	addCorsHeaders(w)
	w.Header().Set("Content-Type", "text/html")
	w.Write([]byte("<pre>"))
	w.Write([]byte(worker.StoreStats()))
	w.Write([]byte("</pre>"))
}

// handlerInit does some standard checks. Returns false if something is wrong.
func handlerInit(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != "GET" {
		x.SetStatus(w, x.ErrorInvalidMethod, "Invalid method")
		return false
	}

	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil || !net.ParseIP(ip).IsLoopback() {
		x.SetStatus(w, x.ErrorUnauthorized, fmt.Sprintf("Request from IP: %v", ip))
		return false
	}
	return true
}

func shutDownHandler(w http.ResponseWriter, r *http.Request) {
	if !handlerInit(w, r) {
		return
	}

	shutdownServer()
	x.SetStatus(w, x.Success, "Server is shutting down")
}

func shutdownServer() {
	x.Printf("Got clean exit request")
	stopProfiling()          // stop profiling
	shutdownCh <- struct{}{} // exit grpc and http servers.

	// wait for grpc and http servers to finish pending reqs and
	// then stop all nodes, internal grpc servers and sync all the marks
	go func() {
		defer func() { shutdownCh <- struct{}{} }()

		// wait for grpc, http and http2 servers to stop
		<-finishCh
		<-finishCh
		<-finishCh

		worker.BlockingStop()
	}()
}

func backupHandler(w http.ResponseWriter, r *http.Request) {
	if !handlerInit(w, r) {
		return
	}
	ctx := context.Background()
	if err := worker.BackupOverNetwork(ctx); err != nil {
		x.SetStatus(w, err.Error(), "Backup failed.")
		return
	}
	x.SetStatus(w, x.Success, "Backup completed.")
}

func hasGraphOps(mu *protos.Mutation) bool {
	return len(mu.Set) > 0 || len(mu.Del) > 0 || len(mu.Schema) > 0
}

// server is used to implement protos.DgraphServer
type grpcServer struct{}

// This method is used to execute the query and return the response to the
// client as a protocol buffer message.
func (s *grpcServer) Run(ctx context.Context,
	req *protos.Request) (resp *protos.Response, err error) {
	// we need membership information
	if !worker.HealthCheck() {
		x.Trace(ctx, "This server hasn't yet been fully initiated. Please retry later.")
		return resp, x.Errorf("Uninitiated server. Please retry later")
	}
	if rand.Float64() < *tracing {
		tr := trace.New("Dgraph", "GrpcQuery")
		defer tr.Finish()
		ctx = trace.NewContext(ctx, tr)
	}

	// Sanitize the context of the keys used for internal purposes only
	ctx = context.WithValue(ctx, "_share_", nil)

	resp = new(protos.Response)
	if len(req.Query) == 0 && req.Mutation == nil {
		x.TraceError(ctx, x.Errorf("Empty query and mutation."))
		return resp, fmt.Errorf("Empty query and mutation.")
	}

	var l query.Latency
	l.Start = time.Now()
	x.Trace(ctx, "Query received: %v, variables: %v", req.Query, req.Vars)
	res, err := parseQueryAndMutation(ctx, gql.Request{
		Str:       req.Query,
		Variables: req.Vars,
		Http:      false,
	})
	if err != nil {
		return resp, err
	}

	if req.Schema != nil && res.Schema != nil {
		return resp, x.Errorf("Multiple schema blocks found")
	}
	// Schema Block can be part of query string or request
	if res.Schema == nil {
		res.Schema = req.Schema
	}

	var er executeResult
	if er, err = executeQuery(ctx, res, &l); err != nil {
		x.TraceError(ctx, err)
		return resp, x.Wrap(err)
	}
	resp.AssignedUids = er.allocations
	resp.Schema = er.schemaNode

	nodes, err := query.ToProtocolBuf(&l, er.subgraphs)
	if err != nil {
		x.TraceError(ctx, x.Wrapf(err, "Error while converting to ProtocolBuffer"))
		return resp, err
	}
	resp.N = nodes

	gl := new(protos.Latency)
	gl.Parsing, gl.Processing, gl.Pb = l.Parsing.String(), l.Processing.String(),
		l.ProtocolBuffer.String()
	resp.L = gl
	return resp, err
}

func (s *grpcServer) CheckVersion(ctx context.Context, c *protos.Check) (v *protos.Version,
	err error) {
	// we need membership information
	if !worker.HealthCheck() {
		x.Trace(ctx, "This server hasn't yet been fully initiated. Please retry later.")
		return v, x.Errorf("Uninitiated server. Please retry later")
	}

	v = new(protos.Version)
	v.Tag = x.Version()
	return v, nil
}

var uiDir string

func init() {
	// uiDir can also be set through -ldflags while doing a release build. In that
	// case it points to usr/local/share/dgraph/assets where we store assets for
	// the user. In other cases, it should point to the build directory within the repository.
	flag.StringVar(&uiDir, "ui", uiDir, "Directory which contains assets for the user interface")
	if uiDir == "" {
		uiDir = os.Getenv("GOPATH") + "/src/github.com/dgraph-io/dgraph/dashboard/build"
	}
}

func checkFlagsAndInitDirs() {
	if len(*cpuprofile) > 0 {
		f, err := os.Create(*cpuprofile)
		x.Check(err)
		pprof.StartCPUProfile(f)
	}

	// Create parent directories for postings, uids and mutations
	x.Check(os.MkdirAll(*postingDir, 0700))
}

func setupListener(addr string, port int) (listener net.Listener, err error) {
	var reload func()
	laddr := fmt.Sprintf("%s:%d", addr, port)
	if !*tlsEnabled {
		listener, err = net.Listen("tcp", laddr)
	} else {
		var tlsCfg *tls.Config
		tlsCfg, reload, err = x.GenerateTLSConfig(x.TLSHelperConfig{
			ConfigType:             x.TLSServerConfig,
			CertRequired:           *tlsEnabled,
			Cert:                   *tlsCert,
			Key:                    *tlsKey,
			KeyPassphrase:          *tlsKeyPass,
			ClientAuth:             *tlsClientAuth,
			ClientCACerts:          *tlsClientCACerts,
			UseSystemClientCACerts: *tlsSystemCACerts,
			MinVersion:             *tlsMinVersion,
			MaxVersion:             *tlsMaxVersion,
		})
		if err != nil {
			return nil, err
		}
		listener, err = tls.Listen("tcp", laddr, tlsCfg)
	}
	go func() {
		sigChan := make(chan os.Signal, 1)
		signal.Notify(sigChan, syscall.SIGHUP)
		for range sigChan {
			log.Println("SIGHUP signal received")
			if reload != nil {
				reload()
				log.Println("TLS certificates and CAs reloaded")
			}
		}
	}()
	return listener, err
}

func serveGRPC(l net.Listener) {
	defer func() { finishCh <- struct{}{} }()
	s := grpc.NewServer(grpc.CustomCodec(&query.Codec{}))
	protos.RegisterDgraphServer(s, &grpcServer{})
	err := s.Serve(l)
	log.Printf("gRpc server stopped : %s", err.Error())
	s.GracefulStop()
}

func serveHTTP(l net.Listener) {
	defer func() { finishCh <- struct{}{} }()
	srv := &http.Server{
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 600 * time.Second,
		IdleTimeout:  2 * time.Minute,
	}

	err := srv.Serve(l)
	log.Printf("Stopped taking more http(s) requests. Err: %s", err.Error())
	ctx, cancel := context.WithTimeout(context.Background(), 630*time.Second)
	defer cancel()
	err = srv.Shutdown(ctx)
	log.Printf("All http(s) requests finished.")
	if err != nil {
		log.Printf("Http(s) shutdown err: %v", err.Error())
	}
}

func setupServer(che chan error) {
	go worker.RunServer(*bindall) // For internal communication.

	laddr := "localhost"
	if *bindall {
		laddr = "0.0.0.0"
	}

	l, err := setupListener(laddr, *port)
	if err != nil {
		log.Fatal(err)
	}

	tcpm := cmux.New(l)
	httpl := tcpm.Match(cmux.HTTP1Fast())
	grpcl := tcpm.MatchWithWriters(
		cmux.HTTP2MatchHeaderFieldSendSettings("content-type", "application/grpc"))
	http2 := tcpm.Match(cmux.HTTP2())

	http.HandleFunc("/health", healthCheck)
	http.HandleFunc("/query", queryHandler)
	http.HandleFunc("/share", shareHandler)
	http.HandleFunc("/debug/store", storeStatsHandler)
	http.HandleFunc("/admin/shutdown", shutDownHandler)
	http.HandleFunc("/admin/backup", backupHandler)

	// UI related API's.
	// Share urls have a hex string as the shareId. So if
	// our url path matches it, we wan't to serve index.html.
	reg := regexp.MustCompile(`\/0[xX][0-9a-fA-F]+`)
	http.Handle("/", homeHandler(http.FileServer(http.Dir(uiDir)), reg))
	http.HandleFunc("/ui/keywords", keywordHandler)

	// Initilize the servers.
	go serveGRPC(grpcl)
	go serveHTTP(httpl)
	go serveHTTP(http2)

	go func() {
		<-shutdownCh
		// Stops grpc/http servers; Already accepted connections are not closed.
		l.Close()
	}()

	log.Println("grpc server started.")
	log.Println("http server started.")
	log.Println("Server listening on port", *port)

	err = tcpm.Serve() // Start cmux serving. blocking call
	<-shutdownCh       // wait for shutdownServer to finish
	che <- err         // final close for main.
}

func main() {
	rand.Seed(time.Now().UnixNano())
	x.Init()
	checkFlagsAndInitDirs()

	// All the writes to posting store should be synchronous. We use batched writers
	// for posting lists, so the cost of sync writes is amortized.
	ps, err := store.NewSyncStore(*postingDir)
	x.Checkf(err, "Error initializing postings store")
	defer ps.Close()

	x.Check(group.ParseGroupConfig(*gconf))
	schema.Init(ps)

	// Posting will initialize index which requires schema. Hence, initialize
	// schema before calling posting.Init().
	posting.Init(ps)
	worker.Init(ps)

	// setup shutdown os signal handler
	sdCh := make(chan os.Signal, 1)
	defer close(sdCh)
	// sigint : Ctrl-C, sigquit : Ctrl-\ (backslash), sigterm : kill command.
	signal.Notify(sdCh, os.Interrupt, syscall.SIGINT, syscall.SIGQUIT, syscall.SIGTERM)
	go func() {
		_, ok := <-sdCh
		if ok {
			shutdownServer()
		}
	}()

	// Setup external communication.
	che := make(chan error, 1)
	go setupServer(che)
	go worker.StartRaftNodes(*walDir)

	if err := <-che; !strings.Contains(err.Error(),
		"use of closed network connection") {
		log.Fatal(err)
	}
}

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/collab"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/domain/permission"
	"github.com/jiawei-wang-dev/go-realtime-collab-editor/internal/protocol"
	webtransportgo "github.com/quic-go/webtransport-go"
)

type config struct {
	apiURL             string
	endpoints          []string
	caFile             string
	serverName         string
	origin             string
	tlsConfig          *tls.Config
	httpClient         *http.Client
	clients            int
	documents          int
	prepareConcurrency int
	prepareRetries     int
	ramp               time.Duration
	online             time.Duration
	editInterval       time.Duration
	editCount          int
	editRetries        int
	connectRetries     int
	password           string
	prefix             string
	resumeCount        int
	resumeDelay        time.Duration
	resumeSettle       time.Duration
}

type report struct {
	Clients                       int               `json:"clients"`
	Documents                     int               `json:"documents"`
	PreparationConcurrency        int               `json:"preparation_concurrency"`
	PreparationRetries            int               `json:"preparation_retries"`
	PreparationSeconds            float64           `json:"preparation_seconds"`
	JoinSuccess                   int64             `json:"join_success"`
	JoinFailures                  int64             `json:"join_failures"`
	JoinRetries                   int64             `json:"join_retries"`
	ResumeAttempts                int64             `json:"resume_attempts"`
	ResumeSuccess                 int64             `json:"resume_success"`
	ResumeFailures                int64             `json:"resume_failures"`
	ResyncCount                   int64             `json:"resync_count"`
	ResumeErrorSamples            map[string]int    `json:"resume_error_samples,omitempty"`
	JoinErrorSamples              map[string]int    `json:"join_error_samples,omitempty"`
	Redirects                     int64             `json:"redirects"`
	Edits                         int64             `json:"edits"`
	EditFailures                  int64             `json:"edit_failures"`
	EditRetries                   int               `json:"edit_retries"`
	EditErrorSamples              map[string]int    `json:"edit_error_samples,omitempty"`
	RemoteUpdates                 int64             `json:"remote_updates"`
	MaxObservedVersion            uint64            `json:"max_observed_version"`
	ConvergenceOK                 bool              `json:"convergence_ok"`
	ConvergenceErrors             []string          `json:"convergence_errors,omitempty"`
	FinalVersions                 map[string]uint64 `json:"final_versions,omitempty"`
	FinalContents                 map[string]string `json:"final_contents,omitempty"`
	UnexpectedDisconnects         int64             `json:"unexpected_disconnects"`
	LogicalSessions               int               `json:"logical_sessions"`
	TransportInstances            int               `json:"transport_instances"`
	UnderlyingConnectionsMeasured bool              `json:"underlying_connections_measured"`
	JoinP50Ms                     float64           `json:"join_p50_ms"`
	JoinP95Ms                     float64           `json:"join_p95_ms"`
	EditAckP50Ms                  float64           `json:"edit_ack_p50_ms"`
	EditAckP95Ms                  float64           `json:"edit_ack_p95_ms"`
	ElapsedSeconds                float64           `json:"elapsed_seconds"`
}

type session struct {
	transport    *webtransportgo.Transport
	session      *webtransportgo.Session
	stream       *webtransportgo.Stream
	token        string
	docID        string
	version      atomic.Uint64
	resumeResync atomic.Bool
	contentMu    sync.Mutex
	content      string
	acks         sync.Map
	done         chan struct{}
	closing      atomic.Bool
	err          atomic.Value
}

type pendingAck struct {
	done chan struct{}
	err  chan error
}

type convergenceTracker struct {
	mu         sync.Mutex
	operations map[string]map[uint64]collab.Operation
}

func newConvergenceTracker(documents []string) *convergenceTracker {
	operations := make(map[string]map[uint64]collab.Operation, len(documents))
	for _, documentID := range documents {
		operations[documentID] = make(map[uint64]collab.Operation)
	}
	return &convergenceTracker{operations: operations}
}

func (tracker *convergenceTracker) record(message protocol.Message) {
	if message.Type != protocol.TypeEditApplied || message.Operation == nil {
		return
	}
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	byVersion, ok := tracker.operations[message.DocID]
	if !ok {
		byVersion = make(map[uint64]collab.Operation)
		tracker.operations[message.DocID] = byVersion
	}
	if _, exists := byVersion[message.Version]; !exists {
		byVersion[message.Version] = *message.Operation
	}
}

func (tracker *convergenceTracker) snapshot() (bool, map[string]uint64, map[string]string, []string) {
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	versions := make(map[string]uint64, len(tracker.operations))
	contents := make(map[string]string, len(tracker.operations))
	var problems []string
	for documentID, byVersion := range tracker.operations {
		content := ""
		var version uint64
		for next := uint64(1); ; next++ {
			op, ok := byVersion[next]
			if !ok {
				break
			}
			var err error
			content, err = collab.Apply(content, op)
			if err != nil {
				problems = append(problems, fmt.Sprintf("%s version %d: %v", documentID, next, err))
				break
			}
			version = next
		}
		versions[documentID] = version
		contents[documentID] = content
		if version != uint64(len(byVersion)) {
			problems = append(problems, fmt.Sprintf("%s has non-contiguous versions: applied=%d observed=%d", documentID, len(byVersion), version))
		}
	}
	return len(problems) == 0, versions, contents, problems
}

func retryPreparation(ctx context.Context, retries int, work func() error) error {
	for attempt := 0; ; attempt++ {
		err := work()
		if err == nil || attempt >= retries {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 250 * time.Millisecond):
		}
	}
}

func runIndexed(ctx context.Context, count, concurrency int, work func(int) error) error {
	if count == 0 {
		return nil
	}
	if concurrency < 1 {
		return errors.New("preparation concurrency must be positive")
	}
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	var workers sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for worker := 0; worker < concurrency; worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for {
				select {
				case <-workCtx.Done():
					return
				case index, ok := <-jobs:
					if !ok {
						return
					}
					if err := work(index); err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = err
							cancel()
						}
						errMu.Unlock()
					}
				}
			}
		}()
	}
	for index := 0; index < count; index++ {
		select {
		case <-workCtx.Done():
			break
		case jobs <- index:
		}
		if workCtx.Err() != nil {
			break
		}
	}
	close(jobs)
	workers.Wait()
	return firstErr
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := parseConfig()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), cfg.online+cfg.ramp+30*time.Minute)
	defer cancel()

	prepStart := time.Now()
	ownerToken, err := registerAndLogin(ctx, cfg.httpClient, cfg.apiURL, cfg.prefix+"-owner", cfg.password)
	if err != nil {
		return fmt.Errorf("register owner: %w", err)
	}
	docs := make([]string, cfg.documents)
	for i := range docs {
		docs[i] = fmt.Sprintf("wt-load-%s-%d", randomID(), i)
	}
	if err := runIndexed(ctx, len(docs), cfg.prepareConcurrency, func(i int) error {
		if err := retryPreparation(ctx, cfg.prepareRetries, func() error {
			return createDocument(ctx, cfg.httpClient, cfg.apiURL, ownerToken, docs[i])
		}); err != nil {
			return fmt.Errorf("create document %s: %w", docs[i], err)
		}
		return nil
	}); err != nil {
		return err
	}
	users := make([]string, cfg.clients)
	tokens := make([]string, cfg.clients)
	for i := range users {
		users[i] = fmt.Sprintf("%s-user-%d-%s", cfg.prefix, i, randomID())
	}
	if err := runIndexed(ctx, len(users), cfg.prepareConcurrency, func(i int) error {
		var err error
		err = retryPreparation(ctx, cfg.prepareRetries, func() error {
			tokens[i], err = registerAndLogin(ctx, cfg.httpClient, cfg.apiURL, users[i], cfg.password)
			return err
		})
		if err != nil {
			return fmt.Errorf("register user %d: %w", i, err)
		}
		if err := retryPreparation(ctx, cfg.prepareRetries, func() error {
			return grantWrite(ctx, cfg.httpClient, cfg.apiURL, ownerToken, tokens[i], docs[i%len(docs)])
		}); err != nil {
			return fmt.Errorf("grant user %d: %w", i, err)
		}
		return nil
	}); err != nil {
		return err
	}
	prepSeconds := time.Since(prepStart).Seconds()

	var joinLatencies []time.Duration
	var editLatencies []time.Duration
	var joinMu, editMu, joinErrorMu, editErrorMu sync.Mutex
	joinErrorSamples := make(map[string]int)
	resumeErrorSamples := make(map[string]int)
	editErrorSamples := make(map[string]int)
	var joins, joinFailures, joinRetries, redirects, edits, editFailures, remoteUpdates, disconnects atomic.Int64
	var resumeAttempts, resumeSuccess, resumeFailures, resyncCount atomic.Int64
	var maxObservedVersion atomic.Uint64
	tracker := newConvergenceTracker(docs)
	sessions := make([]*session, 0, cfg.clients)
	for i := range users {
		started := time.Now()
		current, count, retries, err := connectWithRetry(ctx, cfg, tokens[i], docs[i%len(docs)])
		joinRetries.Add(int64(retries))
		if err != nil {
			joinFailures.Add(1)
			joinErrorMu.Lock()
			if len(joinErrorSamples) < 5 {
				joinErrorSamples[err.Error()]++
			}
			joinErrorMu.Unlock()
			continue
		}
		joinMu.Lock()
		joinLatencies = append(joinLatencies, time.Since(started))
		joinMu.Unlock()
		joins.Add(1)
		redirects.Add(int64(count))
		sessions = append(sessions, current)
		go current.receive(&disconnects, &remoteUpdates, &maxObservedVersion, tracker)
		if cfg.ramp > 0 && i+1 < cfg.clients {
			timer := time.NewTimer(cfg.ramp / time.Duration(cfg.clients))
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
		}
	}

	if len(sessions) != cfg.clients {
		report := report{Clients: cfg.clients, Documents: cfg.documents, PreparationConcurrency: cfg.prepareConcurrency, PreparationRetries: cfg.prepareRetries, PreparationSeconds: prepSeconds, EditRetries: cfg.editRetries, EditErrorSamples: editErrorSamples, JoinSuccess: joins.Load(), JoinFailures: joinFailures.Load(), JoinRetries: joinRetries.Load(), ResumeErrorSamples: resumeErrorSamples, JoinErrorSamples: joinErrorSamples, Redirects: redirects.Load(), LogicalSessions: len(sessions), TransportInstances: len(sessions), UnderlyingConnectionsMeasured: false}
		closeSessions(sessions)
		return emitReport(report, fmt.Errorf("join incomplete: success=%d target=%d", len(sessions), cfg.clients))
	}
	if cfg.resumeCount > 0 && cfg.resumeDelay > 0 {
		select {
		case <-ctx.Done():
			closeSessions(sessions)
			return ctx.Err()
		case <-time.After(cfg.resumeDelay):
		}
	}
	for i := 0; i < cfg.resumeCount; i++ {
		resumeAttempts.Add(1)
		old := sessions[i]
		lastVersion := old.version.Load()
		old.contentMu.Lock()
		lastContent := old.content
		old.contentMu.Unlock()
		old.close()
		select {
		case <-old.done:
		case <-ctx.Done():
			resumeFailures.Add(1)
			continue
		}
		resumed, resumeRedirects, resynced, _, err := resumeWithRetry(ctx, cfg, old.token, old.docID, lastVersion, lastContent)
		if err != nil {
			resumeFailures.Add(1)
			if len(resumeErrorSamples) < 5 {
				resumeErrorSamples[err.Error()]++
			}
			continue
		}
		if resynced {
			resyncCount.Add(1)
		}
		if resumeRedirects > 0 && len(cfg.endpoints) > 1 {
			cfg.endpoints = append(append([]string(nil), cfg.endpoints[1:]...), cfg.endpoints[0])
		}
		resumeSuccess.Add(1)
		sessions[i] = resumed
		go resumed.receive(&disconnects, &remoteUpdates, &maxObservedVersion, tracker)
	}

	report := report{Clients: cfg.clients, Documents: cfg.documents, PreparationConcurrency: cfg.prepareConcurrency, PreparationRetries: cfg.prepareRetries, PreparationSeconds: prepSeconds, EditRetries: cfg.editRetries, EditErrorSamples: editErrorSamples, JoinSuccess: joins.Load(), JoinFailures: joinFailures.Load(), JoinRetries: joinRetries.Load(), ResumeAttempts: resumeAttempts.Load(), ResumeSuccess: resumeSuccess.Load(), ResumeFailures: resumeFailures.Load(), ResyncCount: resyncCount.Load(), ResumeErrorSamples: resumeErrorSamples, JoinErrorSamples: joinErrorSamples, Redirects: redirects.Load(), LogicalSessions: len(sessions), TransportInstances: len(sessions), UnderlyingConnectionsMeasured: false}
	if resumeFailures.Load() > 0 {
		closeSessions(sessions)
		return emitReport(report, fmt.Errorf("resume incomplete: success=%d target=%d", resumeSuccess.Load(), resumeAttempts.Load()))
	}
	if cfg.resumeSettle > 0 {
		select {
		case <-ctx.Done():
			closeSessions(sessions)
			return ctx.Err()
		case <-time.After(cfg.resumeSettle):
		}
	}

	start := time.Now()
	editCounts := make([]int, len(sessions))
	deadline := time.NewTimer(cfg.online)
	defer deadline.Stop()
	for {
		select {
		case <-ctx.Done():
			closeSessions(sessions)
			return ctx.Err()
		case <-deadline.C:
			convergenceOK, finalVersions, finalContents, convergenceErrors := tracker.snapshot()
			closeSessions(sessions)
			report.ConvergenceOK = convergenceOK
			report.ConvergenceErrors = convergenceErrors
			report.FinalVersions = finalVersions
			report.FinalContents = finalContents
			report.JoinP50Ms, report.JoinP95Ms = percentiles(joinLatencies)
			report.EditAckP50Ms, report.EditAckP95Ms = percentiles(editLatencies)
			report.Edits = edits.Load()
			report.EditFailures = editFailures.Load()
			report.RemoteUpdates = remoteUpdates.Load()
			report.MaxObservedVersion = maxObservedVersion.Load()
			report.UnexpectedDisconnects = disconnects.Load()
			report.ElapsedSeconds = time.Since(start).Seconds()
			if err := emitReport(report, nil); err != nil {
				return err
			}
			return nil
		default:
			for i, current := range sessions {
				if cfg.editCount == 0 || current == nil || editCounts[i] >= cfg.editCount {
					continue
				}
				if time.Since(start) < cfg.editInterval*time.Duration(editCounts[i]) {
					continue
				}
				requestID := fmt.Sprintf("%s-edit-%d-%d", cfg.prefix, i, editCounts[i])
				editCounts[i]++
				operationStarted := time.Now()
				op := collab.Operation{Kind: collab.OperationInsert, Position: 0, Text: "x"}
				succeeded := false
				for attempt := 0; attempt <= cfg.editRetries; attempt++ {
					ack := &pendingAck{done: make(chan struct{}), err: make(chan error, 1)}
					current.acks.Store(requestID, ack)
					base := current.version.Load()
					if err := current.write(protocol.Message{Type: protocol.TypeEdit, DocID: current.docID, RequestID: requestID, BaseVersion: base, Operation: &op}); err != nil {
						current.acks.Delete(requestID)
						continue
					}
					select {
					case <-ack.done:
						editMu.Lock()
						editLatencies = append(editLatencies, time.Since(operationStarted))
						editMu.Unlock()
						edits.Add(1)
						succeeded = true
					case editErr := <-ack.err:
						editErrorMu.Lock()
						if len(editErrorSamples) < 5 {
							editErrorSamples[editErr.Error()]++
						}
						editErrorMu.Unlock()
						current.acks.Delete(requestID)
						time.Sleep(250 * time.Millisecond)
					case <-time.After(10 * time.Second):
						current.acks.Delete(requestID)
					}
					if succeeded {
						break
					}
				}
				if !succeeded {
					editFailures.Add(1)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
}

func parseConfig() (config, error) {
	var cfg config
	flag.StringVar(&cfg.apiURL, "api", os.Getenv("WT_LOAD_API_URL"), "REST API origin")
	endpoints := flag.String("endpoints", os.Getenv("WT_LOAD_ENDPOINTS"), "comma-separated WebTransport URLs")
	flag.StringVar(&cfg.caFile, "ca", os.Getenv("WT_LOAD_CA_FILE"), "CA PEM path")
	flag.StringVar(&cfg.serverName, "server-name", os.Getenv("WT_LOAD_SERVER_NAME"), "optional TLS server name override")
	flag.StringVar(&cfg.origin, "origin", envOr("WT_LOAD_ORIGIN", "https://localhost"), "WebTransport Origin header")
	flag.IntVar(&cfg.clients, "clients", intEnv("WT_LOAD_CLIENTS", 50), "logical sessions")
	flag.IntVar(&cfg.documents, "documents", intEnv("WT_LOAD_DOCUMENTS", 200), "documents")
	flag.IntVar(&cfg.prepareConcurrency, "prepare-concurrency", intEnv("WT_LOAD_PREPARE_CONCURRENCY", 4), "bounded REST preparation concurrency")
	flag.IntVar(&cfg.prepareRetries, "prepare-retries", intEnv("WT_LOAD_PREPARE_RETRIES", 1), "retries for transient REST preparation failures")
	flag.DurationVar(&cfg.ramp, "ramp", durationEnv("WT_LOAD_RAMP", 0), "ramp duration")
	flag.DurationVar(&cfg.online, "online", durationEnv("WT_LOAD_ONLINE", time.Minute), "online duration")
	flag.DurationVar(&cfg.editInterval, "edit-interval", durationEnv("WT_LOAD_EDIT_INTERVAL", 0), "per-session edit interval")
	flag.IntVar(&cfg.editCount, "edit-count", intEnv("WT_LOAD_EDIT_COUNT", 0), "edits per session")
	flag.IntVar(&cfg.editRetries, "edit-retries", intEnv("WT_LOAD_EDIT_RETRIES", 0), "retries for transient edit failures")
	flag.IntVar(&cfg.connectRetries, "connect-retries", intEnv("WT_LOAD_CONNECT_RETRIES", 0), "retries after WebTransport Dial failures")
	flag.StringVar(&cfg.password, "password", os.Getenv("WT_LOAD_PASSWORD"), "test password")
	flag.StringVar(&cfg.prefix, "prefix", envOr("WT_LOAD_PREFIX", "wt-load"), "account prefix")
	flag.IntVar(&cfg.resumeCount, "resume-count", intEnv("WT_LOAD_RESUME_COUNT", 0), "sessions to reconnect with RESUME before online edits")
	flag.DurationVar(&cfg.resumeDelay, "resume-delay", durationEnv("WT_LOAD_RESUME_DELAY", 0), "delay before planned RESUME reconnect")
	flag.DurationVar(&cfg.resumeSettle, "resume-settle", durationEnv("WT_LOAD_RESUME_SETTLE", 0), "delay after RESUME before online edits")
	flag.Parse()
	if cfg.apiURL == "" || *endpoints == "" || cfg.caFile == "" || cfg.password == "" {
		return cfg, errors.New("api, endpoints, ca and password are required")
	}
	if cfg.clients < 1 || cfg.documents < 1 || cfg.prepareConcurrency < 1 || cfg.prepareRetries < 0 || cfg.ramp < 0 || cfg.online <= 0 || cfg.editCount < 0 || cfg.editRetries < 0 || cfg.editInterval < 0 || cfg.connectRetries < 0 || cfg.resumeCount < 0 || cfg.resumeCount > cfg.clients || cfg.resumeDelay < 0 || cfg.resumeSettle < 0 {
		return cfg, errors.New("invalid load configuration")
	}
	maxUsername := fmt.Sprintf("%s-user-%d-%s", cfg.prefix, cfg.clients-1, "000000000000")
	if len(maxUsername) > 32 {
		return cfg, fmt.Errorf("prefix produces usernames longer than 32 characters; shorten prefix")
	}
	for _, endpoint := range splitCSV(*endpoints) {
		normalized, err := normalizeEndpoint(endpoint)
		if err != nil {
			return cfg, err
		}
		cfg.endpoints = append(cfg.endpoints, normalized)
	}
	if len(cfg.endpoints) == 0 {
		return cfg, errors.New("endpoints must contain at least one URL")
	}
	if cfg.origin == "" {
		return cfg, errors.New("origin must not be empty")
	}
	roots, err := os.ReadFile(cfg.caFile)
	if err != nil {
		return cfg, fmt.Errorf("read CA file: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(roots) {
		return cfg, errors.New("CA file contains no certificates")
	}
	cfg.tlsConfig = &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: cfg.serverName}
	cfg.httpClient = &http.Client{Transport: &http.Transport{TLSClientConfig: cfg.tlsConfig}}
	return cfg, nil
}

func normalizeEndpoint(value string) (string, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid owner redirect endpoint")
	}
	if parsed.Path == "" || parsed.Path == "/" {
		parsed.Path = "/api/webtransport"
	}
	if parsed.Path != "/api/webtransport" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("owner redirect endpoint has invalid path")
	}
	return parsed.String(), nil
}

func connectWithRetry(ctx context.Context, cfg config, token, docID string) (*session, int, int, error) {
	var lastRedirects int
	var lastErr error
	var retries int
	for attempt := 0; attempt <= cfg.connectRetries; attempt++ {
		current, redirects, err := connect(ctx, cfg, token, docID)
		lastRedirects = redirects
		if err == nil {
			return current, redirects, attempt, nil
		}
		lastErr = err
		if attempt == cfg.connectRetries {
			break
		}
		select {
		case <-ctx.Done():
			return nil, redirects, attempt, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 100 * time.Millisecond):
			retries++
		}
	}
	return nil, lastRedirects, retries, lastErr
}

func connect(ctx context.Context, cfg config, token, docID string) (*session, int, error) {
	return connectMode(ctx, cfg, token, docID, protocol.TypeJoin, 0, "")
}

func resume(ctx context.Context, cfg config, token, docID string, lastVersion uint64, content string) (*session, int, bool, error) {
	current, redirects, err := connectMode(ctx, cfg, token, docID, protocol.TypeResume, lastVersion, content)
	if err != nil {
		return nil, redirects, false, err
	}
	return current, redirects, current.resumeResync.Load(), nil
}

func resumeWithRetry(ctx context.Context, cfg config, token, docID string, lastVersion uint64, content string) (*session, int, bool, int, error) {
	var redirects int
	for attempt := 0; attempt <= cfg.connectRetries; attempt++ {
		current, count, resynced, err := resume(ctx, cfg, token, docID, lastVersion, content)
		redirects += count
		if err == nil {
			return current, redirects, resynced, attempt, nil
		}
		if attempt == cfg.connectRetries {
			return nil, redirects, false, attempt, err
		}
		select {
		case <-ctx.Done():
			return nil, redirects, false, attempt, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
		}
	}
	return nil, redirects, false, cfg.connectRetries, errors.New("resume retry loop exhausted")
}

func connectMode(ctx context.Context, cfg config, token, docID, messageType string, lastVersion uint64, content string) (*session, int, error) {
	visited := map[string]struct{}{}
	endpoint := cfg.endpoints[0]
	for redirects := 0; redirects <= len(cfg.endpoints)+1; redirects++ {
		if _, ok := visited[endpoint]; ok {
			return nil, redirects, errors.New("owner redirect loop")
		}
		visited[endpoint] = struct{}{}
		transport := &webtransportgo.Transport{TLSClientConfig: cfg.tlsConfig.Clone()}
		_, wtSession, err := transport.Dial(ctx, endpoint, http.Header{"Origin": []string{cfg.origin}})
		if err != nil {
			_ = transport.Close()
			for _, candidate := range cfg.endpoints {
				if _, seen := visited[candidate]; !seen {
					endpoint = candidate
					continue
				}
			}
			if len(visited) < len(cfg.endpoints) {
				continue
			}
			return nil, redirects, fmt.Errorf("webtransport dial %s: %w", endpoint, err)
		}
		stream, err := wtSession.OpenStreamSync(ctx)
		if err != nil {
			_ = wtSession.CloseWithError(0, "stream failed")
			_ = transport.Close()
			return nil, redirects, fmt.Errorf("open WebTransport stream: %w", err)
		}
		current := &session{transport: transport, session: wtSession, stream: stream, token: token, docID: docID, content: content, done: make(chan struct{})}
		if err := current.write(protocol.Message{Type: protocol.TypeAuth, Token: token}); err != nil {
			current.close()
			return nil, redirects, fmt.Errorf("AUTH write: %w", err)
		}
		message, err := current.read()
		if err != nil {
			current.close()
			return nil, redirects, fmt.Errorf("AUTH read: %w", err)
		}
		if message.Type != protocol.TypeAuthOK {
			current.close()
			return nil, redirects, fmt.Errorf("unexpected AUTH response %s", message.Type)
		}
		request := protocol.Message{Type: messageType, DocID: docID, LastVersion: lastVersion}
		if err := current.write(request); err != nil {
			current.close()
			return nil, redirects, fmt.Errorf("%s write: %w", strings.ToUpper(messageType), err)
		}
		message, err = current.read()
		if err != nil {
			current.close()
			return nil, redirects, fmt.Errorf("%s read: %w", strings.ToUpper(messageType), err)
		}
		if message.Type == protocol.TypeOwnerRedirect {
			current.close()
			if message.Endpoint == "" {
				return nil, redirects, errors.New("owner redirect without endpoint")
			}
			endpoint, err = normalizeEndpoint(message.Endpoint)
			if err != nil {
				return nil, redirects, err
			}
			continue
		}
		switch {
		case messageType == protocol.TypeJoin && message.Type == protocol.TypeJoined:
			current.content = message.Content
			current.version.Store(message.Version)
			return current, redirects, nil
		case messageType == protocol.TypeResume && (message.Type == protocol.TypeSync || message.Type == protocol.TypeResyncRequired):
			if err := applyRecoveryMessage(current, message, lastVersion); err != nil {
				current.close()
				return nil, redirects, err
			}
			return current, redirects, nil
		default:
			current.close()
			return nil, redirects, fmt.Errorf("unexpected %s response %s", strings.ToUpper(messageType), message.Type)
		}
	}
	return nil, len(cfg.endpoints) + 1, errors.New("too many owner redirects")
}

func applyRecoveryMessage(s *session, message protocol.Message, lastVersion uint64) error {
	if message.DocID != s.docID {
		return fmt.Errorf("recovery doc mismatch: got %s want %s", message.DocID, s.docID)
	}
	s.contentMu.Lock()
	defer s.contentMu.Unlock()
	content := s.content
	version := lastVersion
	if message.Type == protocol.TypeResyncRequired {
		if message.Snapshot == nil || message.Snapshot.Version > message.CurrentVersion {
			return errors.New("invalid RESYNC snapshot")
		}
		content = message.Snapshot.Content
		version = message.Snapshot.Version
		s.resumeResync.Store(true)
	}
	if message.Type == protocol.TypeSync && message.FromVersion != lastVersion {
		return fmt.Errorf("SYNC from_version %d does not match local version %d", message.FromVersion, lastVersion)
	}
	for _, operation := range message.Operations {
		if operation.Version != version+1 {
			return fmt.Errorf("recovery operation version %d is not contiguous after %d", operation.Version, version)
		}
		var err error
		content, err = collab.Apply(content, operation.Operation)
		if err != nil {
			return fmt.Errorf("apply recovery operation %d: %w", operation.Version, err)
		}
		version = operation.Version
	}
	if message.Type == protocol.TypeSync {
		if version != message.ToVersion {
			return fmt.Errorf("SYNC ended at version %d want %d", version, message.ToVersion)
		}
	} else if version != message.CurrentVersion {
		return fmt.Errorf("RESYNC ended at version %d want %d", version, message.CurrentVersion)
	}
	s.content = content
	s.version.Store(version)
	return nil
}

func (s *session) receive(disconnects, remoteUpdates *atomic.Int64, maxObservedVersion *atomic.Uint64, tracker *convergenceTracker) {
	defer close(s.done)
	for {
		message, err := s.read()
		if err != nil {
			if !s.closing.Load() {
				disconnects.Add(1)
				s.err.Store(err)
			}
			return
		}
		if message.Type == protocol.TypeError {
			if value, ok := s.acks.LoadAndDelete(message.RequestID); ok {
				ack := value.(*pendingAck)
				ack.err <- fmt.Errorf("%s: %s", message.Code, message.Message)
			} else {
				remoteUpdates.Add(1)
			}
			continue
		}
		if message.Type == protocol.TypeEditApplied {
			tracker.record(message)
			if message.Operation != nil {
				s.contentMu.Lock()
				content, applyErr := collab.Apply(s.content, *message.Operation)
				if applyErr != nil {
					s.contentMu.Unlock()
					s.err.Store(applyErr)
					return
				}
				s.content = content
				s.contentMu.Unlock()
			}
			s.version.Store(message.Version)
			for {
				observed := maxObservedVersion.Load()
				if message.Version <= observed || maxObservedVersion.CompareAndSwap(observed, message.Version) {
					break
				}
			}
			if value, ok := s.acks.LoadAndDelete(message.RequestID); ok {
				close(value.(*pendingAck).done)
			} else {
				remoteUpdates.Add(1)
			}
		}
	}
}

const maxMessageSize = 1 << 20

func (s *session) write(message protocol.Message) error {
	return writeMessage(s.stream, message)
}

func (s *session) read() (protocol.Message, error) {
	return readMessage(s.stream)
}

func writeMessage(writer io.Writer, message protocol.Message) error {
	payload, err := json.Marshal(message)
	if err != nil {
		return err
	}
	if len(payload) > maxMessageSize {
		return errors.New("message exceeds 1 MiB")
	}
	frame := make([]byte, 4+len(payload))
	binary.BigEndian.PutUint32(frame[:4], uint32(len(payload)))
	copy(frame[4:], payload)
	written := 0
	for written < len(frame) {
		count, err := writer.Write(frame[written:])
		written += count
		if err != nil {
			return err
		}
		if count == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}

func readMessage(reader io.Reader) (protocol.Message, error) {
	var header [4]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return protocol.Message{}, err
	}
	length := binary.BigEndian.Uint32(header[:])
	if length > maxMessageSize {
		return protocol.Message{}, errors.New("message exceeds 1 MiB")
	}
	payload := make([]byte, length)
	if _, err := io.ReadFull(reader, payload); err != nil {
		return protocol.Message{}, err
	}
	var message protocol.Message
	if err := json.Unmarshal(payload, &message); err != nil {
		return protocol.Message{}, err
	}
	return message, nil
}

func (s *session) close() {
	s.closing.Store(true)
	_ = s.stream.Close()
	_ = s.session.CloseWithError(0, "load complete")
	_ = s.transport.Close()
}

func closeSessions(sessions []*session) {
	for _, current := range sessions {
		if current != nil {
			current.close()
		}
	}
}

func registerAndLogin(ctx context.Context, client *http.Client, apiURL, username, password string) (string, error) {
	if err := jsonRequest(ctx, client, http.MethodPost, apiURL+"/api/auth/register", "", map[string]string{"username": username, "password": password}, nil); err != nil {
		return "", err
	}
	var response struct {
		AccessToken string `json:"access_token"`
	}
	if err := jsonRequest(ctx, client, http.MethodPost, apiURL+"/api/auth/login", "", map[string]string{"username": username, "password": password}, &response); err != nil {
		return "", err
	}
	if response.AccessToken == "" {
		return "", errors.New("login returned empty access token")
	}
	return response.AccessToken, nil
}

func createDocument(ctx context.Context, client *http.Client, apiURL, token, documentID string) error {
	return jsonRequest(ctx, client, http.MethodPost, apiURL+"/api/documents", token, map[string]string{"id": documentID, "content": ""}, nil)
}

func grantWrite(ctx context.Context, client *http.Client, apiURL, ownerToken, userToken, documentID string) error {
	var user struct {
		ID string `json:"id"`
	}
	if err := jsonRequest(ctx, client, http.MethodGet, apiURL+"/api/auth/me", userToken, nil, &user); err != nil {
		return err
	}
	return jsonRequest(ctx, client, http.MethodPut, apiURL+"/api/documents/"+documentID+"/permissions/"+user.ID, ownerToken, map[string]permission.Role{"role": permission.RoleWrite}, nil)
}

func jsonRequest(ctx context.Context, client *http.Client, method, endpoint, token string, body any, output any) error {
	var payload []byte
	var err error
	if body != nil {
		payload, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if client == nil {
		return errors.New("HTTP client is nil")
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("%s %s returned %s", method, endpoint, response.Status)
	}
	if output != nil {
		return json.NewDecoder(response.Body).Decode(output)
	}
	return nil
}

func emitReport(value report, runErr error) error {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(encoded))
	return runErr
}

func percentiles(values []time.Duration) (float64, float64) {
	if len(values) == 0 {
		return 0, 0
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	at := func(fraction float64) float64 {
		index := int(float64(len(values)-1) * fraction)
		return float64(values[index].Microseconds()) / 1000
	}
	return at(0.50), at(0.95)
}

func randomID() string {
	value := make([]byte, 6)
	if _, err := rand.Read(value); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value)
}

func splitCSV(value string) []string {
	var result []string
	for _, item := range bytes.Split([]byte(value), []byte(",")) {
		if trimmed := string(bytes.TrimSpace(item)); trimmed != "" {
			result = append(result, trimmed)
		}
	}
	return result
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func intEnv(name string, fallback int) int {
	if value := os.Getenv(name); value != "" {
		var parsed int
		if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
			return parsed
		}
	}
	return fallback
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	if value := os.Getenv(name); value != "" {
		if parsed, err := time.ParseDuration(value); err == nil {
			return parsed
		}
	}
	return fallback
}

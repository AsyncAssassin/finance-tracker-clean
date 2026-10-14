package rest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/AsyncAssassin/finance-tracker-clean/internal/gateway/ledgerclient"
)

// These tests run the import over a real HTTP server on the loopback
// interface, because the deadlines of the import live on the connection:
// a ResponseRecorder has none. Every test uses one unfinished connection
// at a time, short deadlines and small bodies, bounds every wait, and on
// cleanup requires the server to close in time with no ledger call left
// in flight.

const (
	testServerTimeout = 100 * time.Millisecond // the server-wide read and write timeouts, shorter than the import's on purpose
	importBody        = "category,amount\nеда,1\n"
)

// pairBarrier lets two imports meet inside the ledger. A participant
// leaves when the other arrives, when its context ends, when the test
// aborts the pair or when the bound passes; nothing keeps waiting in the
// background after that.
type pairBarrier struct {
	arrivals atomic.Int32
	both     chan struct{} // closed when the second participant arrives
	abort    chan struct{} // closed by the test when the pair cannot complete
}

func newPairBarrier() *pairBarrier {
	return &pairBarrier{both: make(chan struct{}), abort: make(chan struct{})}
}

func (b *pairBarrier) wait(ctx context.Context, bound time.Duration) error {
	if b.arrivals.Add(1) == 2 {
		close(b.both)
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	select {
	case <-b.both:
		return nil
	case <-b.abort:
		return errors.New("the other import was not admitted: an import slot is held")
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return errors.New("the other import did not arrive in time: an import slot is held")
	}
}

func TestPairBarrierAlwaysLeaves(t *testing.T) {
	t.Parallel()
	const quick = 200 * time.Millisecond
	timed := func(f func() error) (time.Duration, error) {
		start := time.Now()
		err := f()
		return time.Since(start), err
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if took, err := timed(func() error { return newPairBarrier().wait(canceled, time.Minute) }); !errors.Is(err, context.Canceled) || took > quick {
		t.Errorf("canceled context: %v after %s, want context.Canceled at once", err, took)
	}

	aborted := newPairBarrier()
	close(aborted.abort)
	if took, err := timed(func() error { return aborted.wait(context.Background(), time.Minute) }); err == nil || took > quick {
		t.Errorf("aborted pair: %v after %s, want an error at once", err, took)
	}

	if took, err := timed(func() error { return newPairBarrier().wait(context.Background(), 20*time.Millisecond) }); err == nil || took > quick {
		t.Errorf("nobody else: %v after %s, want an error at the bound", err, took)
	}

	pair := newPairBarrier()
	second := make(chan error, 1)
	go func() { second <- pair.wait(context.Background(), time.Second) }()
	if err := pair.wait(context.Background(), time.Second); err != nil {
		t.Errorf("first of two: %v", err)
	}
	if err := <-second; err != nil {
		t.Errorf("second of two: %v", err)
	}
}

// importProbe is the ledger of these tests. It counts the imports that
// reach it and, switched with atomics so that handlers and the test
// never race on it, can take time, wait for its context to end or meet
// a second import at a barrier. inFlight counts the calls that have not
// returned, so cleanup can tell that none is left waiting.
type importProbe struct {
	calls    atomic.Int32
	inFlight atomic.Int32
	canceled atomic.Int32
	slow     atomic.Int64 // nanoseconds every import takes
	blocking atomic.Bool  // wait for the context to end
	pair     atomic.Pointer[pairBarrier]
	entered  chan struct{} // one value per call that entered, for the tests that need the moment
}

func newImportProbe() *importProbe {
	return &importProbe{entered: make(chan struct{}, 16)}
}

func (p *importProbe) importRows(ctx context.Context, _ int64, rows []ledgerclient.ImportRow) ([]ledgerclient.ImportResult, error) {
	p.calls.Add(1)
	p.inFlight.Add(1)
	defer p.inFlight.Add(-1)
	select {
	case p.entered <- struct{}{}:
	default:
	}
	if d := p.slow.Load(); d > 0 {
		timer := time.NewTimer(time.Duration(d))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if p.blocking.Load() {
		// Longer than any wait of the tests, so that it cannot stand in
		// for the cancellation they expect.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			p.canceled.Add(1)
			return nil, ctx.Err()
		case <-timer.C:
			return nil, errors.New("the import was not canceled with the client")
		}
	}
	if b := p.pair.Load(); b != nil {
		if err := b.wait(ctx, 2*time.Second); err != nil {
			return nil, err
		}
	}
	out := make([]ledgerclient.ImportResult, len(rows))
	for i, r := range rows {
		out[i] = ledgerclient.ImportResult{Line: r.Line, ID: int64(i + 1)}
	}
	return out, nil
}

// importServer is a real server over the gateway's handler. finished gets
// a value when a request to the import route has left the handler chain,
// including the release of its admission slot.
type importServer struct {
	*httptest.Server
	probe    *importProbe
	finished chan struct{}
}

// startImportServer serves the gateway with the given import deadline
// and, when grace is set, that margin for writing an answer instead of
// the usual 20 seconds. Cleanup closes the server within a bound and
// requires that no ledger call is still in flight, on the successful
// and on the failing path alike.
func startImportServer(t *testing.T, importTimeout, grace time.Duration) *importServer {
	t.Helper()
	probe := newImportProbe()
	gw := newServer(Deps{
		Ledger:        &fakeLedger{importRows: probe.importRows},
		Auth:          &fakeAuth{},
		Tokens:        fakeTokens{testToken: testUser, "other-import-user": testUser + 1},
		Log:           slog.New(slog.DiscardHandler),
		ImportTimeout: importTimeout,
	})
	if grace > 0 {
		gw.responseGrace = grace
	}
	h := gw.handler()
	finished := make(chan struct{}, 64)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(w, r)
		if r.URL.Path == importPath {
			finished <- struct{}{}
		}
	}))
	// Like cmd/gateway, the server has its own timeouts; the import must
	// override them, so here they are shorter than the import's deadline.
	srv.Config.ReadHeaderTimeout = time.Second
	srv.Config.ReadTimeout = testServerTimeout
	srv.Config.WriteTimeout = testServerTimeout
	srv.Start()
	t.Cleanup(func() {
		closed := make(chan struct{})
		go func() { srv.Close(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Error("the server did not close in time: a handler or a connection is stuck")
			return
		}
		if n := probe.inFlight.Load(); n != 0 {
			t.Errorf("%d ledger calls are still in flight after the server closed", n)
		}
	})
	return &importServer{Server: srv, probe: probe, finished: finished}
}

// waitFinished waits for one import request to leave the handler chain.
func (s *importServer) waitFinished(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-s.finished:
	case <-time.After(within):
		t.Fatalf("the import handler did not finish within %s", within)
	}
}

// openImport sends the start of an import request with a Content-Length
// that promises more than sent and returns the connection, still open.
func (s *importServer) openImport(t *testing.T, partial string, promised int) net.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", s.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: gateway\r\nAuthorization: Bearer %s\r\nContent-Type: text/csv\r\nContent-Length: %d\r\n\r\n%s",
		importPath, testToken, promised, partial)
	if err != nil {
		t.Fatal(err)
	}
	return conn
}

// readAnswer reads one response from conn within the bound.
func readAnswer(conn net.Conn, within time.Duration) (*http.Response, string, error) {
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		return nil, "", err
	}
	res, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		return nil, "", err
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return res, "", fmt.Errorf("reading the body: %w", err)
	}
	return res, string(body), nil
}

// importResult is the outcome of one complete import sent with the HTTP
// client; it carries an error instead of failing the test, because the
// imports run in their own goroutines.
type importResult struct {
	status int
	body   string
	err    error
}

// importOnce sends a complete small import and waits for its answer
// within a bound.
func (s *importServer) importOnce(tokens ...string) importResult {
	token := testToken
	if len(tokens) > 0 {
		token = tokens[0]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.URL+importPath, strings.NewReader(importBody))
	if err != nil {
		return importResult{err: err}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "text/csv")
	res, err := s.Client().Do(req)
	if err != nil {
		return importResult{err: err}
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		return importResult{status: res.StatusCode, err: fmt.Errorf("reading the answer: %w", err)}
	}
	return importResult{status: res.StatusCode, body: string(body)}
}

// requireBothImportSlotsFree sends two imports at once and requires both
// to be admitted and to meet in the ledger. One free slot would let a
// single import through, so one success proves nothing; this fails as
// soon as one of the two slots is still held, and then lets the import
// waiting at the barrier go instead of waiting out its bound.
func (s *importServer) requireBothImportSlotsFree(t *testing.T) {
	t.Helper()
	barrier := newPairBarrier()
	s.probe.pair.Store(barrier)
	defer s.probe.pair.Store(nil)
	results := make(chan importResult, 2)
	for _, token := range []string{testToken, "other-import-user"} {
		go func() { results <- s.importOnce(token) }()
	}
	first := <-results
	if first.err != nil || first.status != http.StatusOK {
		close(barrier.abort)
	}
	second := <-results
	for i, r := range []importResult{first, second} {
		if r.err != nil {
			t.Fatalf("concurrent import %d: %v", i+1, r.err)
		}
		if r.status != http.StatusOK {
			t.Fatalf("concurrent import %d = %d %s, want 200 for both: an import slot was not released", i+1, r.status, r.body)
		}
	}
	for range 2 {
		s.waitFinished(t, time.Second)
	}
}

// A client that stops sending the body keeps its import slot only until
// the import's own read deadline, which is longer than the server-wide
// ReadTimeout; then the handler answers 408, returns and frees the slot,
// without ever calling the ledger.
func TestImportReadDeadlineReleasesTheSlot(t *testing.T) {
	t.Parallel()
	const importTimeout = 300 * time.Millisecond
	s := startImportServer(t, importTimeout, 0)
	start := time.Now()
	conn := s.openImport(t, "category,amount\n", 1000)
	s.waitFinished(t, 3*time.Second)
	if elapsed := time.Since(start); elapsed < importTimeout-50*time.Millisecond {
		t.Errorf("the handler finished after %s, before the import deadline %s: the server-wide ReadTimeout cut the body", elapsed, importTimeout)
	}
	res, body, err := readAnswer(conn, 2*time.Second)
	if err != nil {
		t.Fatalf("reading the answer after the deadline: %v", err)
	}
	if res.StatusCode != http.StatusRequestTimeout || !strings.Contains(body, codeTimeout) {
		t.Errorf("answer after the deadline = %d %s, want 408 %s", res.StatusCode, body, codeTimeout)
	}
	if s.probe.calls.Load() != 0 {
		t.Error("the ledger was called with an unfinished body")
	}
	s.requireBothImportSlotsFree(t)
}

// disconnectImportTimeout is the import deadline of the disconnect
// tests: far longer than the time they give the handler to finish, so a
// release by the deadline cannot pass for a release by the disconnect.
const disconnectImportTimeout = 10 * time.Second

// A client that goes away before the body is complete frees its slot at
// once: the handler returns on the connection error, long before the
// read deadline, without calling the ledger.
func TestImportClientDisconnectBeforeTheBodyReleasesTheSlot(t *testing.T) {
	t.Parallel()
	s := startImportServer(t, disconnectImportTimeout, 0)
	conn := s.openImport(t, "category,amount\n", 1000)
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	s.waitFinished(t, time.Second)
	if s.probe.calls.Load() != 0 {
		t.Error("the ledger was called with an unfinished body")
	}
	s.requireBothImportSlotsFree(t)
}

// A client that goes away during the ledger call cancels the call
// through the request context, and the handler returns at once.
func TestImportClientDisconnectDuringTheLedgerCallCancelsIt(t *testing.T) {
	t.Parallel()
	s := startImportServer(t, disconnectImportTimeout, 0)
	s.probe.blocking.Store(true)
	conn := s.openImport(t, importBody, len(importBody))
	select {
	case <-s.probe.entered:
	case <-time.After(time.Second):
		t.Fatal("the import did not reach the ledger")
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	s.waitFinished(t, time.Second)
	s.probe.blocking.Store(false)
	if s.probe.canceled.Load() != 1 {
		t.Errorf("ledger calls canceled with the client = %d, want 1", s.probe.canceled.Load())
	}
	s.requireBothImportSlotsFree(t)
}

// deadlineWriter records the deadlines a handler sets, standing in for
// the connection behind http.ResponseWriter.
type deadlineWriter struct {
	http.ResponseWriter
	read, write time.Time
}

func (w *deadlineWriter) SetReadDeadline(d time.Time) error  { w.read = d; return nil }
func (w *deadlineWriter) SetWriteDeadline(d time.Time) error { w.write = d; return nil }

// The import sets its deadlines through http.ResponseController, which
// reaches the connection through the Unwrap of the middleware's writer.
// A ResponseRecorder has no deadlines: the controller reports that and
// the import goes on without them, as in the other tests.
func TestImportDeadlinesReachTheConnection(t *testing.T) {
	t.Parallel()
	h := NewHandler(Deps{
		Ledger:        &fakeLedger{importRows: newImportProbe().importRows},
		Auth:          &fakeAuth{},
		Tokens:        fakeTokens{testToken: testUser},
		Log:           slog.New(slog.DiscardHandler),
		ImportTimeout: time.Minute,
	})
	req := httptest.NewRequest(http.MethodPost, importPath, strings.NewReader(importBody))
	req.Header.Set("Authorization", "Bearer "+testToken)
	req.Header.Set("Content-Type", "text/csv")
	w := &deadlineWriter{ResponseWriter: httptest.NewRecorder()}
	before := time.Now()
	h.ServeHTTP(w, req)
	if w.read.Before(before.Add(time.Minute)) || w.read.After(time.Now().Add(time.Minute)) {
		t.Errorf("read deadline = %s, want the import timeout after the start", w.read.Sub(before))
	}
	if w.write.Before(w.read.Add(20*time.Second)) || w.write.After(time.Now().Add(time.Minute+20*time.Second)) {
		t.Errorf("write deadline = %s after the start, want 20 s after the read deadline", w.write.Sub(before))
	}
	if rec := w.ResponseWriter.(*httptest.ResponseRecorder); rec.Code != http.StatusOK {
		t.Errorf("import = %d %s", rec.Code, rec.Body)
	}

	rc := http.NewResponseController(&statusWriter{ResponseWriter: httptest.NewRecorder()})
	if err := rc.SetReadDeadline(time.Now()); !errors.Is(err, http.ErrNotSupported) {
		t.Errorf("SetReadDeadline on a recorder = %v, want ErrNotSupported", err)
	}
}

// The body may take the whole import deadline to arrive, and the ledger
// call, which starts only then, has a deadline of its own; the answer
// comes after both. The write deadline must therefore cover the ledger
// call counted from its start, or a slow upload followed by a long
// import gets no answer at all.
func TestImportResponseDeadlineCoversTheLedgerCall(t *testing.T) {
	t.Parallel()
	const importTimeout, grace = 600 * time.Millisecond, 150 * time.Millisecond
	s := startImportServer(t, importTimeout, grace)
	s.probe.slow.Store(int64(450 * time.Millisecond))
	half := len(importBody) / 2
	conn := s.openImport(t, importBody[:half], len(importBody))
	time.Sleep(400 * time.Millisecond) // the pace of the slow upload, within the read deadline
	if _, err := io.WriteString(conn, importBody[half:]); err != nil {
		t.Fatal(err)
	}
	res, body, err := readAnswer(conn, 3*time.Second)
	if err != nil {
		t.Fatalf("no answer after a slow upload and a long import: %v", err)
	}
	if res.StatusCode != http.StatusOK || !strings.Contains(body, `"created":1`) {
		t.Errorf("answer = %d %s, want 200 with the import result", res.StatusCode, body)
	}
	s.waitFinished(t, time.Second)
}

func TestOneUserCannotOccupyBothImportSlots(t *testing.T) {
	s := startImportServer(t, 2*time.Second, 0)
	first := s.openImport(t, "category,amount\n", 100)
	defer first.Close()
	// Wait for the read to begin by sending another import until it is rejected.
	deadline := time.Now().Add(time.Second)
	for {
		result := s.importOnce()
		if result.status == 503 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("same user's parallel import was not rejected: %+v", result)
		}
	}
	other := s.importOnce("other-import-user")
	if other.err != nil || other.status != 200 {
		t.Fatalf("independent user: %+v", other)
	}
}

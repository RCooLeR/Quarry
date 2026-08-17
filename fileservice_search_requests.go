package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
)

var (
	ErrInvalidSearchRequestID         = errors.New("invalid search request id")
	ErrSearchRequestNotFound          = errors.New("search request is not active")
	ErrSearchRequestAlreadyStarted    = errors.New("search request was already started")
	ErrSearchRequestSequenceExhausted = errors.New("search request sequence is exhausted")
)

const searchRequestIDPrefix = "search"

type interactiveSearchRequest struct {
	id         string
	fileID     string
	ctx        context.Context
	cancel     context.CancelFunc
	finish     func()
	running    bool
	canceled   bool
	finishOnce sync.Once
}

func (request *interactiveSearchRequest) finalize() {
	request.finishOnce.Do(func() {
		request.cancel()
		request.finish()
	})
}

func validateRPCSearchRequestID(requestID string) error {
	return validateGeneratedRPCID(requestID, searchRequestIDPrefix, ErrInvalidSearchRequestID)
}

// BeginSearchRequest reserves one bounded foreground search owner and returns
// its server-generated cancellation identity. The reservation expires under
// the same hard timeout as an executing search, so an abandoned bridge call
// cannot leak capacity. Call exactly one *Request method or CancelSearch.
func (s *FileService) BeginSearchRequest(fileID string) (string, error) {
	if err := validateRPCFileID(fileID); err != nil {
		return "", err
	}
	parent, cancel := context.WithTimeout(context.Background(), searchTimeout)
	ctx, finish, err := s.beginForegroundRun(parent, fileID)
	if err != nil {
		cancel()
		return "", err
	}
	// Prove that the syntactically valid ID still names a registered file
	// while the foreground lifecycle barrier is already in place. Registering
	// first prevents close/refresh from completing between validation and
	// reservation; the short-lived lease is released before the two-RPC
	// reservation is published and is never retained across bridge calls.
	lease, _, err := s.acquireReadFileContext(ctx, fileID)
	if err != nil {
		cancel()
		finish()
		return "", err
	}
	lease.Release()
	if err := ctx.Err(); err != nil {
		cancel()
		finish()
		return "", err
	}

	s.searchRequestMu.Lock()
	// Lifecycle cancellation and request-map publication synchronize on this
	// mutex. A close/refresh barrier may cancel ctx after the lease preflight
	// above; rechecking while holding the publication lock prevents an already
	// canceled reservation from appearing after the lifecycle cleanup pass.
	if err := ctx.Err(); err != nil {
		s.searchRequestMu.Unlock()
		cancel()
		finish()
		return "", err
	}
	if s.searchRequests == nil {
		s.searchRequests = make(map[string]*interactiveSearchRequest)
	}
	if s.searchRequestSeq >= math.MaxInt64 {
		s.searchRequestMu.Unlock()
		cancel()
		finish()
		return "", ErrSearchRequestSequenceExhausted
	}
	s.searchRequestSeq++
	requestID := fmt.Sprintf("%s%d", searchRequestIDPrefix, s.searchRequestSeq)
	request := &interactiveSearchRequest{
		id:     requestID,
		fileID: fileID,
		ctx:    ctx,
		cancel: cancel,
		finish: finish,
	}
	s.searchRequests[requestID] = request
	s.searchRequestMu.Unlock()

	// A reservation is bounded by the foreground caps, so one lightweight
	// watcher per active interactive search is also bounded. Running work owns
	// its completion; an unclaimed canceled/expired reservation is finalized
	// here so lifecycle shutdown can drain it.
	go func() {
		<-ctx.Done()
		s.finishUnclaimedSearchRequest(request)
	}()
	return requestID, nil
}

func (s *FileService) claimSearchRequest(requestID string) (*interactiveSearchRequest, error) {
	if err := validateRPCSearchRequestID(requestID); err != nil {
		return nil, err
	}
	s.searchRequestMu.Lock()
	request := s.searchRequests[requestID]
	if request == nil {
		s.searchRequestMu.Unlock()
		return nil, ErrSearchRequestNotFound
	}
	if request.running {
		s.searchRequestMu.Unlock()
		return nil, ErrSearchRequestAlreadyStarted
	}
	if err := request.ctx.Err(); err != nil {
		delete(s.searchRequests, requestID)
		s.searchRequestMu.Unlock()
		request.finalize()
		return nil, err
	}
	request.running = true
	s.searchRequestMu.Unlock()
	return request, nil
}

func (s *FileService) completeSearchRequest(request *interactiveSearchRequest) {
	s.searchRequestMu.Lock()
	owned := s.searchRequests[request.id] == request
	if owned {
		delete(s.searchRequests, request.id)
	}
	s.searchRequestMu.Unlock()
	request.finalize()
}

func (s *FileService) finishUnclaimedSearchRequest(request *interactiveSearchRequest) {
	s.searchRequestMu.Lock()
	owned := s.searchRequests[request.id] == request
	if owned {
		request.canceled = true
	}
	finish := owned && !request.running
	if finish {
		delete(s.searchRequests, request.id)
	}
	s.searchRequestMu.Unlock()
	if finish {
		request.finalize()
	}
}

// CancelSearch cancels only the exact active server request. false means the
// request is already complete/canceled or never existed; IDs are never reused,
// so repeated or delayed cancellation cannot affect a newer search.
func (s *FileService) CancelSearch(requestID string) (bool, error) {
	if err := validateRPCSearchRequestID(requestID); err != nil {
		return false, err
	}
	s.searchRequestMu.Lock()
	request := s.searchRequests[requestID]
	if request == nil {
		s.searchRequestMu.Unlock()
		return false, nil
	}
	if request.canceled {
		s.searchRequestMu.Unlock()
		return false, nil
	}
	request.canceled = true
	running := request.running
	if !running {
		delete(s.searchRequests, requestID)
	}
	s.searchRequestMu.Unlock()
	if running {
		request.cancel()
	} else {
		request.finalize()
	}
	return true, nil
}

// stopSearchRequests removes every public cancellation identity at the
// irreversible service-stop boundary. Unclaimed reservations finalize here;
// executing requests retain their foreground completion signal until their
// worker returns, but can no longer be addressed or survive in the RPC map.
func (s *FileService) stopSearchRequests() {
	type stoppedRequest struct {
		request *interactiveSearchRequest
		running bool
	}
	s.searchRequestMu.Lock()
	requests := make([]stoppedRequest, 0, len(s.searchRequests))
	for requestID, request := range s.searchRequests {
		delete(s.searchRequests, requestID)
		request.canceled = true
		requests = append(requests, stoppedRequest{request: request, running: request.running})
	}
	s.searchRequestMu.Unlock()
	for _, stopped := range requests {
		if stopped.running {
			stopped.request.cancel()
		} else {
			stopped.request.finalize()
		}
	}
}

// stopSearchRequestsForFile removes every public cancellation identity owned
// by one lifecycle-blocked source. Unclaimed reservations finalize
// synchronously; running requests remain responsible for their worker cleanup
// but are canceled and cannot be addressed after the lifecycle boundary.
// BeginSearchRequest publishes under the same mutex and rechecks its context,
// so a canceled reservation cannot appear after this pass.
func (s *FileService) stopSearchRequestsForFile(fileID string) {
	type stoppedRequest struct {
		request *interactiveSearchRequest
		running bool
	}
	s.searchRequestMu.Lock()
	requests := make([]stoppedRequest, 0)
	for requestID, request := range s.searchRequests {
		if request.fileID != fileID {
			continue
		}
		delete(s.searchRequests, requestID)
		request.canceled = true
		requests = append(requests, stoppedRequest{request: request, running: request.running})
	}
	s.searchRequestMu.Unlock()
	for _, stopped := range requests {
		if stopped.running {
			stopped.request.cancel()
		} else {
			stopped.request.finalize()
		}
	}
}

func (s *FileService) FindNextRequest(requestID, query string, fromByte int64, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	return s.findRequest(requestID, query, fromByte, false, regex, caseSensitive, wholeWord)
}

func (s *FileService) FindPrevRequest(requestID, query string, beforeByte int64, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	return s.findRequest(requestID, query, beforeByte, true, regex, caseSensitive, wholeWord)
}

func (s *FileService) findRequest(requestID, query string, start int64, backward, regex, caseSensitive, wholeWord bool) (SearchHit, error) {
	request, err := s.claimSearchRequest(requestID)
	if err != nil {
		return SearchHit{}, err
	}
	defer s.completeSearchRequest(request)
	if err := validateServiceSearchQuery(query, regex); err != nil {
		return SearchHit{}, err
	}
	result, err := s.findRegisteredContext(request.ctx, request.fileID, query, start, backward, regex, caseSensitive, wholeWord)
	if errors.Is(request.ctx.Err(), context.DeadlineExceeded) {
		return SearchHit{TimedOut: true}, nil
	}
	if ctxErr := request.ctx.Err(); ctxErr != nil {
		return SearchHit{}, ctxErr
	}
	return result, err
}

func (s *FileService) SearchAllRequest(requestID, query string, regex, caseSensitive, wholeWord bool, maxHits int) (SearchAllResult, error) {
	return s.searchAllRequest(requestID, query, regex, caseSensitive, wholeWord, maxHits, 0, false)
}

// SearchAllPageRequest preserves bounded continuation paging without exposing
// an execution path that bypasses exact request ownership.
func (s *FileService) SearchAllPageRequest(requestID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (SearchAllResult, error) {
	return s.searchAllRequest(requestID, query, regex, caseSensitive, wholeWord, maxHits, startOffset, backward)
}

func (s *FileService) searchAllRequest(requestID, query string, regex, caseSensitive, wholeWord bool, maxHits int, startOffset int64, backward bool) (SearchAllResult, error) {
	request, err := s.claimSearchRequest(requestID)
	if err != nil {
		return SearchAllResult{}, err
	}
	defer s.completeSearchRequest(request)
	if err := validateServiceSearchQuery(query, regex); err != nil {
		return SearchAllResult{}, err
	}
	result, err := s.searchAllPageRegisteredContext(request.ctx, request.fileID, query, regex, caseSensitive, wholeWord, maxHits, startOffset, backward)
	if errors.Is(request.ctx.Err(), context.DeadlineExceeded) {
		return SearchAllResult{TimedOut: true, Limit: result.Limit}, nil
	}
	if ctxErr := request.ctx.Err(); ctxErr != nil {
		return SearchAllResult{}, ctxErr
	}
	return result, err
}

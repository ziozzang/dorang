package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/ziozzang/dorang/internal/batch"
	"github.com/ziozzang/dorang/internal/capacity"
	"github.com/ziozzang/dorang/internal/config"
	"github.com/ziozzang/dorang/internal/router"
	"github.com/ziozzang/dorang/internal/server"
	"github.com/ziozzang/dorang/internal/wire/openai"
	"github.com/ziozzang/dorang/pkg/catalog"
)

// startBatch builds the batch scheduler and its adapters (DESIGN §11.1).
//
// Every dependency internal/batch declares is an interface it defined itself,
// and the adapters here are the only place the real implementations meet them.
// The Store is the durable one: batches, their rows and their result bodies live
// in the database (DESIGN §9.2), so [batch.Service.Recover] below resumes work a
// previous process left in flight instead of resuming nothing. A batch that had
// finished four hundred of its rows costs four hundred rows less on the way back.
func (a *App) startBatch(cfg *config.Config, up *upstreamTable) error {
	dir := filepath.Join(filepath.Dir(config.ExpandPath(cfg.Storage.SQLite.Path)), "blobs")
	blobs, err := batch.NewDiskBlobs(dir)
	if err != nil {
		return fmt.Errorf("app: batch blob store: %w", err)
	}
	a.targets = &batchResolver{}
	a.targets.swap(buildTargets(cfg, a.Catalog))

	svc, err := batch.New(batch.Config{
		Store:    &batchStore{st: a.Store},
		Blobs:    blobs,
		Executor: &batchExecutor{d: a.dispatch},
		Capacity: &batchReserver{broker: a.Broker, table: up},
		Models:   a.targets,
		Logf:     a.logf,
	})
	if err != nil {
		return fmt.Errorf("app: batch service: %w", err)
	}
	a.Batch = svc

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if n, err := svc.Recover(ctx); err != nil {
		a.logf("app: batch recovery: %v", err)
	} else if n > 0 {
		a.logf("app: resumed %d batch(es) left running by a previous process", n)
	}
	return nil
}

// batchResolver satisfies batch.ModelResolver.
//
// It resolves against a snapshot built from configuration rather than asking the
// router, because router.Route ACQUIRES capacity as part of answering and the
// batch scheduler needs the target before it reserves. Model names are compared
// whole; nothing here splits one (DESIGN §2.1).
type batchResolver struct {
	targets atomic.Pointer[map[string]batch.Target]
}

func (r *batchResolver) swap(m map[string]batch.Target) { r.targets.Store(&m) }

// ResolveModel implements batch.ModelResolver.
func (r *batchResolver) ResolveModel(name string) (batch.Target, bool) {
	p := r.targets.Load()
	if p == nil {
		return batch.Target{}, false
	}
	t, ok := (*p)[name]
	return t, ok
}

// buildTargets maps every client-facing name — group and alias alike — onto the
// first deployment behind it.
//
// "First" is a real simplification: the batch surface reserves against ONE
// target and does not walk a ranked candidate list the way the interactive path
// does, so a model group with several deployments sends all of its batch traffic
// to the first one. It is correct (the capacity reservation and the dispatch
// agree about the target, which is what ExecRequest exists to guarantee) and it
// is visibly less than routing.
func buildTargets(cfg *config.Config, cat *catalog.Catalog) map[string]batch.Target {
	out := make(map[string]batch.Target, len(cfg.Models)+len(cfg.Aliases))
	for i := range cfg.Models {
		m := &cfg.Models[i]
		if len(m.Deployments) == 0 {
			continue
		}
		d := &m.Deployments[0]
		t := batch.Target{Provider: d.Provider, UpstreamModel: d.UpstreamModel}
		if p, ok := cfg.Provider(d.Provider); ok {
			t.ProviderGroup = p.CapacityGroup
		}
		out[m.Name] = t
	}
	for alias, target := range cfg.Aliases {
		if t, ok := out[target]; ok {
			out[alias] = t
		}
	}
	return out
}

// batchReserver satisfies batch.Reserver with the real capacity broker.
//
// The Batch flag is passed through untouched: it is what subjects a row to the
// interactive reserve on every axis (§11.1), and softening it here would let
// batch work starve interactive traffic on exactly the axis nobody was watching.
type batchReserver struct {
	broker *capacity.Broker
	table  *upstreamTable
}

// Acquire implements batch.Reserver.
//
// The returned *capacity.Reservation satisfies batch.Reservation directly: the
// broker already exposes CredentialID, and the batch interface asks for it by
// the same name. Nothing is wrapped, so the credential the scheduler dispatches
// with is read from the reservation itself rather than re-derived beside it.
func (r *batchReserver) Acquire(ctx context.Context, req batch.CapacityRequest) (batch.Reservation, error) {
	res, err := r.broker.Acquire(ctx, capacity.Request{
		Provider:      req.Provider,
		Model:         req.UpstreamModel,
		ProviderGroup: req.ProviderGroup,
		PrincipalID:   req.PrincipalID,
		Candidates:    r.table.candidates(req.Provider),
		OnCapacity:    capacity.Spill,
		Batch:         req.Batch,
	})
	if err != nil {
		return nil, err
	}
	return res, nil
}

// candidates lists a provider's credentials as capacity candidates, in a stable
// order.
//
// The sort is not cosmetic. The credentials are held in a map, so without it the
// candidate slice is in Go's randomized map order and the broker walks a
// different list on every call — which means the account a batch row lands on
// varies run to run for no reason the operator can see. That was invisible while
// the executor ignored the broker's choice and always used the same credential;
// now that the reservation decides, an unordered list would make the decision
// itself nondeterministic.
func (t *upstreamTable) candidates(provider string) []capacity.Candidate {
	var out []capacity.Candidate
	for _, c := range t.creds {
		if c.provider != provider {
			continue
		}
		out = append(out, capacity.Candidate{
			ID:            c.id,
			CapacityGroup: c.capacityGroup,
			MaxConcurrent: c.maxConcurrent,
			Provider:      c.provider,
		})
	}
	slices.SortFunc(out, func(a, b capacity.Candidate) int {
		return strings.Compare(a.ID, b.ID)
	})
	return out
}

// anyCredential is the fallback credential for a provider, used only when a
// reservation named none.
//
// It is reachable in one case: a provider configured with no capacity
// candidates at all, where the broker has nothing to choose between and returns
// "". Every other row authenticates with the credential its reservation was
// taken against (batch.ExecRequest.Credential), because reserving against one
// account and dispatching to another makes both accounts' concurrency counts
// wrong.
func (t *upstreamTable) anyCredential(provider string) string {
	best := ""
	for _, c := range t.creds {
		if c.provider != provider {
			continue
		}
		if best == "" || c.id < best {
			best = c.id
		}
	}
	return best
}

// batchExecutor satisfies batch.Executor by running one row through the same
// conversion and upstream call the interactive path uses.
//
// The contract it must honour: a returned error means no HTTP answer was
// produced at all, and a non-2xx answer is a result, not an error, because the
// caller's error file has to carry the upstream's own envelope.
type batchExecutor struct{ d *dispatcher }

// Execute implements batch.Executor.
func (e *batchExecutor) Execute(ctx context.Context, req *batch.ExecRequest) (*batch.ExecResult, error) {
	st := e.d.state()
	if st == nil {
		return nil, errors.New("app: the gateway is not configured")
	}
	up, ok := st.upstreams.provider(req.Provider)
	if !ok {
		return nil, fmt.Errorf("app: no upstream configured for provider %q", req.Provider)
	}
	creq, err := openai.DecodeRequest(req.Body)
	if err != nil {
		// A row body that does not decode produced no HTTP answer, so it is an
		// error rather than a result. Validation already rejected the shape;
		// reaching here means the row is worse than the validator can see.
		return nil, err
	}
	c := &call{
		kind:      callChat,
		clientAPI: catalog.APIOpenAIChat,
		model:     req.Model,
		body:      req.Body,
		creq:      creq,
	}
	// The credential the scheduler reserved against, not one chosen here. The
	// reservation holds slots on THAT account's axes (DESIGN §5.1), so sending
	// the row anywhere else charges one account and burdens another.
	cred := req.Credential
	if cred == "" {
		cred = st.upstreams.anyCredential(req.Provider)
	}
	dec := &router.Decision{
		Provider:      req.Provider,
		UpstreamModel: req.UpstreamModel,
		Credential:    cred,
		Kind:          up.kind,
	}

	payload, endpoint, err := e.d.encodeUpstream(c, dec, up)
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	hreq.Header.Set("Accept-Encoding", "identity")
	applyCredential(up.api, st.upstreams.secret(dec.Credential), hreq.Header)

	resp, err := e.d.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return &batch.ExecResult{StatusCode: resp.StatusCode, Body: body}, nil
	}
	out, _, err := e.d.convertResponse(c, dec, up, body)
	if err != nil {
		return nil, err
	}
	return &batch.ExecResult{StatusCode: resp.StatusCode, Body: out}, nil
}

// batchRoutes mounts the batch and files surface on the HTTP server.
//
// internal/server registers the T0 route set and answers 501 for everything
// else, including /v1/batches and /v1/files, which it lists as declared but
// unbuilt. These routes replace that 501 with the real service.
func (a *App) batchRoutes() []server.Route {
	if a.Batch == nil {
		return nil
	}
	// One Route per PATTERN, never one per operation: internal/server keys its
	// exact table by pattern and refuses a duplicate outright, because two
	// handlers on one path would make one of them unreachable. The method split
	// therefore happens inside the handler.
	byMethod := func(m map[string]server.Handler) server.Handler {
		return func(w http.ResponseWriter, rq *server.Request) error {
			if h, ok := m[rq.Method]; ok {
				return h(w, rq)
			}
			return server.NewError(http.StatusMethodNotAllowed, server.TypeInvalidRequest,
				"method not allowed on this route").WithCode("method_not_allowed")
		}
	}
	return []server.Route{
		{
			Pattern: "/v1/batches", Methods: server.MethodGET | server.MethodPOST,
			Name: "batches", NeedsBody: true,
			Handler: byMethod(map[string]server.Handler{
				http.MethodPost: a.handleBatchCreate,
				http.MethodGet:  a.handleBatchList,
			}),
		},
		{
			Pattern: "/v1/batches/{id}", Methods: server.MethodGET,
			Name: "batches_retrieve", Handler: a.handleBatchRetrieve,
		},
		{
			Pattern: "/v1/batches/{id}/cancel", Methods: server.MethodPOST,
			Name: "batches_cancel", Handler: a.handleBatchCancel,
		},
		{
			// The upload declares no body: an input file may be 200 MiB and the
			// content is streamed into blob storage rather than buffered whole.
			Pattern: "/v1/files", Methods: server.MethodGET | server.MethodPOST,
			Name: "files",
			Handler: byMethod(map[string]server.Handler{
				http.MethodPost: a.handleFileUpload,
				http.MethodGet:  a.handleFileList,
			}),
		},
		{
			Pattern: "/v1/files/{id}", Methods: server.MethodGET | server.MethodDELETE,
			Name: "files_object",
			Handler: byMethod(map[string]server.Handler{
				http.MethodGet:    a.handleFileRetrieve,
				http.MethodDelete: a.handleFileDelete,
			}),
		},
		{
			Pattern: "/v1/files/{id}/content", Methods: server.MethodGET,
			Name: "files_content", Handler: a.handleFileContent,
		},
	}
}

func (a *App) handleBatchCreate(w http.ResponseWriter, rq *server.Request) error {
	var body struct {
		InputFileID      string            `json:"input_file_id"`
		Endpoint         string            `json:"endpoint"`
		CompletionWindow string            `json:"completion_window"`
		Metadata         map[string]string `json:"metadata"`
	}
	if err := json.Unmarshal(rq.Body.Bytes(), &body); err != nil {
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
			err.Error()).WithCode("invalid_body")
	}
	out, err := a.Batch.Create(rq.Context(), batch.CreateRequest{
		InputFileID:      body.InputFileID,
		Endpoint:         body.Endpoint,
		CompletionWindow: body.CompletionWindow,
		Metadata:         body.Metadata,
		OwnerKeyID:       principalID(rq),
	})
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *App) handleBatchList(w http.ResponseWriter, rq *server.Request) error {
	q := rq.HTTP.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := a.Batch.List(rq.Context(), batch.BatchQuery{
		OwnerKeyID: principalID(rq),
		After:      q.Get("after"),
		Limit:      limit,
	})
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *App) handleBatchRetrieve(w http.ResponseWriter, rq *server.Request) error {
	out, err := a.Batch.Retrieve(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *App) handleBatchCancel(w http.ResponseWriter, rq *server.Request) error {
	out, err := a.Batch.Cancel(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

// handleFileUpload reads the multipart body OpenAI's files endpoint takes.
//
// The route declares NeedsBody false so the content is streamed into blob
// storage rather than buffered whole: an input file may be 200 MiB, and the
// point of a file endpoint is that it does not have to fit in a request buffer.
func (a *App) handleFileUpload(w http.ResponseWriter, rq *server.Request) error {
	mr, err := rq.HTTP.MultipartReader()
	if err != nil {
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
			"expected a multipart/form-data body").WithCode("invalid_body")
	}
	purpose := ""
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
				err.Error()).WithCode("invalid_body")
		}
		switch part.FormName() {
		case "purpose":
			b, _ := io.ReadAll(io.LimitReader(part, 128))
			purpose = string(bytes.TrimSpace(b))
		case "file":
			out, err := a.Batch.UploadFile(rq.Context(), batch.UploadRequest{
				Filename:   part.FileName(),
				Purpose:    purpose,
				OwnerKeyID: principalID(rq),
				Content:    part,
			})
			part.Close()
			if err != nil {
				return batchError(err)
			}
			return writeJSON(w, http.StatusOK, out)
		}
		part.Close()
	}
	return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest,
		"the multipart body carried no file part; send purpose before file").
		WithCode("missing_file")
}

func (a *App) handleFileList(w http.ResponseWriter, rq *server.Request) error {
	q := rq.HTTP.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	out, err := a.Batch.ListFiles(rq.Context(), batch.FileQuery{
		OwnerKeyID: principalID(rq),
		Purpose:    q.Get("purpose"),
		After:      q.Get("after"),
		Limit:      limit,
	})
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *App) handleFileRetrieve(w http.ResponseWriter, rq *server.Request) error {
	out, err := a.Batch.RetrieveFile(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

func (a *App) handleFileContent(w http.ResponseWriter, rq *server.Request) error {
	rc, rec, err := a.Batch.FileContent(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return batchError(err)
	}
	defer rc.Close()
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(rec.Bytes, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
	return nil
}

func (a *App) handleFileDelete(w http.ResponseWriter, rq *server.Request) error {
	out, err := a.Batch.DeleteFile(rq.Context(), rq.Param("id"), principalID(rq))
	if err != nil {
		return batchError(err)
	}
	return writeJSON(w, http.StatusOK, out)
}

// batchError maps the package's sentinels onto statuses, exactly as its own
// documentation prescribes.
func batchError(err error) error {
	var ve *batch.ValidationErrors
	if errors.As(err, &ve) {
		e := server.NewError(http.StatusBadRequest, server.TypeInvalidRequest, ve.Error())
		if list := ve.List(); len(list) > 0 && list[0].Code != "" {
			e = e.WithCode(list[0].Code)
		}
		return e
	}
	switch {
	case errors.Is(err, batch.ErrNotFound):
		return server.NewError(http.StatusNotFound, server.TypeInvalidRequest, err.Error()).
			WithCode("not_found")
	case errors.Is(err, batch.ErrInvalidRequest):
		return server.NewError(http.StatusBadRequest, server.TypeInvalidRequest, err.Error()).
			WithCode("invalid_request")
	case errors.Is(err, batch.ErrInvalidState):
		return server.NewError(http.StatusConflict, server.TypeInvalidRequest, err.Error()).
			WithCode("invalid_state")
	case errors.Is(err, batch.ErrClosed):
		return server.NewError(http.StatusServiceUnavailable, server.TypeAPIError, err.Error()).
			WithCode("draining")
	}
	return err
}

func writeJSON(w http.ResponseWriter, status int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Content-Length", strconv.Itoa(len(b)))
	w.WriteHeader(status)
	_, _ = w.Write(b)
	return nil
}

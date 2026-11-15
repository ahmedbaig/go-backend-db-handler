package grpcapi

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/api"
	"github.com/ahmedbaig/go-backend-http"
	dbv1 "github.com/ahmedbaig/go-backend-proto/gen/go/ahmedbaig/dbhandler/v1"
)

type Server struct {
	dbv1.UnimplementedDBHandlerServiceServer
	d api.Deps
}

func New(d api.Deps) *Server { return &Server{d: d} }

func decode(field string, raw []byte, into any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(into); err != nil {
		return httpx.E(httpx.CodeInvalidJSON).WithFields(httpx.FieldError{Field: field, Rule: "json", Message: err.Error()}).Wrap(err)
	}
	return nil
}

func encode(v any) ([]byte, error) {
	return json.Marshal(v)
}

func toQuery(r *dbv1.QueryRequest) (db.Query, error) {
	q := db.Query{Database: r.GetDatabase(), Instance: r.GetInstance(), Service: r.GetService(), Method: db.Method(r.GetMethod())}
	return q, decode("params", r.GetParamsJson(), &q.Params)
}

func (s *Server) Query(ctx context.Context, r *dbv1.QueryRequest) (*dbv1.QueryResponse, error) {
	q, err := toQuery(r)
	if err != nil {
		return nil, err
	}
	if err := httpx.Validate(q); err != nil {
		return nil, err
	}
	res, err := s.d.Engine.Execute(ctx, q)
	if err != nil {
		return nil, err
	}
	return &dbv1.QueryResponse{DataJson: res.Data, Count: res.Count}, nil
}

func (s *Server) Transaction(ctx context.Context, r *dbv1.TransactionRequest) (*dbv1.TransactionResponse, error) {
	tx := db.Transaction{Database: r.GetDatabase(), Instance: r.GetInstance()}
	for _, qr := range r.GetQueries() {
		q, err := toQuery(qr)
		if err != nil {
			return nil, err
		}
		tx.Queries = append(tx.Queries, q)
	}
	if err := httpx.Validate(tx); err != nil {
		return nil, err
	}
	res, err := s.d.Engine.Transaction(ctx, tx)
	if err != nil {
		return nil, err
	}
	out := &dbv1.TransactionResponse{}
	for _, x := range res.Results {
		out.Results = append(out.Results, &dbv1.QueryResponse{DataJson: x.Data, Count: x.Count})
	}
	return out, nil
}

func notConfigured(what string) error {
	return httpx.E(httpx.CodeNotFound).WithMessage(what + " stores are not configured")
}

func (s *Server) KeyValue(ctx context.Context, r *dbv1.KeyValueRequest) (*dbv1.KeyValueResponse, error) {
	if s.d.KV == nil {
		return nil, notConfigured("Redis")
	}
	var req db.KVRequest
	if err := decode("request", r.GetRequestJson(), &req); err != nil {
		return nil, err
	}
	if err := httpx.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.d.KV.Execute(ctx, r.GetStore(), db.KVOp(r.GetOp()), req)
	if err != nil {
		return nil, err
	}
	b, err := encode(res)
	return &dbv1.KeyValueResponse{ResultJson: b}, err
}

func (s *Server) Documents(ctx context.Context, r *dbv1.DocumentsRequest) (*dbv1.DocumentsResponse, error) {
	if s.d.Docs == nil {
		return nil, notConfigured("Mongo")
	}
	var req db.DocsRequest
	if err := decode("request", r.GetRequestJson(), &req); err != nil {
		return nil, err
	}
	if err := httpx.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.d.Docs.Execute(ctx, r.GetStore(), r.GetCollection(), db.DocsOp(r.GetOp()), req)
	if err != nil {
		return nil, err
	}
	b, err := encode(res)
	return &dbv1.DocumentsResponse{ResultJson: b}, err
}

func (s *Server) Track(ctx context.Context, r *dbv1.TrackRequest) (*dbv1.TrackResponse, error) {
	if s.d.Docs == nil {
		return nil, notConfigured("Mongo")
	}
	var req db.TrackRequest
	if err := decode("events", r.GetEventsJson(), &req.Events); err != nil {
		return nil, err
	}
	if err := httpx.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.d.Docs.Track(ctx, r.GetStore(), req.Events, httpx.RequestID(ctx), time.Now())
	if err != nil {
		return nil, err
	}
	b, err := encode(res)
	return &dbv1.TrackResponse{ResultJson: b}, err
}

func (s *Server) DailyActiveUsers(ctx context.Context, r *dbv1.DailyActiveUsersRequest) (*dbv1.DailyActiveUsersResponse, error) {
	if s.d.Docs == nil {
		return nil, notConfigured("Mongo")
	}
	req := db.DAURequest{From: r.GetFrom(), To: r.GetTo()}
	if err := httpx.Validate(req); err != nil {
		return nil, err
	}
	res, err := s.d.Docs.DAU(ctx, r.GetStore(), req.From, req.To)
	if err != nil {
		return nil, err
	}
	b, err := encode(res)
	return &dbv1.DailyActiveUsersResponse{PointsJson: b}, err
}

func (s *Server) Capabilities(context.Context, *dbv1.CapabilitiesRequest) (*dbv1.CapabilitiesResponse, error) {
	b, err := encode(api.Capabilities(s.d))
	return &dbv1.CapabilitiesResponse{CapabilitiesJson: b}, err
}

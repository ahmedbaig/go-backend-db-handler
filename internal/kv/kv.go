package kv

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ahmedbaig/go-backend-db"
	"github.com/ahmedbaig/go-backend-db-handler/internal/appconfig"
	"github.com/ahmedbaig/go-backend-http"
	"github.com/ahmedbaig/go-backend-monitoring"
	"github.com/redis/go-redis/v9"
)

const (
	maxKeyLen   = 512
	maxKeys     = 1000
	maxValueLen = 1 << 20
)

type store struct {
	cfg appconfig.RedisStore
	rdb *redis.Client
}

type Pool struct {
	stores map[string]*store
}

func Open(ctx context.Context, cfgs []appconfig.RedisStore, log *slog.Logger) (*Pool, error) {
	p := &Pool{stores: map[string]*store{}}
	for _, c := range cfgs {
		var opts *redis.Options
		if c.URL != "" {
			o, err := redis.ParseURL(c.URL)
			if err != nil {
				return nil, fmt.Errorf("redis store %s: %w", c.Name, err)
			}
			opts = o
		} else {
			opts = &redis.Options{Addr: c.Addr, Username: c.Username, Password: c.Password, DB: c.DB}
			if c.TLS {
				opts.TLSConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}
		}
		if c.PoolSize > 0 {
			opts.PoolSize = c.PoolSize
		}
		if t := c.Timeout.Std(); t > 0 {
			opts.ReadTimeout, opts.WriteTimeout, opts.DialTimeout = t, t, t
		}
		rdb := redis.NewClient(opts)
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		if err := rdb.Ping(pctx).Err(); err != nil {
			log.Warn("redis not reachable at startup", slog.String("store", c.Name), slog.Any("error", err))
		}
		cancel()
		p.stores[c.Name] = &store{cfg: c, rdb: rdb}
		log.Info("redis store ready", slog.String("store", c.Name))
	}
	return p, nil
}

func (p *Pool) Stores() []db.StoreInfo {
	out := make([]db.StoreInfo, 0, len(p.stores))
	for name, s := range p.stores {
		out = append(out, db.StoreInfo{Name: name, Kind: "redis", Prefix: s.cfg.Prefix})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (p *Pool) RegisterProbes(pr *monitoring.Prober) error {
	var errs []error
	for name, s := range p.stores {
		rdb := s.rdb
		errs = append(errs, pr.AddProbe("redis:"+name, func(ctx context.Context) error {
			return rdb.Ping(ctx).Err()
		}, monitoring.Interval(15*time.Second), monitoring.Timeout(5*time.Second)))
	}
	return errors.Join(errs...)
}

func (p *Pool) Close() error {
	var errs []error
	for _, s := range p.stores {
		errs = append(errs, s.rdb.Close())
	}
	return errors.Join(errs...)
}

func fieldErr(field, rule, msg string) error {
	return httpx.E(httpx.CodeValidation).WithFields(httpx.FieldError{Field: field, Rule: rule, Message: msg})
}

func (s *store) key(k string) (string, error) {
	if k == "" {
		return "", fieldErr("key", "required", "is required")
	}
	if len(k) > maxKeyLen {
		return "", fieldErr("key", "max", fmt.Sprintf("must be at most %d bytes", maxKeyLen))
	}
	return s.cfg.Prefix + k, nil
}

func (s *store) keys(ks []string) ([]string, error) {
	if len(ks) == 0 {
		return nil, fieldErr("keys", "required", "is required")
	}
	if len(ks) > maxKeys {
		return nil, fieldErr("keys", "max", fmt.Sprintf("may list at most %d keys", maxKeys))
	}
	out := make([]string, len(ks))
	for i, k := range ks {
		full, err := s.key(k)
		if err != nil {
			return nil, err
		}
		out[i] = full
	}
	return out, nil
}

func ttl(ms int64) (time.Duration, error) {
	if ms < 0 {
		return 0, fieldErr("ttlMs", "min", "must be 0 or more")
	}
	return time.Duration(ms) * time.Millisecond, nil
}

func value(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fieldErr("value", "required", "is required")
	}
	if len(raw) > maxValueLen {
		return nil, fieldErr("value", "max", "must be at most 1 MiB")
	}
	if !json.Valid(raw) {
		return nil, fieldErr("value", "json", "must be valid JSON")
	}
	return raw, nil
}

func asJSON(s string) json.RawMessage {
	if json.Valid([]byte(s)) {
		return json.RawMessage(s)
	}
	b, _ := json.Marshal(s)
	return b
}

func (p *Pool) Execute(ctx context.Context, name string, op db.KVOp, req db.KVRequest) (db.KVResult, error) {
	s, ok := p.stores[name]
	if !ok {
		return db.KVResult{}, httpx.E(httpx.CodeNotFound).WithMessage("Unknown redis store " + name)
	}
	r := s.rdb
	switch op {
	case db.KVGet:
		k, err := s.key(req.Key)
		if err != nil {
			return db.KVResult{}, err
		}
		var v string
		if req.Touch {
			d, terr := ttl(req.TTLms)
			if terr != nil {
				return db.KVResult{}, terr
			}
			if d == 0 {
				return db.KVResult{}, fieldErr("ttlMs", "required", "is required with touch")
			}
			v, err = r.GetEx(ctx, k, d).Result()
		} else {
			v, err = r.Get(ctx, k).Result()
		}
		if errors.Is(err, redis.Nil) {
			return db.KVResult{Found: false}, nil
		}
		if err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{Found: true, Value: asJSON(v)}, nil

	case db.KVMGet:
		ks, err := s.keys(req.Keys)
		if err != nil {
			return db.KVResult{}, err
		}
		vals, err := r.MGet(ctx, ks...).Result()
		if err != nil {
			return db.KVResult{}, err
		}
		out := make([]json.RawMessage, len(vals))
		var found int64
		for i, v := range vals {
			if sv, ok := v.(string); ok {
				out[i] = asJSON(sv)
				found++
			} else {
				out[i] = json.RawMessage("null")
			}
		}
		return db.KVResult{Found: found > 0, Values: out, Count: found}, nil

	case db.KVSet:
		k, err := s.key(req.Key)
		if err != nil {
			return db.KVResult{}, err
		}
		v, err := value(req.Value)
		if err != nil {
			return db.KVResult{}, err
		}
		d, err := ttl(req.TTLms)
		if err != nil {
			return db.KVResult{}, err
		}
		if req.NX && req.XX {
			return db.KVResult{}, fieldErr("nx", "exclusive", "nx and xx cannot both be set")
		}
		args := redis.SetArgs{TTL: d}
		if req.NX {
			args.Mode = "NX"
		}
		if req.XX {
			args.Mode = "XX"
		}
		res, err := r.SetArgs(ctx, k, v, args).Result()
		if errors.Is(err, redis.Nil) {
			return db.KVResult{OK: false}, nil
		}
		if err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{OK: res == "OK"}, nil

	case db.KVDel:
		keys := req.Keys
		if req.Key != "" {
			keys = append(keys, req.Key)
		}
		ks, err := s.keys(keys)
		if err != nil {
			return db.KVResult{}, err
		}
		n, err := r.Del(ctx, ks...).Result()
		if err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{Count: n, OK: n > 0}, nil

	case db.KVExpire:
		k, err := s.key(req.Key)
		if err != nil {
			return db.KVResult{}, err
		}
		d, err := ttl(req.TTLms)
		if err != nil || d == 0 {
			return db.KVResult{}, fieldErr("ttlMs", "min", "must be more than 0")
		}
		ok, err := r.PExpire(ctx, k, d).Result()
		if err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{OK: ok, Found: ok}, nil

	case db.KVTTL:
		k, err := s.key(req.Key)
		if err != nil {
			return db.KVResult{}, err
		}
		d, err := r.PTTL(ctx, k).Result()
		if err != nil {
			return db.KVResult{}, err
		}
		switch {
		case d == -2*time.Nanosecond || d == -2*time.Millisecond:
			return db.KVResult{Found: false}, nil
		case d < 0:
			return db.KVResult{Found: true, TTLms: -1}, nil
		}
		return db.KVResult{Found: true, TTLms: d.Milliseconds()}, nil

	case db.KVIncr:
		k, err := s.key(req.Key)
		if err != nil {
			return db.KVResult{}, err
		}
		by := req.By
		if by == 0 {
			by = 1
		}
		d, err := ttl(req.TTLms)
		if err != nil {
			return db.KVResult{}, err
		}
		pipe := r.TxPipeline()
		incr := pipe.IncrBy(ctx, k, by)
		if d > 0 {
			pipe.PExpire(ctx, k, d)
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{Count: incr.Val(), OK: true}, nil

	case db.KVPublish:
		if req.Channel == "" {
			return db.KVResult{}, fieldErr("channel", "required", "is required")
		}
		v, err := value(req.Value)
		if err != nil {
			return db.KVResult{}, err
		}
		n, err := r.Publish(ctx, s.cfg.Prefix+req.Channel, v).Result()
		if err != nil {
			return db.KVResult{}, err
		}
		return db.KVResult{Count: n, OK: true}, nil
	}
	return db.KVResult{}, fieldErr("op", "oneof", "is not a supported operation")
}
